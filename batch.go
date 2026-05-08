package client

import (
	"context"
	"sync"
)

// Release each non-nil Response via Client.ReleaseResponse.
type BatchResult struct {
	Index    int
	Response *Response
	Err      error
}

type Batch struct {
	client  *Client
	entries []batchEntry
}

type batchEntry struct {
	req   *Request
	owned bool // acquired internally; auto-released after execution
}

func (b *Batch) Do(req *Request) *Batch {
	b.entries = append(b.entries, batchEntry{req: req})
	return b
}

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

func (c *Client) Batch(fn func(*Batch)) []BatchResult {
	return c.BatchWithContext(context.Background(), fn)
}

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
