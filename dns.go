package client

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// DNSCache is a thread-safe, TTL-based DNS resolution cache with async refresh,
// in-flight deduplication, and round-robin IP selection.
type DNSCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]*dnsEntry
	// in-flight dedup: host → *dnsInflight
	inflightMu sync.Mutex
	inflight   map[string]*dnsInflight
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// dnsInflight represents an in-progress lookup.
type dnsInflight struct {
	done  chan struct{}
	addrs []string
	err   error
}

type dnsEntry struct {
	addrs   []string
	expires time.Time
	// idx is used for round-robin selection, accessed atomically.
	idx uint32
}

// NewDNSCache creates a new DNS cache with the given TTL and starts background
// prefetch and janitor goroutines.
func NewDNSCache(ttl time.Duration) *DNSCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c := &DNSCache{
		ttl:      ttl,
		entries:  make(map[string]*dnsEntry),
		inflight: make(map[string]*dnsInflight),
		stopCh:   make(chan struct{}),
	}
	go c.janitor()
	return c
}

// LookupHost resolves a hostname to an IP address using the cache.
// Multiple concurrent misses for the same host share one lookup (singleflight).
// Returns a single IP selected via round-robin when multiple IPs are cached.
func (c *DNSCache) LookupHost(host string) ([]string, error) {
	if net.ParseIP(host) != nil {
		return []string{host}, nil
	}

	c.mu.RLock()
	entry, ok := c.entries[host]
	now := time.Now()
	if ok && now.Before(entry.expires) {
		addrs := entry.addrs
		idx := atomic.AddUint32(&entry.idx, 1) - 1
		ip := addrs[idx%uint32(len(addrs))]
		c.mu.RUnlock()
		// Trigger background prefetch when 80% of TTL has elapsed.
		age := c.ttl - entry.expires.Sub(now)
		if age > c.ttl*4/5 {
			go c.backgroundRefresh(host)
		}
		return []string{ip}, nil
	}
	var stale []string
	if ok {
		stale = entry.addrs
	}
	c.mu.RUnlock()

	// In-flight dedup: only one goroutine does the lookup; others wait.
	c.inflightMu.Lock()
	if fl, exists := c.inflight[host]; exists {
		c.inflightMu.Unlock()
		<-fl.done
		if fl.err != nil {
			if stale != nil {
				return stale, nil
			}
			return nil, fl.err
		}
		return fl.addrs[:1], nil
	}
	fl := &dnsInflight{done: make(chan struct{})}
	c.inflight[host] = fl
	c.inflightMu.Unlock()

	addrs, err := net.LookupHost(host)

	fl.addrs = addrs
	fl.err = err
	close(fl.done)

	c.inflightMu.Lock()
	delete(c.inflight, host)
	c.inflightMu.Unlock()

	if err != nil {
		if stale != nil {
			return stale, nil
		}
		return nil, err
	}

	c.mu.Lock()
	c.entries[host] = &dnsEntry{
		addrs:   addrs,
		expires: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return []string{addrs[0]}, nil
}

// backgroundRefresh performs a non-blocking DNS refresh for host.
// It will no-op if a lookup is already in flight for the host.
func (c *DNSCache) backgroundRefresh(host string) {
	c.inflightMu.Lock()
	if _, exists := c.inflight[host]; exists {
		c.inflightMu.Unlock()
		return
	}
	fl := &dnsInflight{done: make(chan struct{})}
	c.inflight[host] = fl
	c.inflightMu.Unlock()

	addrs, err := net.LookupHost(host)

	fl.addrs = addrs
	fl.err = err
	close(fl.done)

	c.inflightMu.Lock()
	delete(c.inflight, host)
	c.inflightMu.Unlock()

	if err != nil || len(addrs) == 0 {
		return
	}

	c.mu.Lock()
	// Preserve round-robin index if entry already exists.
	var idx uint32
	if old, ok := c.entries[host]; ok {
		idx = atomic.LoadUint32(&old.idx)
	}
	entry := &dnsEntry{
		addrs:   addrs,
		expires: time.Now().Add(c.ttl),
	}
	atomic.StoreUint32(&entry.idx, idx)
	c.entries[host] = entry
	c.mu.Unlock()
}

// Refresh forces a fresh DNS lookup for the given host, updating the cache.
func (c *DNSCache) Refresh(host string) ([]string, error) {
	addrs, err := net.LookupHost(host)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[host] = &dnsEntry{
		addrs:   addrs,
		expires: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
	return addrs, nil
}

// Invalidate removes a host from the cache.
func (c *DNSCache) Invalidate(host string) {
	c.mu.Lock()
	delete(c.entries, host)
	c.mu.Unlock()
}

// Clear removes all cached entries.
func (c *DNSCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]*dnsEntry)
	c.mu.Unlock()
}

// Stop shuts down the background goroutines.
func (c *DNSCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// janitor periodically evicts expired entries and triggers prefetch for
// entries approaching TTL expiry (80% threshold).
func (c *DNSCache) janitor() {
	// Check at 1/4 TTL intervals to catch prefetch window accurately.
	interval := c.ttl / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			prefetchThreshold := c.ttl / 5 // refresh when less than 20% TTL remains

			c.mu.RLock()
			var expired []string
			var prefetch []string
			for host, entry := range c.entries {
				if now.After(entry.expires) {
					expired = append(expired, host)
				} else if entry.expires.Sub(now) < prefetchThreshold {
					prefetch = append(prefetch, host)
				}
			}
			c.mu.RUnlock()

			if len(expired) > 0 {
				c.mu.Lock()
				for _, host := range expired {
					delete(c.entries, host)
				}
				c.mu.Unlock()
			}

			for _, host := range prefetch {
				go c.backgroundRefresh(host)
			}
		}
	}
}
