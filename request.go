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

type Request struct {
	Method string
	URL    string // full URL; when set, Host/Port/TLS are derived and Path is ignored
	Path   string
	Body   []byte

	Compressed     bool
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	ExpectContinue bool

	ctx context.Context

	parsedHost string
	parsedPort int
	parsedTLS  bool
	parsedPath string
	urlParsed  bool

	headerBuf   []byte
	headerLen   int
	maxBodySize int

	// pre-encoded request line + headers; connection appends Content-Length + body
	PreEncodedHeaderPrefix []byte

	bodyBuf   []byte
	resp      *Response
	releaseFn func(*Response) // called by ResponseReader to release resp after cancelled pipeline request
}

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

func AcquireRequest() *Request {
	req := requestPool.Get().(*Request)
	req.Reset()
	return req
}

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

func (r *Request) WithMethod(method string) *Request {
	r.Method = method
	return r
}

func (r *Request) WithURL(rawURL string) *Request {
	r.URL = rawURL
	r.urlParsed = false
	return r
}

func (r *Request) WithPath(path string) *Request {
	r.Path = path
	return r
}

func (r *Request) WithBody(body []byte) *Request {
	r.Body = body
	return r
}

func (r *Request) WithHeader(key, value string) *Request {
	r.SetHeader(key, value)
	return r
}

func (r *Request) WithContext(ctx context.Context) *Request {
	r.ctx = ctx
	return r
}

func (r *Request) WithReadTimeout(d time.Duration) *Request {
	r.ReadTimeout = d
	return r
}

func (r *Request) WithWriteTimeout(d time.Duration) *Request {
	r.WriteTimeout = d
	return r
}

func (r *Request) WithCompression() *Request {
	r.Compressed = true
	return r
}

func (r *Request) WithExpectContinue() *Request {
	r.ExpectContinue = true
	return r
}

func (r *Request) SetContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *Request) Context() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

func containsCRLF(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return true
		}
	}
	return false
}

// returns false if key/value contain CR or LF (header injection guard)
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

func (r *Request) SetBody(body []byte) {
	r.Body = body
}

func (r *Request) resolveURL() (host string, port int, useTLS bool, path string, ok bool) {
	if r.urlParsed {
		return r.parsedHost, r.parsedPort, r.parsedTLS, r.parsedPath, r.parsedHost != ""
	}
	r.urlParsed = true

	raw := r.URL
	if raw == "" {
		return "", 0, false, "", false
	}

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
