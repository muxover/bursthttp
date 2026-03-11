package client

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Pool manages per-host connection pools. Host pool lookup and connection list
// access are lock-free (sync.Map + atomic.Pointer for the connection slice).
type Pool struct {
	hostPools  sync.Map // map[string]*HostPool
	config     *Config
	dialer     *Dialer
	tlsConfig  *TLSConfig
	compressor *Compressor
	nextConnID atomic.Int32
	stopCh     chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
}

// HostPool manages connections for a single scheme+host+port key.
// connections is updated with atomic.Pointer for lock-free get path.
type HostPool struct {
	connections atomic.Pointer[[]*Connection]
	index       atomic.Uint32

	maxIdle     int
	maxActive   int
	activeCount atomic.Int32
	idleCount   atomic.Int32

	pool      *Pool
	host      string     // pool key e.g. "https://api.example.com:443"
	dialHost  string     // bare hostname for dialing
	dialPort  int        // port number for dialing
	tlsConfig *TLSConfig // per-pool TLS config (nil for plain HTTP)
}

// NewPool creates a new connection pool.
func NewPool(config *Config, dialer *Dialer, tlsConfig *TLSConfig, compressor *Compressor) *Pool {
	p := &Pool{
		config:     config,
		dialer:     dialer,
		tlsConfig:  tlsConfig,
		compressor: compressor,
		stopCh:     make(chan struct{}),
	}
	if config.IdleCheckInterval > 0 && config.IdleTimeout > 0 {
		p.wg.Add(1)
		go p.idleEvictor()
	}
	return p
}

// Stop closes every connection in every host pool.
func (p *Pool) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	p.wg.Wait()

	var keys []interface{}
	p.hostPools.Range(func(key, value interface{}) bool {
		keys = append(keys, key)
		hp := value.(*HostPool)
		conns := hp.connections.Swap(nil)
		if conns != nil {
			for _, c := range *conns {
				c.Stop()
			}
		}
		return true
	})
	for _, k := range keys {
		p.hostPools.Delete(k)
	}
}

// GracefulStop waits for in-flight requests to complete (up to timeout) before
// closing all connections. Returns true if all requests drained cleanly.
func (p *Pool) GracefulStop(timeout time.Duration) bool {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	p.wg.Wait()

	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			p.Stop()
			return false
		}
		active := p.activeRequests()
		if active == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var keys []interface{}
	p.hostPools.Range(func(key, value interface{}) bool {
		keys = append(keys, key)
		hp := value.(*HostPool)
		conns := hp.connections.Swap(nil)
		if conns != nil {
			for _, c := range *conns {
				c.Stop()
			}
		}
		return true
	})
	for _, k := range keys {
		p.hostPools.Delete(k)
	}
	return true
}

func (p *Pool) activeRequests() int {
	total := 0
	p.hostPools.Range(func(_, value interface{}) bool {
		hp := value.(*HostPool)
		conns := hp.connections.Load()
		if conns != nil {
			for _, c := range *conns {
				total += int(c.activeReqs.Load())
			}
		}
		return true
	})
	return total
}

// WarmUp pre-establishes connections for the primary host.
// n is the number of connections to create (capped at PoolSize).
func (p *Pool) WarmUp(key string, useTLS bool, n int) int {
	if n <= 0 {
		return 0
	}
	hp := p.getHostPool(key, useTLS)
	created := 0
	for i := 0; i < n; i++ {
		conn := hp.createConnection()
		if conn == nil {
			break
		}
		created++
	}
	return created
}

// idleEvictor periodically scans host pools and removes connections that have
// been idle longer than IdleTimeout. Runs until the pool is stopped.
func (p *Pool) idleEvictor() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.config.IdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.evictIdleConnections()
		}
	}
}

func (p *Pool) evictIdleConnections() {
	now := time.Now().UnixNano()
	threshold := p.config.IdleTimeout.Nanoseconds()

	p.hostPools.Range(func(_, value interface{}) bool {
		hp := value.(*HostPool)
		conns := hp.connections.Load()
		if conns == nil {
			return true
		}
		var idle []*Connection
		for _, c := range *conns {
			if c.activeReqs.Load() == 0 && c.IsHealthy() {
				lastUsed := c.lastUsed.Load()
				if now-lastUsed > threshold {
					idle = append(idle, c)
				}
			}
		}
		if len(idle) > 0 {
			hp.removeUnhealthyConnections(idle)
		}
		return true
	})
}

// GetConnection returns an available connection for the given pool key.
// key is "scheme://host:port" (e.g. "https://api.example.com:443").
// useTLS controls whether new connections use TLS.
func (p *Pool) GetConnection(key string, useTLS bool) *Connection {
	if key == "" {
		scheme := "http"
		if p.config.UseTLS {
			scheme = "https"
		}
		key = fmt.Sprintf("%s://%s:%d", scheme, p.config.Host, p.config.Port)
		useTLS = p.config.UseTLS
	}
	hp := p.getHostPool(key, useTLS)

	const maxAttempts = 20
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 1. Prefer an idle connection with available pipeline capacity.
		if conn := hp.getIdleConnection(); conn != nil {
			return conn
		}
		// 2. Try to create a new connection (eagerly connects; returns nil if pool full).
		// Time the call: if it returns quickly the pool was full (CAS raced); if it
		// took significant time a real connection attempt was made and failed —
		// in that case stop retrying immediately to avoid multiplying timeouts
		// (e.g. a 30s proxy CONNECT timeout × 20 attempts = 600s hang).
		dialStart := time.Now()
		if conn := hp.createConnection(); conn != nil {
			return conn
		}
		if time.Since(dialStart) > time.Millisecond {
			// A real dial was attempted and failed — don't pile on.
			return nil
		}
		// 3. Fall back to any healthy connection, ignoring pipeline capacity.
		if conn := hp.getAnyConnection(); conn != nil {
			return conn
		}
		// 4. All connections are mid-dial or pool is saturated — wait briefly.
		if attempt < maxAttempts-1 {
			time.Sleep(500 * time.Microsecond)
		}
	}
	return nil
}

// getHostPool returns (or creates) the HostPool for the given key.
// key is "scheme://host:port". useTLS determines TLS for newly created pools.
func (p *Pool) getHostPool(key string, useTLS bool) *HostPool {
	if v, ok := p.hostPools.Load(key); ok {
		return v.(*HostPool)
	}

	maxIdle := p.config.MaxIdleConnsPerHost
	if maxIdle <= 0 {
		maxIdle = p.config.PoolSize
		if maxIdle <= 0 {
			maxIdle = 512
		}
	}
	maxActive := p.config.PoolSize
	if maxActive <= 0 {
		maxActive = 512
	}

	hostPort := key
	if idx := strings.Index(hostPort, "://"); idx >= 0 {
		hostPort = hostPort[idx+3:]
	}
	dialHost, dialPort := parseHostPort(hostPort, p.config.Host, p.config.Port)

	var tlsCfg *TLSConfig
	if useTLS {
		if p.tlsConfig != nil && p.config.Host == dialHost {
			tlsCfg = p.tlsConfig
		} else {
			hostConfig := p.config.BuildTLSConfig().Clone()
			if hostConfig.ServerName == "" || hostConfig.ServerName != dialHost {
				hostConfig.ServerName = dialHost
			}
			hostConfig.ClientSessionCache = tls.NewLRUClientSessionCache(256)
			hostConfig.SessionTicketsDisabled = false
			hsTimeout := p.config.TLSHandshakeTimeout
			if hsTimeout <= 0 {
				hsTimeout = 10 * time.Second
			}
			tlsCfg = &TLSConfig{
				config:           hostConfig,
				handshakeTimeout: hsTimeout,
				enableLogging:    p.config.EnableLogging,
			}
		}
	}

	emptyConns := []*Connection{}
	hp := &HostPool{
		maxIdle:     maxIdle,
		maxActive:   maxActive,
		pool:        p,
		host:        key,
		dialHost:    dialHost,
		dialPort:    dialPort,
		tlsConfig:   tlsCfg,
	}
	hp.connections.Store(&emptyConns)

	if v, loaded := p.hostPools.LoadOrStore(key, hp); loaded {
		return v.(*HostPool)
	}
	return hp
}

// parseHostPort splits "host:port" into parts.
func parseHostPort(key, defaultHost string, defaultPort int) (string, int) {
	if key == "" {
		return defaultHost, defaultPort
	}
	// IPv6: "[::1]:8080"
	if strings.HasPrefix(key, "[") {
		end := strings.LastIndex(key, "]")
		if end < 0 {
			return key, defaultPort
		}
		host := key[1:end]
		rest := key[end+1:]
		if strings.HasPrefix(rest, ":") {
			if port, err := strconv.Atoi(rest[1:]); err == nil && port > 0 {
				return host, port
			}
		}
		return host, defaultPort
	}
	idx := strings.LastIndex(key, ":")
	if idx < 0 {
		return key, defaultPort
	}
	host := key[:idx]
	port, err := strconv.Atoi(key[idx+1:])
	if err != nil || port <= 0 {
		return host, defaultPort
	}
	return host, port
}

// removeUnhealthyConnections stops and removes a batch of unhealthy connections.
// Uses CAS to replace the connection slice lock-free.
func (hp *HostPool) removeUnhealthyConnections(unhealthy []*Connection) {
	if len(unhealthy) == 0 {
		return
	}
	useMap := len(unhealthy) > 4
	var unhealthyMap map[*Connection]bool
	if useMap {
		unhealthyMap = make(map[*Connection]bool, len(unhealthy))
		for _, c := range unhealthy {
			unhealthyMap[c] = true
		}
	}

	for {
		old := hp.connections.Load()
		if old == nil || len(*old) == 0 {
			break
		}
		var newConns []*Connection
		removed := 0
		for _, c := range *old {
			drop := false
			if useMap {
				drop = unhealthyMap[c]
			} else {
				for _, u := range unhealthy {
					if c == u {
						drop = true
						break
					}
				}
			}
			if drop {
				removed++
			} else {
				newConns = append(newConns, c)
			}
		}
		if removed == 0 {
			break
		}
		newPtr := &newConns
		if hp.connections.CompareAndSwap(old, newPtr) {
			for _, c := range unhealthy {
				c.Stop()
			}
			hp.activeCount.Add(-int32(removed))
			hp.idleCount.Add(-int32(removed))
			break
		}
	}
}

// getIdleConnection returns a healthy, pipeline-ready connection.
func (hp *HostPool) getIdleConnection() *Connection {
	for {
		conns := hp.connections.Load()
		if conns == nil {
			return nil
		}
		connCount := uint32(len(*conns))
		if connCount == 0 {
			return nil
		}

		startIndex := hp.index.Load()
		firstIndex := startIndex % connCount
		firstConn := (*conns)[firstIndex]

		if firstConn.IsHealthy() && firstConn.CanAcceptRequest() {
			hp.index.Store((firstIndex + 1) % connCount)
			return firstConn
		}

		var unhealthy []*Connection
		if !firstConn.IsHealthy() {
			unhealthy = append(unhealthy, firstConn)
		}

		maxScan := connCount
		if maxScan > 8 {
			maxScan = 8
		}
		var found *Connection
		for i := uint32(1); i < maxScan; i++ {
			idx := (startIndex + i) % connCount
			c := (*conns)[idx]
			if c.IsHealthy() && c.CanAcceptRequest() {
				hp.index.Store((idx + 1) % connCount)
				found = c
				break
			} else if !c.IsHealthy() {
				unhealthy = append(unhealthy, c)
			}
		}

		if len(unhealthy) > 0 {
			hp.removeUnhealthyConnections(unhealthy)
			if !firstConn.IsHealthy() && found == nil {
				continue
			}
		}
		if found != nil {
			return found
		}

		if maxScan < connCount {
			unhealthy = unhealthy[:0]
			for i := maxScan; i < connCount; i++ {
				idx := (startIndex + i) % connCount
				c := (*conns)[idx]
				if c.IsHealthy() && c.CanAcceptRequest() {
					hp.index.Store((idx + 1) % connCount)
					if len(unhealthy) > 0 {
						hp.removeUnhealthyConnections(unhealthy)
					}
					return c
				} else if !c.IsHealthy() {
					unhealthy = append(unhealthy, c)
				}
			}
			if len(unhealthy) > 0 {
				hp.removeUnhealthyConnections(unhealthy)
			}
		}
		return nil
	}
}

// getAnyConnection returns any healthy connection (ignores pipeline capacity).
func (hp *HostPool) getAnyConnection() *Connection {
	for {
		conns := hp.connections.Load()
		if conns == nil {
			return nil
		}
		connCount := uint32(len(*conns))
		if connCount == 0 {
			return nil
		}

		startIndex := hp.index.Load()
		firstIndex := startIndex % connCount
		firstConn := (*conns)[firstIndex]

		if firstConn.IsHealthy() {
			hp.index.Store((firstIndex + 1) % connCount)
			return firstConn
		}

		var unhealthy []*Connection
		unhealthy = append(unhealthy, firstConn)

		maxScan := connCount
		if maxScan > 8 {
			maxScan = 8
		}
		var found *Connection
		for i := uint32(1); i < maxScan; i++ {
			idx := (startIndex + i) % connCount
			c := (*conns)[idx]
			if c.IsHealthy() {
				hp.index.Store((idx + 1) % connCount)
				found = c
				break
			} else {
				unhealthy = append(unhealthy, c)
			}
		}

		if len(unhealthy) > 0 {
			hp.removeUnhealthyConnections(unhealthy)
			if found == nil {
				continue
			}
		}
		return found
	}
}

// createConnection allocates a new Connection, establishes the TCP connection
// eagerly (no lock held during dial), and adds it to the pool.
//
// A CAS loop atomically reserves a pool slot before dialing, so the pool
// never exceeds maxIdle connections and never evicts in-use connections.
// Returns nil when the pool is at capacity or when the server is unreachable.
func (hp *HostPool) createConnection() *Connection {
	limit := int32(hp.maxIdle)
	for {
		cur := hp.activeCount.Load()
		if cur >= limit {
			return nil
		}
		if hp.activeCount.CompareAndSwap(cur, cur+1) {
			break
		}
	}

	connID := int(hp.pool.nextConnID.Add(1))
	conn := NewConnection(connID, hp.dialHost, hp.dialPort, hp.pool.config, hp.pool.dialer, hp.tlsConfig, hp.pool.compressor)

	if !conn.ensureConnection() {
		hp.activeCount.Add(-1)
		return nil
	}

	for {
		old := hp.connections.Load()
		if old == nil {
			hp.activeCount.Add(-1)
			return nil
		}
		newSlice := make([]*Connection, len(*old)+1)
		copy(newSlice, *old)
		newSlice[len(*old)] = conn
		newPtr := &newSlice
		if hp.connections.CompareAndSwap(old, newPtr) {
			hp.idleCount.Add(1)
			return conn
		}
	}
}

// GetHealthyConnections returns the total healthy connection count across all pools.
func (p *Pool) GetHealthyConnections() int {
	count := 0
	p.hostPools.Range(func(_, value interface{}) bool {
		hp := value.(*HostPool)
		conns := hp.connections.Load()
		if conns != nil {
			for _, c := range *conns {
				if c.IsHealthy() {
					count++
				}
			}
		}
		return true
	})
	return count
}
