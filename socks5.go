package client

import (
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"time"
)

var (
	errSOCKS5AuthFailed  = errors.New("SOCKS5 authentication failed")
	errSOCKS5ConnRefused = errors.New("SOCKS5 connection refused")
	errSOCKS5Unsupported = errors.New("SOCKS5 unsupported address type")
)

// SOCKS5Dialer handles connections through a SOCKS5 proxy.
type SOCKS5Dialer struct {
	proxyAddr     string
	username      string
	password      string
	timeout       time.Duration
	enableLogging bool
}

// NewSOCKS5Dialer creates a SOCKS5 dialer from configuration.
func NewSOCKS5Dialer(config *Config) (*SOCKS5Dialer, error) {
	if config.SOCKS5Addr == "" {
		return nil, WrapError(ErrorTypeProxy, "SOCKS5 address is empty", ErrProxyFailed)
	}
	timeout := config.ProxyConnectTimeout
	if timeout <= 0 {
		timeout = config.DialTimeout
	}
	return &SOCKS5Dialer{
		proxyAddr:     config.SOCKS5Addr,
		username:      config.SOCKS5Username,
		password:      config.SOCKS5Password,
		timeout:       timeout,
		enableLogging: config.EnableLogging,
	}, nil
}

// Dial connects to the target host:port through the SOCKS5 proxy.
func (d *SOCKS5Dialer) Dial(host string, port int) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", d.proxyAddr, d.timeout)
	if err != nil {
		return nil, LogErrorWithFlag(ErrorTypeProxy, "SOCKS5 connect failed", err,
			map[string]interface{}{"proxy": d.proxyAddr}, d.enableLogging)
	}

	if err := conn.SetDeadline(time.Now().Add(d.timeout)); err != nil {
		conn.Close()
		return nil, err
	}

	if err := d.handshake(conn); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeProxy, "SOCKS5 handshake failed", err,
			map[string]interface{}{"proxy": d.proxyAddr}, d.enableLogging)
	}

	if err := d.connectRequest(conn, host, port); err != nil {
		conn.Close()
		return nil, LogErrorWithFlag(ErrorTypeProxy, "SOCKS5 connect request failed", err,
			map[string]interface{}{"host": host, "port": port}, d.enableLogging)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (d *SOCKS5Dialer) handshake(conn net.Conn) error {
	if d.username != "" {
		_, err := conn.Write([]byte{0x05, 0x02, 0x00, 0x02})
		if err != nil {
			return err
		}
	} else {
		_, err := conn.Write([]byte{0x05, 0x01, 0x00})
		if err != nil {
			return err
		}
	}

	var resp [2]byte
	if _, err := readFull(conn, resp[:]); err != nil {
		return err
	}
	if resp[0] != 0x05 {
		return errSOCKS5Unsupported
	}

	switch resp[1] {
	case 0x00:
		return nil
	case 0x02:
		return d.authenticate(conn)
	case 0xFF:
		return errSOCKS5AuthFailed
	default:
		return errSOCKS5Unsupported
	}
}

func (d *SOCKS5Dialer) authenticate(conn net.Conn) error {
	uLen := len(d.username)
	pLen := len(d.password)
	if uLen > 255 || pLen > 255 {
		return errSOCKS5AuthFailed
	}
	buf := make([]byte, 3+uLen+pLen)
	buf[0] = 0x01 // version
	buf[1] = byte(uLen)
	copy(buf[2:], d.username)
	buf[2+uLen] = byte(pLen)
	copy(buf[3+uLen:], d.password)

	if _, err := conn.Write(buf); err != nil {
		return err
	}

	var resp [2]byte
	if _, err := readFull(conn, resp[:]); err != nil {
		return err
	}
	if resp[1] != 0x00 {
		return errSOCKS5AuthFailed
	}
	return nil
}

func (d *SOCKS5Dialer) connectRequest(conn net.Conn, host string, port int) error {
	portBytes := [2]byte{}
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))

	var req []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = make([]byte, 10)
			req[0] = 0x05
			req[1] = 0x01
			req[2] = 0x00
			req[3] = 0x01 // IPv4
			copy(req[4:8], ip4)
			copy(req[8:10], portBytes[:])
		} else {
			req = make([]byte, 22)
			req[0] = 0x05
			req[1] = 0x01
			req[2] = 0x00
			req[3] = 0x04 // IPv6
			copy(req[4:20], ip.To16())
			copy(req[20:22], portBytes[:])
		}
	} else {
		hostLen := len(host)
		if hostLen > 255 {
			hostLen = 255
			host = host[:255]
		}
		req = make([]byte, 7+hostLen)
		req[0] = 0x05
		req[1] = 0x01
		req[2] = 0x00
		req[3] = 0x03 // Domain
		req[4] = byte(hostLen)
		copy(req[5:5+hostLen], host)
		copy(req[5+hostLen:], portBytes[:])
	}

	if _, err := conn.Write(req); err != nil {
		return err
	}

	var hdr [4]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 0x05 {
		return errSOCKS5Unsupported
	}
	if hdr[1] != 0x00 {
		return &DetailedError{
			Type:    ErrorTypeProxy,
			Message: "SOCKS5 connection failed with code " + strconv.Itoa(int(hdr[1])),
			Err:     errSOCKS5ConnRefused,
		}
	}

	switch hdr[3] {
	case 0x01: // IPv4
		var skip [6]byte // 4 addr + 2 port
		_, err := readFull(conn, skip[:])
		return err
	case 0x04: // IPv6
		var skip [18]byte // 16 addr + 2 port
		_, err := readFull(conn, skip[:])
		return err
	case 0x03: // Domain
		var dLen [1]byte
		if _, err := readFull(conn, dLen[:]); err != nil {
			return err
		}
		skip := make([]byte, int(dLen[0])+2) // domain + port
		_, err := readFull(conn, skip)
		return err
	default:
		return errSOCKS5Unsupported
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
