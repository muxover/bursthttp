package client

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"time"
)

// ProxyDialer handles HTTP CONNECT proxy connections with authentication.
// Optimized for zero-allocation and minimal syscalls.
type ProxyDialer struct {
	proxyAddr      string
	authHeader     []byte
	connectTimeout time.Duration
	readTimeout    time.Duration
	enableLogging  bool // Cached from config for performance
}

// NewProxyDialer creates a new proxy dialer from configuration.
func NewProxyDialer(config *Config) (*ProxyDialer, error) {
	u, err := url.Parse(config.ProxyURL)
	if err != nil {
		return nil, LogErrorWithFlag(ErrorTypeProxy, "invalid proxy URL", err, map[string]interface{}{
			"proxy_url": config.ProxyURL,
		}, config.EnableLogging)
	}

	proxyAddr := u.Host
	if proxyAddr == "" {
		return nil, WrapError(ErrorTypeProxy, "empty proxy address", ErrProxyFailed)
	}

	if u.Port() == "" {
		if u.Scheme == "https" {
			proxyAddr += ":443"
		} else {
			proxyAddr += ":80"
		}
	}

	var authHeader []byte
	if config.ProxyUsername != "" || config.ProxyPassword != "" {
		raw := config.ProxyUsername + ":" + config.ProxyPassword
		enc := base64.StdEncoding.EncodeToString([]byte(raw))
		authHeader = []byte("Proxy-Authorization: Basic " + enc + "\r\n")
	}

	return &ProxyDialer{
		proxyAddr:      proxyAddr,
		authHeader:     authHeader,
		connectTimeout: config.ProxyConnectTimeout,
		readTimeout:    config.ProxyReadTimeout,
		enableLogging:  config.EnableLogging,
	}, nil
}

// DialForward connects to the proxy over plain TCP without issuing a CONNECT
// request. Used for HTTP (non-TLS) targets where the proxy acts as a
// forward proxy: the client sends the absolute-form request directly to the
// proxy TCP socket and the proxy forwards it to the origin.
func (p *ProxyDialer) DialForward() (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", p.proxyAddr, p.connectTimeout)
	if err != nil {
		return nil, LogErrorWithFlag(ErrorTypeProxy, "forward proxy connection failed", err, map[string]interface{}{
			"proxy": p.proxyAddr,
		}, p.enableLogging)
	}
	return conn, nil
}

// Dial establishes a connection through the proxy using HTTP CONNECT.
// Each CONNECT tunnel is bound to a single target; the main pool handles reuse.
func (p *ProxyDialer) Dial(host string, port int) (net.Conn, error) {
	target := p.buildTarget(host, port)

	conn, err := net.DialTimeout("tcp", p.proxyAddr, p.connectTimeout)
	if err != nil {
		return nil, LogErrorWithFlag(ErrorTypeProxy, "proxy connection failed", err, map[string]interface{}{
			"proxy": p.proxyAddr,
		}, p.enableLogging)
	}

	if err := p.writeConnect(conn, target); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeProxy, "proxy CONNECT write failed", err, map[string]interface{}{
			"target": target,
		}, p.enableLogging)
	}

	if err := p.readConnectResponse(conn); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeProxy, "proxy CONNECT response failed", err, map[string]interface{}{
			"target": target,
		}, p.enableLogging)
	}

	return conn, nil
}

func (p *ProxyDialer) buildTarget(host string, port int) string {
	if port == 443 {
		return host + ":443"
	}
	if port == 80 {
		return host + ":80"
	}

	var portBuf [12]byte
	start := len(portBuf)
	n := port
	if n == 0 {
		portBuf[start-1] = '0'
		start--
	} else {
		for n > 0 {
			start--
			portBuf[start] = byte('0' + n%10)
			n /= 10
		}
	}
	return host + ":" + string(portBuf[start:])
}

// writeConnect sends an HTTP CONNECT request to the proxy using a stack buffer.
func (p *ProxyDialer) writeConnect(conn net.Conn, target string) error {
	targetLen := len(target)
	authLen := len(p.authHeader)
	totalSize := 8 + targetLen + 17 + targetLen + 2 + authLen + 2

	var stackBuf [512]byte
	var buf []byte
	if totalSize <= len(stackBuf) {
		buf = stackBuf[:totalSize]
	} else {
		buf = make([]byte, totalSize)
	}

	pos := 0
	copy(buf[pos:], "CONNECT ")
	pos += 8
	copy(buf[pos:], target)
	pos += targetLen
	copy(buf[pos:], " HTTP/1.1\r\nHost: ")
	pos += 17
	copy(buf[pos:], target)
	pos += targetLen
	copy(buf[pos:], "\r\n")
	pos += 2
	if authLen > 0 {
		copy(buf[pos:], p.authHeader)
		pos += authLen
	}
	copy(buf[pos:], "\r\n")

	deadline := time.Now().Add(p.connectTimeout)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}

	if err := writeAll(conn, buf); err != nil {
		_ = conn.SetWriteDeadline(time.Time{})
		return err
	}

	_ = conn.SetWriteDeadline(time.Time{})
	return nil
}

// readConnectResponse reads and validates the proxy CONNECT response.
func (p *ProxyDialer) readConnectResponse(conn net.Conn) error {
	deadline := time.Now().Add(p.readTimeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}

	var buf [512]byte
	total := 0

	n, err := conn.Read(buf[:])
	if err != nil {
		_ = conn.SetReadDeadline(time.Time{})
		return err
	}
	if n == 0 {
		_ = conn.SetReadDeadline(time.Time{})
		return WrapError(ErrorTypeProtocol, "proxy connection closed", ErrInvalidResponse)
	}
	total = n

	headerEnd := findHeaderEnd(buf[:total])

	if headerEnd < 0 {
		if total >= len(buf) {
			_ = conn.SetReadDeadline(time.Time{})
			return WrapError(ErrorTypeProtocol, "proxy response too large", ErrHeaderTooLarge)
		}

		n, err := conn.Read(buf[total:])
		if err != nil {
			_ = conn.SetReadDeadline(time.Time{})
			return err
		}
		if n == 0 {
			_ = conn.SetReadDeadline(time.Time{})
			return WrapError(ErrorTypeProtocol, "proxy connection closed", ErrInvalidResponse)
		}
		total += n
		headerEnd = findHeaderEnd(buf[:total])

		if headerEnd < 0 {
			_ = conn.SetReadDeadline(time.Time{})
			return WrapError(ErrorTypeProtocol, "proxy response incomplete", ErrInvalidResponse)
		}
	}

	_ = conn.SetReadDeadline(time.Time{})

	if total < 12 {
		return WrapError(ErrorTypeProtocol, "proxy response too short", ErrInvalidResponse)
	}

	// Status code sits at bytes 9-11 ("HTTP/1.x 200 ...")
	if !(buf[9] == '2' && buf[10] == '0' && buf[11] == '0') {
		statusCode := int(buf[9]-'0')*100 + int(buf[10]-'0')*10 + int(buf[11]-'0')
		return WrapError(ErrorTypeProxy, fmt.Sprintf("proxy CONNECT rejected (HTTP %d)", statusCode), ErrProxyFailed)
	}

	return nil
}
