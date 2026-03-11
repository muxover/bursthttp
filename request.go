package client

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Initial buffer sizes — tuned for high-RPS workloads with small requests.
	initialRequestBodyBufferSize = 32 * 1024
	initialHeaderBufferSize      = 16 * 1024
)

// Request represents an HTTP request with pre-allocated buffers.
// Acquire via Client.AcquireRequest() or the package-level AcquireRequest().
// Release after Do() returns via Client.ReleaseRequest() or ReleaseRequest().
type Request struct {
	// Method is the HTTP verb (GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS).
	Method string

	// URL is a full URL e.g. "https://api.example.com/v1/users".
	// When set, Host/Port/TLS are derived from it; Path is ignored.
	URL string

	// Path is the request path used when URL is empty (e.g. "/api/v1/users").
	// Deprecated in favour of URL for multi-host clients.
	Path string

	// Body is the raw request body.
	Body []byte

	// Compressed, if true, gzip-compresses Body before sending
	// (requires Compressor configured in Client).
	Compressed bool

	// Per-request timeout overrides (0 = use client config).
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// ExpectContinue, when true, sends "Expect: 100-continue" and waits
	// for a 100 response before transmitting the body.
	ExpectContinue bool

	// ctx for cancellation.
	ctx context.Context

	// Parsed URL fields (populated lazily by resolveURL).
	parsedHost string
	parsedPort int
	parsedTLS  bool
	parsedPath string
	urlParsed  bool

	// Internal wire-format header buffer.
	// Use SetHeader / WithHeader to populate it.
	headerBuf   []byte
	headerLen   int
	maxBodySize int

	// PreEncodedHeaderPrefix, when non-nil, is the pre-encoded request header
	// (request line + all headers) up to but not including "Content-Length: N\r\n\r\n".
	// The connection will append Content-Length and body. Build via BuildPreEncodedHeaderPrefix.
	PreEncodedHeaderPrefix []byte

	// bodyBuf is the internal scratch buffer for body compression.
	bodyBuf []byte

	// resp is set by Client.DoWithContext before calling conn.Do.
	resp *Response

	// releaseFn is set by Client.DoWithContext. It is called by the ResponseReader
	// goroutine to return resp to the pool after a pipelined request is cancelled.
	releaseFn func(*Response)
}

// Header is a single HTTP header key-value pair.
type Header struct {
	Key   string
	Value string
}

var requestPool = sync.Pool{
	New: func() interface{} {
		return &Request{
			headerBuf: make([]byte, initialHeaderBufferSize),
			bodyBuf:   make([]byte, initialRequestBodyBufferSize),
		}
	},
}

// AcquireRequest gets a Request from the global pool.
func AcquireRequest() *Request {
	req := requestPool.Get().(*Request)
	req.Reset()
	return req
}

// ReleaseRequest returns req to the global pool.
func ReleaseRequest(req *Request) {
	if req == nil {
		return
	}
	if len(req.bodyBuf) > initialRequestBodyBufferSize*2 {
		req.bodyBuf = make([]byte, initialRequestBodyBufferSize)
	}
	if len(req.headerBuf) > initialHeaderBufferSize*2 {
		req.headerBuf = make([]byte, initialHeaderBufferSize)
	}
	requestPool.Put(req)
}

// Reset clears the request for reuse, preserving underlying buffer capacity.
func (r *Request) Reset() {
	r.Method = ""
	r.URL = ""
	r.Path = ""
	r.Body = nil
	r.Compressed = false
	r.ExpectContinue = false
	r.ReadTimeout = 0
	r.WriteTimeout = 0
	r.ctx = nil
	r.resp = nil
	r.releaseFn = nil
	r.headerLen = 0
	r.urlParsed = false
	r.parsedHost = ""
	r.parsedPort = 0
	r.parsedTLS = false
	r.parsedPath = ""
	r.PreEncodedHeaderPrefix = nil
}

// WithMethod sets the HTTP method and returns r for chaining.
func (r *Request) WithMethod(method string) *Request {
	r.Method = method
	return r
}

// WithURL sets the full URL and returns r for chaining.
// Example: req.WithURL("https://api.example.com/v1/items")
func (r *Request) WithURL(rawURL string) *Request {
	r.URL = rawURL
	r.urlParsed = false
	return r
}

// WithPath sets the request path (used when URL is empty) and returns r.
func (r *Request) WithPath(path string) *Request {
	r.Path = path
	return r
}

// WithBody sets the request body and returns r for chaining.
func (r *Request) WithBody(body []byte) *Request {
	r.Body = body
	return r
}

// WithHeader adds a header in wire format and returns r for chaining.
// Returns r unchanged if the buffer is full (grows automatically up to 1 MB).
func (r *Request) WithHeader(key, value string) *Request {
	r.SetHeader(key, value)
	return r
}

// WithContext sets the cancellation context and returns r for chaining.
func (r *Request) WithContext(ctx context.Context) *Request {
	r.ctx = ctx
	return r
}

// WithReadTimeout overrides the client read timeout for this request.
func (r *Request) WithReadTimeout(d time.Duration) *Request {
	r.ReadTimeout = d
	return r
}

// WithWriteTimeout overrides the client write timeout for this request.
func (r *Request) WithWriteTimeout(d time.Duration) *Request {
	r.WriteTimeout = d
	return r
}

// WithCompression enables gzip compression of the request body.
func (r *Request) WithCompression() *Request {
	r.Compressed = true
	return r
}

// WithExpectContinue enables the Expect: 100-continue protocol.
// The client sends headers first, waits for a 100 response, then sends the body.
func (r *Request) WithExpectContinue() *Request {
	r.ExpectContinue = true
	return r
}

// SetContext sets the context for the request.
func (r *Request) SetContext(ctx context.Context) {
	r.ctx = ctx
}

// Context returns the request's context (context.Background() if nil).
func (r *Request) Context() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// containsCRLF reports whether s contains CR or LF (used to prevent header injection).
func containsCRLF(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return true
		}
	}
	return false
}

// SetHeader appends a header in wire format ("Key: Value\r\n") to the
// internal buffer. Returns false if key/value contain CR or LF (header
// injection protection) or if the buffer cannot grow beyond 1 MB.
func (r *Request) SetHeader(key, value string) bool {
	if containsCRLF(key) || containsCRLF(value) {
		return false
	}
	required := len(key) + 2 + len(value) + 2 // "Key: Value\r\n"
	if r.headerLen+required > len(r.headerBuf) {
		if !r.ensureHeaderBufferSize(r.headerLen + required) {
			return false
		}
	}
	pos := r.headerLen
	copy(r.headerBuf[pos:], key)
	pos += len(key)
	r.headerBuf[pos] = ':'
	pos++
	r.headerBuf[pos] = ' '
	pos++
	copy(r.headerBuf[pos:], value)
	pos += len(value)
	r.headerBuf[pos] = '\r'
	pos++
	r.headerBuf[pos] = '\n'
	pos++
	r.headerLen = pos
	return true
}

// SetBody sets the request body.
func (r *Request) SetBody(body []byte) {
	r.Body = body
}

// resolveURL parses r.URL and caches the result.
// Returns (host, port, useTLS, path, ok).
func (r *Request) resolveURL() (host string, port int, useTLS bool, path string, ok bool) {
	if r.urlParsed {
		return r.parsedHost, r.parsedPort, r.parsedTLS, r.parsedPath, r.parsedHost != ""
	}
	r.urlParsed = true

	raw := r.URL
	if raw == "" {
		return "", 0, false, "", false
	}

	// Require a scheme.
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, false, "", false
	}

	switch strings.ToLower(u.Scheme) {
	case "https":
		useTLS = true
		port = 443
	case "http":
		useTLS = false
		port = 80
	default:
		return "", 0, false, "", false
	}

	hostname := u.Hostname()
	if hostname == "" {
		return "", 0, false, "", false
	}

	if ps := u.Port(); ps != "" {
		if p, err := strconv.Atoi(ps); err == nil && p > 0 {
			port = p
		}
	}

	path = u.RequestURI() // includes path + query string
	if path == "" {
		path = "/"
	}

	r.parsedHost = hostname
	r.parsedPort = port
	r.parsedTLS = useTLS
	r.parsedPath = path
	return hostname, port, useTLS, path, true
}

// effectivePath returns the wire path for the request.
// Prefers parsedPath (from URL), then r.Path, then "/".
func (r *Request) effectivePath() string {
	if r.parsedPath != "" {
		return r.parsedPath
	}
	if r.Path != "" {
		return r.Path
	}
	return "/"
}

func (r *Request) ensureBodyBufferSize(required int) bool {
	if required <= len(r.bodyBuf) {
		return true
	}
	if r.maxBodySize > 0 && required > r.maxBodySize {
		return false
	}
	newSize := len(r.bodyBuf) * 2
	if newSize < required {
		newSize = required
	}
	if newSize < 1024*1024 {
		if newSize%4096 != 0 {
			newSize = ((newSize / 4096) + 1) * 4096
		}
	} else {
		if newSize%65536 != 0 {
			newSize = ((newSize / 65536) + 1) * 65536
		}
	}
	if r.maxBodySize > 0 && newSize > r.maxBodySize {
		newSize = r.maxBodySize
		if newSize < required {
			return false
		}
	}
	r.bodyBuf = make([]byte, newSize)
	return true
}

func (r *Request) ensureHeaderBufferSize(required int) bool {
	if required <= len(r.headerBuf) {
		return true
	}
	newSize := len(r.headerBuf) * 2
	if newSize < required {
		newSize = required
	}
	if newSize%4096 != 0 {
		newSize = ((newSize / 4096) + 1) * 4096
	}
	const maxHeaderSize = 1024 * 1024
	if newSize > maxHeaderSize {
		newSize = maxHeaderSize
		if newSize < required {
			return false
		}
	}
	newBuf := make([]byte, newSize)
	copy(newBuf, r.headerBuf[:r.headerLen])
	r.headerBuf = newBuf
	return true
}
