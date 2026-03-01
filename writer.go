package client

import (
	"net"
)

// writeRequest encodes an HTTP request into buf.
// host and port are the actual dial target (may differ from cfg.Host when
// URL-routing is used). useTLS controls whether the default port is omitted.
func writeRequest(buf []byte, req *Request, cfg *Config, host string, port int, useTLS bool, body []byte, compressed bool) (int, error) {
	pos := 0
	method := req.Method
	if method == "" {
		method = "GET"
	}
	path := req.effectivePath()

	if !writeString(buf, &pos, method) ||
		!writeByte(buf, &pos, ' ') ||
		!writeString(buf, &pos, path) ||
		!writeString(buf, &pos, " HTTP/1.1\r\n") {
		return 0, ErrHeaderBufferSmall
	}

	if !writeString(buf, &pos, "Host: ") {
		return 0, ErrHeaderBufferSmall
	}
	if !writeString(buf, &pos, host) {
		return 0, ErrHeaderBufferSmall
	}
	defaultPort := 80
	if useTLS {
		defaultPort = 443
	}
	if port > 0 && port != defaultPort {
		if !writeByte(buf, &pos, ':') {
			return 0, ErrHeaderBufferSmall
		}
		if !writeInt(buf, &pos, port) {
			return 0, ErrHeaderBufferSmall
		}
	}
	if !writeString(buf, &pos, "\r\n") {
		return 0, ErrHeaderBufferSmall
	}

	if cfg.KeepAlive && !cfg.DisableKeepAlive {
		if !writeString(buf, &pos, "Connection: keep-alive\r\n") {
			return 0, ErrHeaderBufferSmall
		}
	} else {
		if !writeString(buf, &pos, "Connection: close\r\n") {
			return 0, ErrHeaderBufferSmall
		}
	}

	if compressed {
		if !writeString(buf, &pos, "Content-Encoding: gzip\r\n") {
			return 0, ErrHeaderBufferSmall
		}
	}

	if req.headerLen > 0 {
		if !writeBytes(buf, &pos, req.headerBuf[:req.headerLen]) {
			return 0, ErrHeaderBufferSmall
		}
	}

	// Expect: 100-continue (body sent separately after server confirms).
	if req.ExpectContinue && len(body) > 0 {
		if !writeString(buf, &pos, "Expect: 100-continue\r\n") {
			return 0, ErrHeaderBufferSmall
		}
	}

	if !writeString(buf, &pos, "Content-Length: ") ||
		!writeInt(buf, &pos, len(body)) ||
		!writeString(buf, &pos, "\r\n\r\n") {
		return 0, ErrHeaderBufferSmall
	}

	return pos, nil
}

// writeString writes a string to the buffer.
func writeString(buf []byte, pos *int, s string) bool {
	if *pos+len(s) > len(buf) {
		return false
	}
	copy(buf[*pos:*pos+len(s)], s)
	*pos += len(s)
	return true
}

// writeBytes writes bytes to the buffer.
func writeBytes(buf []byte, pos *int, b []byte) bool {
	if *pos+len(b) > len(buf) {
		return false
	}
	copy(buf[*pos:*pos+len(b)], b)
	*pos += len(b)
	return true
}

// writeByte writes a byte to the buffer.
func writeByte(buf []byte, pos *int, b byte) bool {
	if *pos+1 > len(buf) {
		return false
	}
	buf[*pos] = b
	*pos++
	return true
}

// writeInt writes an integer to the buffer.
func writeInt(buf []byte, pos *int, n int) bool {
	if n < 0 {
		return false
	}

	if n == 0 {
		return writeByte(buf, pos, '0')
	}
	if n < 10 {
		return writeByte(buf, pos, byte('0'+n))
	}
	if n < 100 {
		if *pos+2 > len(buf) {
			return false
		}
		buf[*pos] = byte('0' + n/10)
		buf[*pos+1] = byte('0' + n%10)
		*pos += 2
		return true
	}
	if n < 1000 {
		if *pos+3 > len(buf) {
			return false
		}
		buf[*pos] = byte('0' + n/100)
		buf[*pos+1] = byte('0' + (n/10)%10)
		buf[*pos+2] = byte('0' + n%10)
		*pos += 3
		return true
	}
	if n < 10000 {
		if *pos+4 > len(buf) {
			return false
		}
		buf[*pos] = byte('0' + n/1000)
		buf[*pos+1] = byte('0' + (n/100)%10)
		buf[*pos+2] = byte('0' + (n/10)%10)
		buf[*pos+3] = byte('0' + n%10)
		*pos += 4
		return true
	}

	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return writeBytes(buf, pos, tmp[i:])
}

// writeAll writes all bytes to the connection.
func writeAll(conn net.Conn, buf []byte) error {
	for len(buf) > 0 {
		n, err := conn.Write(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}

// writeAllBatched writes header and body efficiently.
// For small bodies, tries to combine in single syscall when possible without allocation.
func writeAllBatched(conn net.Conn, header []byte, body []byte) error {
	if len(body) == 0 {
		return writeAll(conn, header)
	}

	// Use vectored write when supported (reduces syscalls without allocation)
	buffers := net.Buffers{header, body}
	_, err := buffers.WriteTo(conn)
	return err
}
