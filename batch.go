package client

import (
	"context"
	"sync"
)

// BatchResult holds the outcome of a single request in a Batch call.
// Response is non-nil on success; Err is non-nil on failure.
// The caller must release each non-nil Response via Client.ReleaseResponse.
type BatchResult struct {
	Index    int
	Response *Response
	Err      error
}

// Batch accumulates requests for concurrent fan-out execution.
// Use it inside the closure passed to Client.Batch or Client.BatchWithContext.
// All helper methods (Get, Post, etc.) return *Batch for chaining.
type Batch struct {
	client  *Client
	entries []batchEntry
}

type batchEntry struct {
	req   *Request
	owned bool // acquired internally; auto-released after execution
}

// Do adds a caller-managed request to the batch.
// The caller retains ownership of req; do not release it until after the Batch call returns.
func (b *Batch) Do(req *Request) *Batch {
	b.entries = append(b.entries, batchEntry{req: req})
	return b
}

// Get adds a GET request to the batch.
func (b *Batch) Get(path string, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "GET"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// GetURL adds a GET request for a full URL to the batch.
func (b *Batch) GetURL(rawURL string, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "GET"
	req.URL = rawURL
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// Post adds a POST request to the batch.
func (b *Batch) Post(path string, body []byte, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "POST"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// PostURL adds a POST request for a full URL to the batch.
func (b *Batch) PostURL(rawURL string, body []byte, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "POST"
	req.URL = rawURL
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// Put adds a PUT request to the batch.
func (b *Batch) Put(path string, body []byte, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "PUT"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// Patch adds a PATCH request to the batch.
func (b *Batch) Patch(path string, body []byte, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "PATCH"
	req.Path = path
	req.Body = body
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// Delete adds a DELETE request to the batch.
func (b *Batch) Delete(path string, headers []Header) *Batch {
	req := b.client.acquireRequest()
	req.Method = "DELETE"
	req.Path = path
	for _, h := range headers {
		req.SetHeader(h.Key, h.Value)
	}
	b.entries = append(b.entries, batchEntry{req: req, owned: true})
	return b
}

// Batch executes all requests registered inside fn concurrently and returns
// their results indexed in the same order they were added.
// fn is called synchronously to build the request list; execution is concurrent.
// The caller must release each non-nil BatchResult.Response via Client.ReleaseResponse.
func (c *Client) Batch(fn func(*Batch)) []BatchResult {
	return c.BatchWithContext(context.Background(), fn)
}

// BatchWithContext executes all batch requests concurrently under the given context.
// Cancelling ctx cancels all in-flight requests in the batch.
func (c *Client) BatchWithContext(ctx context.Context, fn func(*Batch)) []BatchResult {
	if fn == nil {
		return nil
	}
	b := &Batch{client: c}
	fn(b)

	n := len(b.entries)
	if n == 0 {
		return nil
	}

	results := make([]BatchResult, n)
	var wg sync.WaitGroup
	wg.Add(n)

	for i, e := range b.entries {
		go func(idx int, entry batchEntry) {
			defer wg.Done()
			if entry.owned {
				defer c.releaseRequest(entry.req)
			}
			resp, err := c.DoWithContext(ctx, entry.req)
			results[idx] = BatchResult{Index: idx, Response: resp, Err: err}
		}(i, e)
	}

	wg.Wait()
	return results
}
