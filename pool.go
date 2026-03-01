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

// Pool manages per-host connection pools.
type Pool struct {
	hostPools  map[string]*HostPool
	mu         sync.RWMutex
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
type HostPool struct {
	connections []*Connection
	index       atomic.Uint32
	mu          sync.RWMutex

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
		hostPools:  make(map[string]*HostPool),
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

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, hp := range p.hostPools {
		hp.mu.Lock()
		for _, c := range hp.connections {
			c.Stop()
		}
		hp.mu.Unlock()
	}
	p.hostPools = make(map[string]*HostPool)
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

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, hp := range p.hostPools {
		hp.mu.Lock()
		for _, c := range hp.connections {
			c.Stop()
		}
		hp.mu.Unlock()
	}
	p.hostPools = make(map[string]*HostPool)
	return true
}

func (p *Pool) activeRequests() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	total := 0
	for _, hp := range p.hostPools {
		hp.mu.RLock()
		for _, c := range hp.connections {
			total += int(c.activeReqs.Load())
		}
		hp.mu.RUnlock()
	}
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

	p.mu.RLock()
	pools := make([]*HostPool, 0, len(p.hostPools))
	for _, hp := range p.hostPools {
		pools = append(pools, hp)
	}
	p.mu.RUnlock()

	for _, hp := range pools {
		var idle []*Connection
		hp.mu.RLock()
		for _, c := range hp.connections {
			if c.activeReqs.Load() == 0 && c.IsHealthy() {
				lastUsed := c.lastUsed.Load()
				if now-lastUsed > threshold {
					idle = append(idle, c)
				}
			}
		}
		hp.mu.RUnlock()
		if len(idle) > 0 {
			hp.removeUnhealthyConnections(idle)
		}
	}
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
		if conn := hp.createConnection(); conn != nil {
			return conn
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
	p.mu.RLock()
	hp, exists := p.hostPools[key]
	p.mu.RUnlock()
	if exists {
		return hp
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if hp, exists := p.hostPools[key]; exists {
		return hp
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

	hp = &HostPool{
		connections: make([]*Connection, 0, maxIdle),
		maxIdle:     maxIdle,
		maxActive:   maxActive,
		pool:        p,
		host:        key,
		dialHost:    dialHost,
		dialPort:    dialPort,
		tlsConfig:   tlsCfg,
	}
	p.hostPools[key] = hp
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
func (hp *HostPool) removeUnhealthyConnections(unhealthy []*Connection) {
	if len(unhealthy) == 0 {
		return
	}
	hp.mu.Lock()
	defer hp.mu.Unlock()

	useMap := len(unhealthy) > 4
	var unhealthyMap map[*Connection]bool
	if useMap {
		unhealthyMap = make(map[*Connection]bool, len(unhealthy))
		for _, c := range unhealthy {
			unhealthyMap[c] = true
		}
	}

	newConns := hp.connections[:0]
	removed := 0
	for _, c := range hp.connections {
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
			c.Stop()
			removed++
		} else {
			newConns = append(newConns, c)
		}
	}
	hp.connections = newConns
	if removed > 0 {
		hp.activeCount.Add(-int32(removed))
		hp.idleCount.Add(-int32(removed))
	}
}

// getIdleConnection returns a healthy, pipeline-ready connection.
func (hp *HostPool) getIdleConnection() *Connection {
	for {
		hp.mu.RLock()
		connCount := uint32(len(hp.connections))
		if connCount == 0 {
			hp.mu.RUnlock()
			return nil
		}

		startIndex := hp.index.Load()
		firstIndex := startIndex % connCount
		firstConn := hp.connections[firstIndex]

		if firstConn.IsHealthy() && firstConn.CanAcceptRequest() {
			hp.index.Store((firstIndex + 1) % connCount)
			hp.mu.RUnlock()
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
			c := hp.connections[idx]
			if c.IsHealthy() && c.CanAcceptRequest() {
				hp.index.Store((idx + 1) % connCount)
				found = c
				break
			} else if !c.IsHealthy() {
				unhealthy = append(unhealthy, c)
			}
		}
		hp.mu.RUnlock()

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
			hp.mu.RLock()
			unhealthy = unhealthy[:0]
			for i := maxScan; i < connCount; i++ {
				idx := (startIndex + i) % connCount
				c := hp.connections[idx]
				if c.IsHealthy() && c.CanAcceptRequest() {
					hp.index.Store((idx + 1) % connCount)
					hp.mu.RUnlock()
					if len(unhealthy) > 0 {
						hp.removeUnhealthyConnections(unhealthy)
					}
					return c
				} else if !c.IsHealthy() {
					unhealthy = append(unhealthy, c)
				}
			}
			hp.mu.RUnlock()
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
		hp.mu.RLock()
		connCount := uint32(len(hp.connections))
		if connCount == 0 {
			hp.mu.RUnlock()
			return nil
		}

		startIndex := hp.index.Load()
		firstIndex := startIndex % connCount
		firstConn := hp.connections[firstIndex]

		if firstConn.IsHealthy() {
			hp.index.Store((firstIndex + 1) % connCount)
			hp.mu.RUnlock()
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
			c := hp.connections[idx]
			if c.IsHealthy() {
				hp.index.Store((idx + 1) % connCount)
				found = c
				break
			} else {
				unhealthy = append(unhealthy, c)
			}
		}
		hp.mu.RUnlock()

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

	hp.mu.Lock()
	hp.connections = append(hp.connections, conn)
	hp.idleCount.Add(1)
	hp.mu.Unlock()

	return conn
}

// GetHealthyConnections returns the total healthy connection count across all pools.
func (p *Pool) GetHealthyConnections() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, hp := range p.hostPools {
		hp.mu.RLock()
		for _, c := range hp.connections {
			if c.IsHealthy() {
				count++
			}
		}
		hp.mu.RUnlock()
	}
	return count
}
