package client

import (
	"net"
	"strings"
	"syscall"
)

// Dialer handles TCP connection establishment with optimised socket settings.
type Dialer struct {
	config       *Config
	proxyDialer  *ProxyDialer
	socks5Dialer *SOCKS5Dialer
	dnsCache     *DNSCache
}

// NewDialer creates a new Dialer from configuration.
func NewDialer(config *Config) (*Dialer, error) {
	d := &Dialer{config: config}
	if config.SOCKS5Addr != "" {
		var err error
		d.socks5Dialer, err = NewSOCKS5Dialer(config)
		if err != nil {
			return nil, WrapError(ErrorTypeProxy, "failed to create SOCKS5 dialer", err)
		}
	} else if config.ProxyURL != "" {
		var err error
		d.proxyDialer, err = NewProxyDialer(config)
		if err != nil {
			return nil, WrapError(ErrorTypeProxy, "failed to create proxy dialer", err)
		}
	}
	if config.EnableDNSCache {
		d.dnsCache = NewDNSCache(config.DNSCacheTTL, config.DNSNegativeTTL)
	}
	return d, nil
}

// IsForwardProxy reports whether HTTP requests to non-TLS targets should be
// sent via forward-proxy mode (absolute URI + Proxy-Authorization), rather
// than through an HTTP CONNECT tunnel.
func (d *Dialer) IsForwardProxy(useTLS bool) bool {
	return d.proxyDialer != nil && !useTLS
}

// ProxyAuthHeader returns the pre-encoded "Proxy-Authorization: Basic …\r\n"
// header bytes, or nil when no proxy credentials are configured.
func (d *Dialer) ProxyAuthHeader() []byte {
	if d.proxyDialer != nil {
		return d.proxyDialer.authHeader
	}
	return nil
}

// DialForward connects to the proxy's TCP address without issuing a CONNECT
// request, then applies the same socket options as DialAddr.
// Used when IsForwardProxy returns true.
func (d *Dialer) DialForward() (net.Conn, error) {
	if d.proxyDialer == nil {
		return nil, WrapError(ErrorTypeProxy, "no proxy configured for forward dial", ErrProxyFailed)
	}
	conn, err := d.proxyDialer.DialForward()
	if err != nil {
		return nil, err
	}
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(d.config.TCPNoDelay); err != nil {
			conn.Close()
			return nil, LogErrorWithFlag(ErrorTypeInternal, "set TCP no delay failed", err, nil, d.config.EnableLogging)
		}
		if d.config.TCPKeepAlive {
			if err := tcpConn.SetKeepAlive(true); err != nil {
				conn.Close()
				return nil, LogErrorWithFlag(ErrorTypeInternal, "set TCP keep alive failed", err, nil, d.config.EnableLogging)
			}
			if d.config.TCPKeepAlivePeriod > 0 {
				_ = tcpConn.SetKeepAlivePeriod(d.config.TCPKeepAlivePeriod)
			}
		}
		if d.config.ReadBufferSize > 0 {
			_ = tcpConn.SetReadBuffer(d.config.ReadBufferSize)
		}
		if d.config.WriteBufferSize > 0 {
			_ = tcpConn.SetWriteBuffer(d.config.WriteBufferSize)
		}
	}
	return conn, nil
}

// Stop cleans up dialer resources (DNS cache).
func (d *Dialer) Stop() {
	if d.dnsCache != nil {
		d.dnsCache.Stop()
	}
}

// Dial establishes a connection to the given address ("host:port").
// Falls back to config Host/Port when address is empty.
func (d *Dialer) Dial() (net.Conn, error) {
	return d.DialAddr("", 0)
}

// DialAddr establishes a connection to host:port.
// When host is empty, d.config.Host is used; when port is 0, d.config.Port is used.
func (d *Dialer) DialAddr(host string, port int) (net.Conn, error) {
	if host == "" {
		host = d.config.Host
	}
	if port == 0 {
		port = d.config.Port
	}
	if port <= 0 {
		if d.config.UseTLS {
			port = 443
		} else {
			port = 80
		}
	}

	var conn net.Conn
	var err error

	if d.socks5Dialer != nil {
		conn, err = d.socks5Dialer.Dial(host, port)
		if err != nil {
			return nil, LogErrorWithFlag(ErrorTypeProxy, "SOCKS5 dial failed", err,
				map[string]interface{}{"host": host, "port": port}, d.config.EnableLogging)
		}
	} else if d.proxyDialer != nil {
		conn, err = d.proxyDialer.Dial(host, port)
		if err != nil {
			return nil, LogErrorWithFlag(ErrorTypeProxy, "proxy dial failed", err,
				map[string]interface{}{"host": host, "port": port}, d.config.EnableLogging)
		}
	} else {
		dialHost := host
		if d.dnsCache != nil {
			addrs, lookupErr := d.dnsCache.LookupHost(host)
			if lookupErr == nil && len(addrs) > 0 {
				dialHost = addrs[0]
			}
		}
		address := buildAddr(dialHost, port)
		dialer := &net.Dialer{
			Timeout:   d.config.DialTimeout,
			KeepAlive: d.config.TCPKeepAlivePeriod,
		}
		if d.config.TCPFastOpen || d.config.TCPReusePort {
			dialer.Control = func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					applySocketOptions(fd, d.config)
				})
			}
		}
		conn, err = dialer.Dial("tcp", address)
		if err != nil {
			return nil, LogErrorWithFlag(ErrorTypeNetwork, "dial failed", err,
				map[string]interface{}{"address": address}, d.config.EnableLogging)
		}
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(d.config.TCPNoDelay); err != nil {
			conn.Close()
			return nil, LogErrorWithFlag(ErrorTypeInternal, "set TCP no delay failed", err, nil, d.config.EnableLogging)
		}
		if d.config.TCPKeepAlive {
			if err := tcpConn.SetKeepAlive(true); err != nil {
				conn.Close()
				return nil, LogErrorWithFlag(ErrorTypeInternal, "set TCP keep alive failed", err, nil, d.config.EnableLogging)
			}
			if d.config.TCPKeepAlivePeriod > 0 {
				_ = tcpConn.SetKeepAlivePeriod(d.config.TCPKeepAlivePeriod)
			}
		}
		if d.config.ReadBufferSize > 0 {
			_ = tcpConn.SetReadBuffer(d.config.ReadBufferSize)
		}
		if d.config.WriteBufferSize > 0 {
			_ = tcpConn.SetWriteBuffer(d.config.WriteBufferSize)
		}
	}

	return conn, nil
}

// buildAddr combines host and port into "host:port" without allocating for
// the most common ports.
func buildAddr(host string, port int) string {
	var portStr string
	switch port {
	case 80:
		portStr = "80"
	case 443:
		portStr = "443"
	case 8080:
		portStr = "8080"
	case 8443:
		portStr = "8443"
	default:
		var buf [8]byte
		n := 0
		p := port
		if p == 0 {
			buf[n] = '0'
			n++
		} else {
			start := len(buf)
			for p > 0 {
				start--
				buf[start] = byte('0' + p%10)
				p /= 10
				n++
			}
			copy(buf[:], buf[len(buf)-n:])
		}
		portStr = string(buf[:n])
	}

	if strings.ContainsRune(host, ':') {
		return "[" + host + "]:" + portStr
	}
	return host + ":" + portStr
}
