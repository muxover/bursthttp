package client

import (
	"io"
	"sync"
)

// Close must be called when done to release the connection back to the pool.
type StreamingResponse struct {
	StatusCode    int
	ContentLength int
	Headers       []Header
	Body          io.ReadCloser

	resp      *Response
	releaseFn func(*Response)
	once      sync.Once
}

func (sr *StreamingResponse) Header(key string) string {
	for _, h := range sr.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return h.Value
		}
	}
	return ""
}

func (sr *StreamingResponse) HasHeader(key string) bool {
	for _, h := range sr.Headers {
		if strEqualFoldASCII(h.Key, key) {
			return true
		}
	}
	return false
}

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
