package client

import (
	"crypto/tls"
	"net"
	"time"
)

// TLSConfig wraps tls.Config with optimized session caching and connection management.
type TLSConfig struct {
	config           *tls.Config
	handshakeTimeout time.Duration
	enableLogging    bool
}

// NewTLSConfig creates a new TLS configuration with session caching enabled.
func NewTLSConfig(config *Config) *TLSConfig {
	tlsConfig := config.BuildTLSConfig()

	cacheSize := config.TLSClientSessionCacheSize
	if cacheSize <= 0 {
		cacheSize = 4096
	}
	sessionCache := tls.NewLRUClientSessionCache(cacheSize)
	tlsConfig.ClientSessionCache = sessionCache

	tlsConfig.SessionTicketsDisabled = false

	hsTimeout := config.TLSHandshakeTimeout
	if hsTimeout <= 0 {
		hsTimeout = 10 * time.Second
	}

	return &TLSConfig{
		config:           tlsConfig,
		handshakeTimeout: hsTimeout,
		enableLogging:    config.EnableLogging,
	}
}

// WrapConnection performs a TLS handshake over an existing connection.
// Uses session caching for faster reconnection.
func (t *TLSConfig) WrapConnection(conn net.Conn) (net.Conn, error) {
	if conn == nil {
		return nil, WrapError(ErrorTypeTLS, "connection is nil", ErrConnectFailed)
	}

	deadline := time.Now().Add(t.handshakeTimeout)
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeTLS, "set handshake deadline failed", err, nil, t.enableLogging)
	}

	tlsConn := tls.Client(conn, t.config)
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeTLS, "TLS handshake failed", err, map[string]interface{}{
			"server_name": t.config.ServerName,
		}, t.enableLogging)
	}

	_ = tlsConn.SetDeadline(time.Time{})

	return tlsConn, nil
}
