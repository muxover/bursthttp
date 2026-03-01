package client

import (
	"io"
	"sync"
)

// StreamingResponse wraps a Response with an io.ReadCloser for the body.
// Use Client.DoStreaming to obtain one. The caller MUST call Close() when done.
type StreamingResponse struct {
	StatusCode    int
	ContentLength int
	Headers       []Header
	Body          io.ReadCloser

	resp      *Response
	releaseFn func(*Response)
	once      sync.Once
}

// Header returns the first value for the given header key (case-insensitive).
func (sr *StreamingResponse) Header(key string) string {
	for _, h := range sr.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

// HasHeader reports whether the streaming response contains the named header.
func (sr *StreamingResponse) HasHeader(key string) bool {
	for _, h := range sr.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return true
		}
	}
	return false
}

// Close releases the underlying response back to the pool.
// Must be called after the body has been fully consumed or abandoned.
func (sr *StreamingResponse) Close() error {
	sr.once.Do(func() {
		if sr.Body != nil {
			sr.Body.Close()
		}
		if sr.releaseFn != nil && sr.resp != nil {
			sr.releaseFn(sr.resp)
		}
	})
	return nil
}

// bodyReader wraps a []byte as an io.ReadCloser.
type bodyReader struct {
	data   []byte
	offset int
}

func newBodyReader(data []byte) *bodyReader {
	return &bodyReader{data: data}
}

func (r *bodyReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *bodyReader) Close() error {
	return nil
}
