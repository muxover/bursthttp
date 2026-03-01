package client

import (
	"crypto/tls"
	"time"
)

// Config holds all configuration for the HTTP client.
// All fields are read-only after initialization for thread safety.
type Config struct {
	Host   string
	Port   int
	UseTLS bool

	PoolSize           int
	MaxRequestsPerConn int
	IdleTimeout        time.Duration

	DialTimeout         time.Duration
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	TLSHandshakeTimeout time.Duration

	ReadBufferSize      int
	WriteBufferSize     int
	HeaderBufferSize    int
	HeaderReadChunkSize int
	BodyReadChunkSize   int
	MaxRequestBodySize  int
	MaxResponseBodySize int

	EnableCompression bool
	CompressionLevel  int

	TLSConfig                 *tls.Config
	TLSMinVersion             uint16
	TLSMaxVersion             uint16
	InsecureSkipVerify        bool
	TLSClientSessionCacheSize int

	ProxyURL            string
	ProxyUsername       string
	ProxyPassword       string
	ProxyConnectTimeout time.Duration
	ProxyReadTimeout    time.Duration

	SOCKS5Addr     string // Takes precedence over ProxyURL.
	SOCKS5Username string
	SOCKS5Password string

	KeepAlive bool

	DisableKeepAlive    bool
	MaxIdleConnsPerHost int

	EnablePipelining     bool
	MaxPipelinedRequests int

	MaxRetries      int
	RetryBaseDelay  time.Duration
	RetryMaxDelay   time.Duration
	RetryMultiplier float64
	RetryJitter     bool
	RetryableStatus []int

	EnableDNSCache bool
	DNSCacheTTL    time.Duration

	IdleCheckInterval time.Duration

	EnableResponseStreaming bool

	TCPNoDelay         bool
	TCPKeepAlive       bool
	TCPKeepAlivePeriod time.Duration

	EnableLogging bool

	// Set to a MetricsCollector implementation to receive per-request telemetry.
	// nil disables metrics (default).
	Metrics MetricsCollector
}

// DefaultConfig returns a default configuration optimized for high performance.
func DefaultConfig() *Config {
	return &Config{
		Host:                      "localhost",
		Port:                      443,
		UseTLS:                    true,
		PoolSize:                  512,
		MaxRequestsPerConn:        0, // 0 = unlimited
		IdleTimeout:               90 * time.Second,
		DialTimeout:               10 * time.Second,
		ReadTimeout:               30 * time.Second,
		WriteTimeout:              10 * time.Second,
		TLSHandshakeTimeout:       10 * time.Second,
		ReadBufferSize:            64 * 1024,
		WriteBufferSize:           64 * 1024,
		HeaderBufferSize:          16 * 1024,
		HeaderReadChunkSize:       4 * 1024,
		BodyReadChunkSize:         64 * 1024,
		MaxRequestBodySize:        10 * 1024 * 1024, // 10MB
		MaxResponseBodySize:       10 * 1024 * 1024, // 10MB
		EnableCompression:         false,
		CompressionLevel:          6,
		TLSMinVersion:             tls.VersionTLS12,
		TLSMaxVersion:             tls.VersionTLS13,
		InsecureSkipVerify:        false,
		TLSClientSessionCacheSize: 4096,
		ProxyConnectTimeout:       10 * time.Second,
		ProxyReadTimeout:          10 * time.Second,
		KeepAlive:                 true,
		DisableKeepAlive:          false,
		MaxIdleConnsPerHost:       512,
		EnablePipelining:          true,
		MaxPipelinedRequests:      10,
		MaxRetries:                0,
		RetryBaseDelay:            100 * time.Millisecond,
		RetryMaxDelay:             5 * time.Second,
		RetryMultiplier:           2.0,
		RetryJitter:               true,
		RetryableStatus:           []int{429, 502, 503, 504},
		EnableDNSCache:            false,
		DNSCacheTTL:               5 * time.Minute,
		IdleCheckInterval:         30 * time.Second,
		EnableResponseStreaming:   false,
		TCPNoDelay:                true,
		TCPKeepAlive:              true,
		TCPKeepAlivePeriod:        30 * time.Second,
		EnableLogging:             false,
	}
}

// HighThroughputConfig returns a config tuned for sustained high RPS.
// Use this for large-scale workloads (100K+ RPS) with adequate hardware.
func HighThroughputConfig() *Config {
	cfg := DefaultConfig()
	cfg.PoolSize = 1024
	cfg.MaxPipelinedRequests = 16
	cfg.ReadBufferSize = 128 * 1024
	cfg.WriteBufferSize = 128 * 1024
	cfg.HeaderReadChunkSize = 8 * 1024
	cfg.BodyReadChunkSize = 64 * 1024
	cfg.MaxIdleConnsPerHost = 1024
	cfg.TLSClientSessionCacheSize = 8192
	cfg.EnableDNSCache = true
	cfg.IdleCheckInterval = 15 * time.Second
	return cfg
}

// ResilientConfig returns a config with retry and resilience features enabled.
func ResilientConfig() *Config {
	cfg := DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 200 * time.Millisecond
	cfg.RetryMaxDelay = 10 * time.Second
	cfg.RetryMultiplier = 2.0
	cfg.RetryJitter = true
	cfg.RetryableStatus = []int{429, 502, 503, 504}
	cfg.EnableDNSCache = true
	return cfg
}

// Validate normalizes and validates the configuration.
func (c *Config) Validate() error {
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == 0 {
		if c.UseTLS {
			c.Port = 443
		} else {
			c.Port = 80
		}
	}
	if c.PoolSize <= 0 {
		c.PoolSize = 512
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 30 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.ReadBufferSize <= 0 {
		c.ReadBufferSize = 64 * 1024
	}
	if c.WriteBufferSize <= 0 {
		c.WriteBufferSize = 64 * 1024
	}
	if c.HeaderBufferSize <= 0 {
		c.HeaderBufferSize = 16 * 1024
	}
	if c.HeaderReadChunkSize <= 0 {
		c.HeaderReadChunkSize = 4 * 1024
	}
	if c.BodyReadChunkSize <= 0 {
		c.BodyReadChunkSize = 64 * 1024
	}
	if c.MaxRequestBodySize <= 0 {
		c.MaxRequestBodySize = 10 * 1024 * 1024 // 10MB default
	}
	if c.MaxResponseBodySize <= 0 {
		c.MaxResponseBodySize = 10 * 1024 * 1024 // 10MB default
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 90 * time.Second
	}
	if c.ProxyConnectTimeout <= 0 {
		c.ProxyConnectTimeout = c.DialTimeout
		if c.ProxyConnectTimeout <= 0 {
			c.ProxyConnectTimeout = 10 * time.Second
		}
	}
	if c.ProxyReadTimeout <= 0 {
		c.ProxyReadTimeout = c.DialTimeout
		if c.ProxyReadTimeout <= 0 {
			c.ProxyReadTimeout = 10 * time.Second
		}
	}
	if c.TLSMinVersion == 0 {
		c.TLSMinVersion = tls.VersionTLS12
	}
	if c.TLSMaxVersion == 0 {
		c.TLSMaxVersion = tls.VersionTLS13
	}
	if c.TLSClientSessionCacheSize <= 0 {
		c.TLSClientSessionCacheSize = 4096
	}
	if c.MaxPipelinedRequests <= 0 {
		c.MaxPipelinedRequests = 10
	}
	if c.TLSHandshakeTimeout <= 0 {
		c.TLSHandshakeTimeout = 10 * time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.RetryBaseDelay <= 0 {
		c.RetryBaseDelay = 100 * time.Millisecond
	}
	if c.RetryMaxDelay <= 0 {
		c.RetryMaxDelay = 5 * time.Second
	}
	if c.RetryMultiplier <= 0 {
		c.RetryMultiplier = 2.0
	}
	if c.DNSCacheTTL <= 0 {
		c.DNSCacheTTL = 5 * time.Minute
	}
	if c.IdleCheckInterval <= 0 {
		c.IdleCheckInterval = 30 * time.Second
	}
	return nil
}

// BuildTLSConfig creates a TLS config from the configuration.
func (c *Config) BuildTLSConfig() *tls.Config {
	if c.TLSConfig != nil {
		return c.TLSConfig
	}

	config := &tls.Config{
		MinVersion:         c.TLSMinVersion,
		MaxVersion:         c.TLSMaxVersion,
		InsecureSkipVerify: c.InsecureSkipVerify,
		NextProtos:         []string{"http/1.1"},
	}

	if c.Host != "" {
		config.ServerName = c.Host
	}

	return config
}
