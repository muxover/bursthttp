package client

import (
	"net"
	"sync"
	"time"
)

// DNSCache is a thread-safe, TTL-based DNS resolution cache.
// It caches the results of net.LookupHost to avoid repeated DNS lookups
// on the hot path. Entries are evicted after TTL expires.
type DNSCache struct {
	ttl      time.Duration
	mu       sync.RWMutex
	entries  map[string]*dnsEntry
	stopCh   chan struct{}
	stopOnce sync.Once
}

type dnsEntry struct {
	addrs   []string
	expires time.Time
	idx     uint32
}

// NewDNSCache creates a new DNS cache with the given TTL.
func NewDNSCache(ttl time.Duration) *DNSCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c := &DNSCache{
		ttl:     ttl,
		entries: make(map[string]*dnsEntry),
		stopCh:  make(chan struct{}),
	}
	go c.janitor()
	return c
}

// LookupHost resolves a hostname to IP addresses, using the cache when possible.
// Falls back to net.LookupHost on cache miss or expiry.
func (c *DNSCache) LookupHost(host string) ([]string, error) {
	if net.ParseIP(host) != nil {
		return []string{host}, nil
	}

	c.mu.RLock()
	entry, ok := c.entries[host]
	if ok && time.Now().Before(entry.expires) {
		addrs := entry.addrs
		c.mu.RUnlock()
		return addrs, nil
	}
	c.mu.RUnlock()

	addrs, err := net.LookupHost(host)
	if err != nil {
		if ok {
			return entry.addrs, nil
		}
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

// Stop shuts down the background janitor.
func (c *DNSCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// janitor periodically evicts expired entries to prevent unbounded growth.
func (c *DNSCache) janitor() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			for host, entry := range c.entries {
				if now.After(entry.expires) {
					delete(c.entries, host)
				}
			}
			c.mu.Unlock()
		}
	}
}
