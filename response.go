package client

import (
	"sync"
)

const (
	initialResponseBufferSize = 64 * 1024
)

// Response represents an HTTP response with pre-allocated body buffer.
type Response struct {
	StatusCode    int
	ContentLength int
	Body          []byte
	Headers       []Header

	// Internal fields
	bodyBuf     []byte
	maxSize     int   // Maximum allowed size from config
	rawHeaderBuf []byte // Copy of raw header block for zero-copy HeaderBytes(); valid until ReleaseResponse
}

// AcquireResponse gets a response from the pool.
func AcquireResponse() *Response {
	resp := responsePool.Get().(*Response)
	resp.Reset()
	return resp
}

// AcquireResponseWithMaxSize gets a response from the pool with a maximum size limit.
// This allows per-client configuration while maintaining pool efficiency.
func AcquireResponseWithMaxSize(maxSize int) *Response {
	resp := AcquireResponse()
	if maxSize > 0 {
		resp.maxSize = maxSize
		// Ensure buffer is large enough but not larger than maxSize
		if len(resp.bodyBuf) < initialResponseBufferSize {
			resp.bodyBuf = make([]byte, initialResponseBufferSize)
		}
		if len(resp.bodyBuf) > maxSize {
			// Shrink if buffer is too large (but keep minimum initial size)
			if maxSize >= initialResponseBufferSize {
				resp.bodyBuf = make([]byte, maxSize)
			}
		}
	}
	return resp
}

// ensureBufferSize ensures the buffer is at least the required size.
// Optimized for high RPS - minimizes allocations and rounding overhead.
func (r *Response) ensureBufferSize(required int) bool {
	if required <= len(r.bodyBuf) {
		return true
	}
	if r.maxSize > 0 && required > r.maxSize {
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
	if r.maxSize > 0 && newSize > r.maxSize {
		newSize = r.maxSize
		if newSize < required {
			return false
		}
	}

	r.bodyBuf = make([]byte, newSize)
	return true
}

// ReleaseResponse returns a response to the pool.
func ReleaseResponse(resp *Response) {
	if resp == nil {
		return
	}
	if len(resp.bodyBuf) > initialResponseBufferSize*2 {
		resp.bodyBuf = make([]byte, initialResponseBufferSize)
	}
	if len(resp.rawHeaderBuf) > 4096 {
		resp.rawHeaderBuf = nil
	}
	responsePool.Put(resp)
}

// Reset clears the response for reuse.
func (r *Response) Reset() {
	r.StatusCode = 0
	r.ContentLength = 0
	r.Body = nil
	r.Headers = r.Headers[:0]
	r.maxSize = 0
	r.rawHeaderBuf = nil
}

// Header returns the first value for the given header key (case-insensitive).
// Returns empty string if the header is not present.
func (r *Response) Header(key string) string {
	for _, h := range r.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

// HasHeader reports whether the response contains the named header (case-insensitive).
func (r *Response) HasHeader(key string) bool {
	for _, h := range r.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return true
		}
	}
	return false
}

// HeaderValues returns all values for the given header key (case-insensitive).
// Returns nil if the header is not present.
func (r *Response) HeaderValues(key string) []string {
	var vals []string
	for _, h := range r.Headers {
		if strEqualFoldASCII(h.Key, key) {
			vals = append(vals, h.Value)
		}
	}
	return vals
}

// HeaderBytes returns the first value for the given header key as a slice into
// the response's raw header buffer (zero-copy). The slice is valid only until
// ReleaseResponse is called. Returns nil if the header is not present.
// Case-insensitive header name match.
func (r *Response) HeaderBytes(key string) []byte {
	return headerBytesFromRaw(r.rawHeaderBuf, key)
}

// isConnectionClose returns true if the response has a Connection: close header.
func (r *Response) isConnectionClose() bool {
	for _, h := range r.Headers {
		if strEqualFoldASCII(h.Key, "Connection") && strEqualFoldASCII(h.Value, "close") {
			return true
		}
	}
	return false
}

// strEqualFoldASCII is a fast ASCII-only case-insensitive string comparison.
func strEqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// responsePool is a sync.Pool for response objects.
var responsePool = sync.Pool{
	New: func() interface{} {
		return &Response{
			bodyBuf: make([]byte, initialResponseBufferSize), // 64KB initial instead of 10MB
			Headers: make([]Header, 0, 16),
		}
	},
}
