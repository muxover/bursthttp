package client

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
)

// readResponse parses an HTTP/1.1 response from r.
// method is the HTTP method used (HEAD requests have no body).
// buf is a caller-supplied scratch buffer; it must not be retained after return.
// r may be a *bufio.Reader to preserve leftover bytes between pipelined reads.
func readResponse(r io.Reader, buf []byte, resp *Response, maxBody int, method string, headerReadSize int, bodyReadSize int) error {
	total := 0
	headerEnd := -1

	readSize := headerReadSize
	if readSize > len(buf) {
		readSize = len(buf)
	}

	for headerEnd < 0 {
		if total >= len(buf) {
			return ErrHeaderTooLarge
		}

		remaining := len(buf) - total
		if remaining > readSize {
			remaining = readSize
		}

		n, err := r.Read(buf[total : total+remaining])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		total += n
		headerEnd = findHeaderEnd(buf[:total])
	}

	status, ok := parseStatusCode(buf[:headerEnd])
	if !ok {
		return WrapError(ErrorTypeProtocol, "failed to parse status code", ErrInvalidResponse)
	}

	isHEAD := method == "HEAD"

	// No-body status codes.
	if isHEAD || status == 204 || status == 304 || status == 101 {
		resp.StatusCode = status
		resp.ContentLength = 0
		parseHeaders(buf[:headerEnd], resp)
		resp.Body = resp.bodyBuf[:0]
		return nil
	}

	// Check Transfer-Encoding: chunked before Content-Length.
	if parseTransferEncoding(buf[:headerEnd]) {
		resp.StatusCode = status
		parseHeaders(buf[:headerEnd], resp)
		return readChunkedBody(r, buf, resp, headerEnd, total, maxBody, bodyReadSize)
	}

	length, hasLen := parseContentLength(buf[:headerEnd])
	if !hasLen {
		// No Content-Length, no chunked — read until close.
		return readResponseUntilEOF(r, buf, resp, headerEnd, total, maxBody, bodyReadSize)
	}

	if length > maxBody {
		return ErrResponseTooLarge
	}

	resp.StatusCode = status
	resp.ContentLength = length
	parseHeaders(buf[:headerEnd], resp)

	if length == 0 {
		resp.Body = resp.bodyBuf[:0]
		return nil
	}

	if !resp.ensureBufferSize(length) {
		return ErrResponseTooLarge
	}

	resp.Body = resp.bodyBuf[:length]

	bodyStart := headerEnd
	bodyInBuf := total - bodyStart
	if bodyInBuf > length {
		bodyInBuf = length
	}
	if bodyInBuf > 0 {
		copy(resp.bodyBuf[:bodyInBuf], buf[bodyStart:bodyStart+bodyInBuf])
	}

	remaining := length - bodyInBuf
	offset := bodyInBuf

	if bodyReadSize <= 0 {
		bodyReadSize = 65536
	}

	for remaining > 0 {
		toRead := remaining
		if toRead > bodyReadSize {
			toRead = bodyReadSize
		}
		n, err := r.Read(resp.bodyBuf[offset : offset+toRead])
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		offset += n
		remaining -= n
	}

	if offset != length {
		return WrapError(ErrorTypeProtocol, "incomplete body read", ErrInvalidResponse)
	}

	return nil
}

// readChunkedBody reads a chunked transfer-encoding body.
// Initial bytes already in buf[headerEnd:total] are consumed first.
func readChunkedBody(r io.Reader, buf []byte, resp *Response, headerEnd, total, maxBody, bodyReadSize int) error {
	if bodyReadSize <= 0 {
		bodyReadSize = 65536
	}

	// Leftover bytes from the header read that belong to the body.
	leftover := buf[headerEnd:total]
	leftoverOff := 0

	var lineBuf [64]byte

	// readByte reads the next byte from leftover or r.
	readByte := func() (byte, error) {
		if leftoverOff < len(leftover) {
			b := leftover[leftoverOff]
			leftoverOff++
			return b, nil
		}
		var tmp [1]byte
		for {
			n, err := r.Read(tmp[:])
			if n > 0 {
				return tmp[0], nil
			}
			if err != nil {
				return 0, err
			}
		}
	}

	// readLine reads until LF, returning content without CRLF.
	readLine := func() ([]byte, error) {
		lineLen := 0
		for {
			b, err := readByte()
			if err != nil {
				return nil, err
			}
			if b == '\n' {
				end := lineLen
				if end > 0 && lineBuf[end-1] == '\r' {
					end--
				}
				return lineBuf[:end], nil
			}
			if lineLen < len(lineBuf) {
				lineBuf[lineLen] = b
				lineLen++
			}
		}
	}

	// readExact reads exactly n bytes.
	readExact := func(dst []byte) error {
		off := 0
		for off < len(dst) {
			if leftoverOff < len(leftover) {
				n := copy(dst[off:], leftover[leftoverOff:])
				leftoverOff += n
				off += n
				continue
			}
			n, err := r.Read(dst[off:])
			if n > 0 {
				off += n
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	bodyLen := 0

	for {
		line, err := readLine()
		if err != nil {
			return err
		}

		// Strip chunk extensions.
		for i, c := range line {
			if c == ';' {
				line = line[:i]
				break
			}
		}

		chunkSize64, err := strconv.ParseInt(string(line), 16, 64)
		if err != nil || chunkSize64 < 0 {
			return WrapError(ErrorTypeProtocol, "invalid chunk size", ErrInvalidResponse)
		}
		chunkSize := int(chunkSize64)

		if chunkSize == 0 {
			// Drain trailers until we reach the blank line (RFC 7230 §4.1).
			for {
				line, err := readLine()
				if err != nil || len(line) == 0 {
					break
				}
			}
			break
		}

		if maxBody > 0 && bodyLen+chunkSize > maxBody {
			return ErrResponseTooLarge
		}

		if !resp.ensureBufferSize(bodyLen + chunkSize) {
			return ErrResponseTooLarge
		}

		if err := readExact(resp.bodyBuf[bodyLen : bodyLen+chunkSize]); err != nil {
			return err
		}
		bodyLen += chunkSize

		// Consume CRLF after chunk data.
		_, _ = readLine()
	}

	resp.ContentLength = bodyLen
	resp.Body = resp.bodyBuf[:bodyLen]
	return nil
}

// readResponseUntilEOF reads response body until EOF when Content-Length is missing
// and Transfer-Encoding is not chunked. Only valid for Connection: close responses.
func readResponseUntilEOF(r io.Reader, buf []byte, resp *Response, headerEnd int, total int, maxBody int, bodyReadSize int) error {
	status, _ := parseStatusCode(buf[:headerEnd])
	resp.StatusCode = status
	parseHeaders(buf[:headerEnd], resp)

	bodyStart := headerEnd
	bodyInBuf := total - bodyStart
	if bodyInBuf > 0 {
		if !resp.ensureBufferSize(bodyInBuf) {
			bodyInBuf = len(resp.bodyBuf)
		}
		if bodyInBuf > 0 {
			copy(resp.bodyBuf[:bodyInBuf], buf[bodyStart:bodyStart+bodyInBuf])
		}
	}

	offset := bodyInBuf
	if bodyReadSize <= 0 {
		bodyReadSize = 65536
	}

	for {
		if maxBody > 0 && offset >= maxBody {
			return ErrResponseTooLarge
		}

		requiredSize := offset + bodyReadSize
		if maxBody > 0 && requiredSize > maxBody {
			requiredSize = maxBody
		}
		if !resp.ensureBufferSize(requiredSize) {
			if offset >= len(resp.bodyBuf) {
				return ErrResponseTooLarge
			}
			requiredSize = len(resp.bodyBuf)
		}

		toRead := len(resp.bodyBuf) - offset
		if toRead > bodyReadSize {
			toRead = bodyReadSize
		}
		if maxBody > 0 && toRead > maxBody-offset {
			toRead = maxBody - offset
		}
		if toRead <= 0 {
			break
		}

		n, err := r.Read(resp.bodyBuf[offset : offset+toRead])
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if n == 0 {
			break
		}
		offset += n
	}

	resp.ContentLength = offset
	resp.Body = resp.bodyBuf[:offset]
	return nil
}

// readResponseBuffered reads one HTTP/1.1 response from a *bufio.Reader.
//
// Headers are read byte-by-byte via br.ReadByte() so the read cursor stops
// exactly at the end of "\r\n\r\n" — no bytes from the next response are
// consumed into the scratch buffer.  The body is read with io.ReadFull (for
// Content-Length responses), which reads exactly the declared number of bytes
// and leaves all remaining data in br's internal buffer for the next response.
//
// This is the correct parser for HTTP/1.1 pipelining where multiple responses
// may arrive in a single TCP segment.
func readResponseBuffered(br *bufio.Reader, scratch []byte, resp *Response, maxBody int, method string) error {
	total := 0
	for {
		if total >= len(scratch) {
			return ErrHeaderTooLarge
		}
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		scratch[total] = b
		total++
		// Detect the blank-line terminator.
		if total >= 4 &&
			scratch[total-4] == '\r' && scratch[total-3] == '\n' &&
			scratch[total-2] == '\r' && scratch[total-1] == '\n' {
			break
		}
	}
	headerEnd := total // scratch[:headerEnd] is the complete header block

	status, ok := parseStatusCode(scratch[:headerEnd])
	if !ok {
		return WrapError(ErrorTypeProtocol, "failed to parse status code", ErrInvalidResponse)
	}

	isHEAD := method == "HEAD"

	// No-body status codes.
	if isHEAD || status == 204 || status == 304 || status == 101 {
		resp.StatusCode = status
		resp.ContentLength = 0
		parseHeaders(scratch[:headerEnd], resp)
		resp.Body = resp.bodyBuf[:0]
		return nil
	}

	// Chunked transfer-encoding.
	if parseTransferEncoding(scratch[:headerEnd]) {
		resp.StatusCode = status
		parseHeaders(scratch[:headerEnd], resp)
		if err := readChunkedBody(br, scratch, resp, headerEnd, headerEnd, maxBody, 65536); err != nil {
			return err
		}
		return decompressBody(resp)
	}

	// Content-Length.
	length, hasLen := parseContentLength(scratch[:headerEnd])
	if !hasLen {
		// No Content-Length, no chunked — read until connection close.
		if err := readResponseUntilEOF(br, scratch, resp, headerEnd, headerEnd, maxBody, 65536); err != nil {
			return err
		}
		return decompressBody(resp)
	}

	if maxBody > 0 && length > maxBody {
		return ErrResponseTooLarge
	}

	resp.StatusCode = status
	resp.ContentLength = length
	parseHeaders(scratch[:headerEnd], resp)

	if length == 0 {
		resp.Body = resp.bodyBuf[:0]
		return nil
	}

	if !resp.ensureBufferSize(length) {
		return ErrResponseTooLarge
	}
	resp.Body = resp.bodyBuf[:length]

	// io.ReadFull reads EXACTLY length bytes from br's internal buffer (plus
	// the underlying conn if needed), leaving all subsequent bytes intact for
	// the next response in the pipeline.
	if _, err := io.ReadFull(br, resp.Body); err != nil {
		return err
	}
	return decompressBody(resp)
}

// decompressBody decompresses resp.Body in-place when Content-Encoding is gzip.
// It replaces resp.Body and resp.bodyBuf with the decompressed data and updates
// resp.ContentLength. It is a no-op for any other or absent encoding.
func decompressBody(resp *Response) error {
	if len(resp.Body) == 0 {
		return nil
	}
	// Scan headers for Content-Encoding: gzip (case-insensitive).
	isGzip := false
	for _, h := range resp.Headers {
		if strings.EqualFold(h.Key, "Content-Encoding") &&
			strings.EqualFold(strings.TrimSpace(h.Value), "gzip") {
			isGzip = true
			break
		}
	}
	if !isGzip {
		return nil
	}

	gr, err := gzip.NewReader(bytes.NewReader(resp.Body))
	if err != nil {
		return WrapError(ErrorTypeProtocol, "gzip reader init failed", err)
	}
	defer gr.Close()

	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return WrapError(ErrorTypeProtocol, "gzip decompression failed", err)
	}

	// Store decompressed bytes back into resp, growing bodyBuf if needed.
	if !resp.ensureBufferSize(len(decompressed)) {
		// If maxSize would be exceeded, store what we can and truncate.
		decompressed = decompressed[:len(resp.bodyBuf)]
	}
	copy(resp.bodyBuf[:len(decompressed)], decompressed)
	resp.Body = resp.bodyBuf[:len(decompressed)]
	resp.ContentLength = len(decompressed)
	return nil
}

// findHeaderEnd finds the end of HTTP headers (\r\n\r\n).
func findHeaderEnd(buf []byte) int {
	if len(buf) < 4 {
		return -1
	}
	for i := 0; i+3 < len(buf); i++ {
		if buf[i] == '\r' {
			if buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n' {
				return i + 4
			}
			if i+1 < len(buf) && buf[i+1] == '\n' {
				i++
			}
		}
	}
	return -1
}

// parseStatusCode extracts the HTTP status code from the response line.
// Handles HTTP/1.0 and HTTP/1.1. Returns (code, true) on success.
func parseStatusCode(buf []byte) (int, bool) {
	// Minimum: "HTTP/1.1 200" = 12 bytes
	if len(buf) < 12 {
		return 0, false
	}
	if buf[0] != 'H' || buf[1] != 'T' || buf[2] != 'T' || buf[3] != 'P' || buf[4] != '/' {
		return 0, false
	}
	// Skip version field until first space.
	i := 5
	for i < len(buf) && buf[i] != ' ' && buf[i] != '\t' {
		i++
	}
	// Skip whitespace.
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t') {
		i++
	}
	if i+3 > len(buf) {
		return 0, false
	}
	a, b, c := buf[i], buf[i+1], buf[i+2]
	if a < '1' || a > '9' || b < '0' || b > '9' || c < '0' || c > '9' {
		return 0, false
	}
	code := int(a-'0')*100 + int(b-'0')*10 + int(c-'0')
	return code, code >= 100 && code < 600
}

// parseContentLength extracts Content-Length from headers (case-insensitive).
func parseContentLength(buf []byte) (int, bool) {
	const header = "content-length:"
	const headerLen = len(header)

	for i := 0; i+headerLen <= len(buf); i++ {
		b := buf[i]
		if b != 'c' && b != 'C' {
			continue
		}
		match := true
		for j := 0; j < headerLen; j++ {
			ch := buf[i+j]
			if ch >= 'A' && ch <= 'Z' {
				ch += 32
			}
			if ch != header[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		start := i + headerLen
		for start < len(buf) && (buf[start] == ' ' || buf[start] == '\t') {
			start++
		}
		length := 0
		digits := 0
		for start < len(buf) {
			ch := buf[start]
			start++
			if ch >= '0' && ch <= '9' {
				length = length*10 + int(ch-'0')
				digits++
			} else if ch == '\r' || ch == '\n' {
				break
			} else if ch == ' ' || ch == '\t' {
				continue
			} else {
				return 0, false
			}
		}
		return length, digits > 0
	}
	return 0, false
}

// parseTransferEncoding returns true when Transfer-Encoding contains "chunked".
func parseTransferEncoding(buf []byte) bool {
	const header = "transfer-encoding:"
	const headerLen = len(header)

	for i := 0; i+headerLen <= len(buf); i++ {
		b := buf[i]
		if b != 't' && b != 'T' {
			continue
		}
		match := true
		for j := 0; j < headerLen; j++ {
			ch := buf[i+j]
			if ch >= 'A' && ch <= 'Z' {
				ch += 32
			}
			if ch != header[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		start := i + headerLen
		for start < len(buf) && (buf[start] == ' ' || buf[start] == '\t') {
			start++
		}
		end := start
		for end < len(buf) && buf[end] != '\r' && buf[end] != '\n' {
			end++
		}
		if containsChunked(buf[start:end]) {
			return true
		}
		i = end
	}
	return false
}

// containsChunked reports whether b contains the token "chunked" (case-insensitive).
func containsChunked(b []byte) bool {
	const tok = "chunked"
	if len(b) < 7 {
		return false
	}
	for i := 0; i+7 <= len(b); i++ {
		match := true
		for j := 0; j < 7; j++ {
			ch := b[i+j]
			if ch >= 'A' && ch <= 'Z' {
				ch += 32
			}
			if ch != tok[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// headerBytesFromRaw returns the first header value for key as a slice into
// raw (zero-copy). raw is the full response header block including status line.
// Returns nil if not found. Case-insensitive key match.
func headerBytesFromRaw(raw []byte, key string) []byte {
	if len(raw) == 0 || len(key) == 0 {
		return nil
	}
	// Skip status line (first \r\n).
	start := 0
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == '\r' && raw[i+1] == '\n' {
			start = i + 2
			break
		}
	}
	for start < len(raw) {
		lineEnd := -1
		for i := start; i+1 < len(raw); i++ {
			if raw[i] == '\r' && raw[i+1] == '\n' {
				lineEnd = i
				break
			}
		}
		if lineEnd < 0 || lineEnd == start {
			break
		}
		colonPos := -1
		for i := start; i < lineEnd; i++ {
			if raw[i] == ':' {
				colonPos = i
				break
			}
		}
		if colonPos > start && colonPos < lineEnd {
			keyStart, keyEnd := start, colonPos
			for keyStart < keyEnd && (raw[keyStart] == ' ' || raw[keyStart] == '\t') {
				keyStart++
			}
			for keyEnd > keyStart && (raw[keyEnd-1] == ' ' || raw[keyEnd-1] == '\t') {
				keyEnd--
			}
			// Case-insensitive compare of key with raw[keyStart:keyEnd].
			if keyEnd-keyStart == len(key) {
				match := true
				for i := 0; i < len(key); i++ {
					ca := raw[keyStart+i]
					if ca >= 'A' && ca <= 'Z' {
						ca += 32
					}
					cb := key[i]
					if cb >= 'A' && cb <= 'Z' {
						cb += 32
					}
					if ca != cb {
						match = false
						break
					}
				}
				if match {
					valueStart := colonPos + 1
					valueEnd := lineEnd
					for valueStart < valueEnd && (raw[valueStart] == ' ' || raw[valueStart] == '\t') {
						valueStart++
					}
					for valueEnd > valueStart && (raw[valueEnd-1] == ' ' || raw[valueEnd-1] == '\t') {
						valueEnd--
					}
					return raw[valueStart:valueEnd]
				}
			}
		}
		start = lineEnd + 2
	}
	return nil
}

// parseHeaders parses HTTP response headers from the header buffer.
// Keys and values are copied into Go strings so the caller can freely
// reuse or recycle buf after this call returns.
// Also stores a copy of headerBuf in resp.rawHeaderBuf for zero-copy HeaderBytes().
func parseHeaders(headerBuf []byte, resp *Response) {
	resp.rawHeaderBuf = make([]byte, len(headerBuf))
	copy(resp.rawHeaderBuf, headerBuf)
	// Find the end of the status line (first \r\n).
	statusLineEnd := -1
	for i := 0; i+1 < len(headerBuf); i++ {
		if headerBuf[i] == '\r' && headerBuf[i+1] == '\n' {
			statusLineEnd = i + 2
			break
		}
	}
	if statusLineEnd < 0 || statusLineEnd >= len(headerBuf) {
		return
	}

	resp.Headers = resp.Headers[:0]

	start := statusLineEnd
	for start < len(headerBuf) {
		lineEnd := -1
		for i := start; i+1 < len(headerBuf); i++ {
			if headerBuf[i] == '\r' && headerBuf[i+1] == '\n' {
				lineEnd = i
				break
			}
		}
		if lineEnd < 0 || lineEnd == start {
			break
		}

		colonPos := -1
		for i := start; i < lineEnd; i++ {
			if headerBuf[i] == ':' {
				colonPos = i
				break
			}
		}

		if colonPos > start && colonPos < lineEnd {
			keyStart, keyEnd := start, colonPos
			for keyStart < keyEnd && (headerBuf[keyStart] == ' ' || headerBuf[keyStart] == '\t') {
				keyStart++
			}
			for keyEnd > keyStart && (headerBuf[keyEnd-1] == ' ' || headerBuf[keyEnd-1] == '\t') {
				keyEnd--
			}

			valueStart, valueEnd := colonPos+1, lineEnd
			for valueStart < valueEnd && (headerBuf[valueStart] == ' ' || headerBuf[valueStart] == '\t') {
				valueStart++
			}
			for valueEnd > valueStart && (headerBuf[valueEnd-1] == ' ' || headerBuf[valueEnd-1] == '\t') {
				valueEnd--
			}

			if keyEnd > keyStart && valueEnd > valueStart {
				// string() copies the bytes — safe to recycle headerBuf later.
				key := string(headerBuf[keyStart:keyEnd])
				value := string(headerBuf[valueStart:valueEnd])
				resp.Headers = append(resp.Headers, Header{Key: key, Value: value})
			}
		}

		start = lineEnd + 2
	}
}
