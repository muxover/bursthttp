package client

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testServer(t testing.TB, handler http.Handler) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(handler)
	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)
	return srv, host, port
}

func testClient(t testing.TB, host string, port int) *Client {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 8
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

// --- Parser ---

func TestParseStatusCode(t *testing.T) {
	cases := []struct {
		input string
		want  int
		ok    bool
	}{
		{"HTTP/1.1 200 OK\r\n", 200, true},
		{"HTTP/1.0 404 Not Found\r\n", 404, true},
		{"HTTP/1.1 500 Internal Server Error\r\n", 500, true},
		{"HTTP/1.1 204 No Content\r\n", 204, true},
		{"HTTP/2 200 OK\r\n", 200, true},
		{"GARBAGE", 0, false},
		{"HTTP/1.1 99 Too Low\r\n", 0, false},
	}
	for _, tc := range cases {
		code, ok := parseStatusCode([]byte(tc.input))
		if ok != tc.ok || code != tc.want {
			t.Errorf("parseStatusCode(%q) = (%d, %v), want (%d, %v)",
				tc.input, code, ok, tc.want, tc.ok)
		}
	}
}

func TestParseContentLength(t *testing.T) {
	cases := []struct {
		input string
		want  int
		ok    bool
	}{
		{"HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n", 42, true},
		{"HTTP/1.1 200 OK\r\ncontent-length: 0\r\n\r\n", 0, true},
		{"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n", 0, false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\n\r\n", 1048576, true},
	}
	for _, tc := range cases {
		length, ok := parseContentLength([]byte(tc.input))
		if ok != tc.ok || length != tc.want {
			t.Errorf("parseContentLength(%q) = (%d, %v), want (%d, %v)",
				tc.input, length, ok, tc.want, tc.ok)
		}
	}
}

func TestParseTransferEncoding(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n", true},
		{"HTTP/1.1 200 OK\r\ntransfer-encoding: CHUNKED\r\n\r\n", true},
		{"HTTP/1.1 200 OK\r\nTransfer-Encoding: identity\r\n\r\n", false},
		{"HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n", false},
	}
	for _, tc := range cases {
		got := parseTransferEncoding([]byte(tc.input))
		if got != tc.want {
			t.Errorf("parseTransferEncoding(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestFindHeaderEnd(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"HTTP/1.1 200 OK\r\n\r\n", 19},
		{"HTTP/1.1 200 OK\r\nFoo: bar\r\n\r\n", 29},
		{"incomplete", -1},
		{"no end\r\n", -1},
	}
	for _, tc := range cases {
		got := findHeaderEnd([]byte(tc.input))
		if got != tc.want {
			t.Errorf("findHeaderEnd(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nX-Custom: value\r\n\r\n"
	resp := &Response{
		bodyBuf: make([]byte, 64),
		Headers: make([]Header, 0, 4),
	}
	parseHeaders([]byte(raw), resp)
	if len(resp.Headers) != 2 {
		t.Fatalf("expected 2 headers, got %d", len(resp.Headers))
	}
	if resp.Headers[0].Key != "Content-Type" || resp.Headers[0].Value != "application/json" {
		t.Errorf("header[0] = %+v", resp.Headers[0])
	}
	if resp.Headers[1].Key != "X-Custom" || resp.Headers[1].Value != "value" {
		t.Errorf("header[1] = %+v", resp.Headers[1])
	}
}

func TestParseHeadersBufferSafety(t *testing.T) {
	buf := []byte("HTTP/1.1 200 OK\r\nX-Safe: original\r\n\r\n")
	resp := &Response{bodyBuf: make([]byte, 64), Headers: make([]Header, 0, 4)}
	parseHeaders(buf, resp)

	if len(resp.Headers) == 0 {
		t.Fatal("no headers parsed")
	}
	copy(buf, bytes.Repeat([]byte("X"), len(buf)))

	if resp.Headers[0].Value != "original" {
		t.Errorf("header value corrupted: %q", resp.Headers[0].Value)
	}
}

func TestReadChunkedBody(t *testing.T) {
	var raw bytes.Buffer
	raw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	raw.WriteString("5\r\nhello\r\n")
	raw.WriteString("6\r\n world\r\n")
	raw.WriteString("0\r\n\r\n")

	conn := newFakeConn(raw.Bytes())
	buf := make([]byte, 4096)
	resp := &Response{bodyBuf: make([]byte, 4096), Headers: make([]Header, 0, 8)}

	err := readResponse(conn, buf, resp, 1<<20, "GET", 4096, 4096)
	if err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != "hello world" {
		t.Errorf("body = %q, want %q", resp.Body, "hello world")
	}
	if resp.ContentLength != 11 {
		t.Errorf("ContentLength = %d, want 11", resp.ContentLength)
	}
}

func TestReadChunkedBodyLarge(t *testing.T) {
	chunk := bytes.Repeat([]byte("A"), 1000)
	var raw bytes.Buffer
	raw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&raw, "%x\r\n", len(chunk))
		raw.Write(chunk)
		raw.WriteString("\r\n")
	}
	raw.WriteString("0\r\n\r\n")

	conn := newFakeConn(raw.Bytes())
	buf := make([]byte, 4096)
	resp := &Response{bodyBuf: make([]byte, 512), Headers: make([]Header, 0, 4)}

	err := readResponse(conn, buf, resp, 1<<20, "GET", 4096, 4096)
	if err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if len(resp.Body) != 10000 {
		t.Errorf("body length = %d, want 10000", len(resp.Body))
	}
}

// --- Request ---

func TestRequestURLParsing(t *testing.T) {
	cases := []struct {
		rawURL   string
		wantHost string
		wantPort int
		wantTLS  bool
		wantPath string
		wantOK   bool
	}{
		{"https://api.example.com/v1/users", "api.example.com", 443, true, "/v1/users", true},
		{"http://localhost:8080/path?q=1", "localhost", 8080, false, "/path?q=1", true},
		{"https://host.com:9443/", "host.com", 9443, true, "/", true},
		{"http://example.com", "example.com", 80, false, "/", true},
		{"", "", 0, false, "", false},
	}
	for _, tc := range cases {
		req := &Request{URL: tc.rawURL}
		host, port, tls, path, ok := req.resolveURL()
		if ok != tc.wantOK {
			t.Errorf("resolveURL(%q) ok=%v, want %v", tc.rawURL, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if host != tc.wantHost || port != tc.wantPort || tls != tc.wantTLS || path != tc.wantPath {
			t.Errorf("resolveURL(%q) = (%q, %d, %v, %q), want (%q, %d, %v, %q)",
				tc.rawURL, host, port, tls, path,
				tc.wantHost, tc.wantPort, tc.wantTLS, tc.wantPath)
		}
	}
}

func TestRequestReset(t *testing.T) {
	req := AcquireRequest()
	req.Method = "POST"
	req.URL = "https://example.com/test"
	req.Path = "/test"
	req.Body = []byte("data")
	req.Compressed = true
	req.ExpectContinue = true
	req.ReadTimeout = 5 * time.Second
	req.WriteTimeout = 5 * time.Second
	req.SetHeader("X-Foo", "bar")

	req.Reset()

	if req.Method != "" || req.URL != "" || req.Path != "" {
		t.Error("Reset should clear method/url/path")
	}
	if req.Body != nil || req.Compressed || req.ExpectContinue {
		t.Error("Reset should clear body/compressed/expect")
	}
	if req.ReadTimeout != 0 || req.WriteTimeout != 0 {
		t.Error("Reset should clear timeouts")
	}
	if req.headerLen != 0 {
		t.Error("Reset should clear header buffer")
	}
	ReleaseRequest(req)
}

func TestRequestPoolReuse(t *testing.T) {
	req1 := AcquireRequest()
	req1.Method = "POST"
	req1.Path = "/test"
	req1.SetHeader("X-Foo", "bar")
	ReleaseRequest(req1)

	req2 := AcquireRequest()
	defer ReleaseRequest(req2)

	if req2.Method != "" || req2.Path != "" || req2.headerLen != 0 {
		t.Error("pooled request not properly reset")
	}
}

func TestHeaderInjectionBlocked(t *testing.T) {
	req := AcquireRequest()
	defer ReleaseRequest(req)

	if req.SetHeader("X-Bad\r\nEvil", "value") {
		t.Error("SetHeader should reject key with CR/LF")
	}
	if req.SetHeader("X-Good", "value\r\nX-Injected: pwned") {
		t.Error("SetHeader should reject value with CR/LF")
	}
	if !req.SetHeader("X-Good", "clean-value") {
		t.Error("SetHeader should accept clean key/value")
	}
}

// --- Response ---

func TestResponseHeader(t *testing.T) {
	resp := &Response{
		bodyBuf: make([]byte, 64),
		Headers: []Header{
			{Key: "Content-Type", Value: "application/json"},
			{Key: "X-Request-Id", Value: "abc123"},
			{Key: "Set-Cookie", Value: "a=1"},
			{Key: "Set-Cookie", Value: "b=2"},
		},
	}

	if got := resp.Header("Content-Type"); got != "application/json" {
		t.Errorf("Header(Content-Type) = %q, want %q", got, "application/json")
	}
	if got := resp.Header("content-type"); got != "application/json" {
		t.Errorf("Header(content-type) = %q, want %q", got, "application/json")
	}
	if got := resp.Header("X-Missing"); got != "" {
		t.Errorf("Header(X-Missing) = %q, want empty", got)
	}
	if !resp.HasHeader("x-request-id") {
		t.Error("HasHeader(x-request-id) = false, want true")
	}
	if resp.HasHeader("X-Missing") {
		t.Error("HasHeader(X-Missing) = true, want false")
	}
	vals := resp.HeaderValues("set-cookie")
	if len(vals) != 2 {
		t.Fatalf("HeaderValues(set-cookie) len = %d, want 2", len(vals))
	}
	if vals[0] != "a=1" || vals[1] != "b=2" {
		t.Errorf("HeaderValues(set-cookie) = %v", vals)
	}
}

func TestResponsePoolReuse(t *testing.T) {
	resp := AcquireResponse()
	resp.StatusCode = 200
	resp.Headers = append(resp.Headers, Header{Key: "X-Test", Value: "val"})
	ReleaseResponse(resp)

	resp2 := AcquireResponse()
	defer ReleaseResponse(resp2)
	if resp2.StatusCode != 0 {
		t.Error("pooled response should have StatusCode 0")
	}
	if len(resp2.Headers) != 0 {
		t.Error("pooled response should have empty headers")
	}
}

func TestIsConnectionClose(t *testing.T) {
	cases := []struct {
		headers []Header
		want    bool
	}{
		{[]Header{{Key: "Connection", Value: "close"}}, true},
		{[]Header{{Key: "connection", Value: "Close"}}, true},
		{[]Header{{Key: "Connection", Value: "keep-alive"}}, false},
		{[]Header{}, false},
		{[]Header{{Key: "X-Foo", Value: "close"}}, false},
	}
	for _, tc := range cases {
		resp := &Response{Headers: tc.headers, bodyBuf: make([]byte, 1)}
		got := resp.isConnectionClose()
		if got != tc.want {
			t.Errorf("isConnectionClose(%v) = %v, want %v", tc.headers, got, tc.want)
		}
	}
}

// --- Client: basic operations ---

func TestClientGet(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("pong"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != "pong" {
		t.Errorf("body = %q, want %q", resp.Body, "pong")
	}
}

func TestClientPost(t *testing.T) {
	var received []byte
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	resp, err := c.Post("/", []byte("hello"), []Header{{"Content-Type", "text/plain"}})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if string(received) != "hello" {
		t.Errorf("server received %q, want %q", received, "hello")
	}
}

func TestClientGetURL(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("url-routed"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = "example.com"
	cfg.Port = 80
	cfg.UseTLS = false
	cfg.EnableLogging = false
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	rawURL := fmt.Sprintf("http://%s:%d/", host, port)
	resp, err := c.GetURL(rawURL, nil)
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != "url-routed" {
		t.Errorf("body = %q, want %q", resp.Body, "url-routed")
	}
}

func TestHTTPMethods(t *testing.T) {
	var lastMethodMu sync.Mutex
	var lastMethod string
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethodMu.Lock()
		lastMethod = r.Method
		lastMethodMu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)

	for _, m := range []struct {
		fn   func(string, []Header) (*Response, error)
		name string
	}{
		{c.Get, "GET"},
		{c.Delete, "DELETE"},
		{c.Head, "HEAD"},
		{c.Options, "OPTIONS"},
	} {
		resp, err := m.fn("/", nil)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		c.ReleaseResponse(resp)
		lastMethodMu.Lock()
		got := lastMethod
		lastMethodMu.Unlock()
		if got != m.name {
			t.Errorf("expected method %s, got %s", m.name, got)
		}
	}

	for _, m := range []struct {
		fn   func(string, []byte, []Header) (*Response, error)
		name string
	}{
		{c.Put, "PUT"},
		{c.Patch, "PATCH"},
	} {
		resp, err := m.fn("/", []byte("data"), nil)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		c.ReleaseResponse(resp)
		lastMethodMu.Lock()
		got := lastMethod
		lastMethodMu.Unlock()
		if got != m.name {
			t.Errorf("expected method %s, got %s", m.name, got)
		}
	}
}

func TestClientFluentBuilder(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "fluent" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	req := c.AcquireRequest().
		WithMethod("GET").
		WithPath("/").
		WithHeader("X-Test", "fluent")
	defer c.ReleaseRequest(req)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestClientContextCancellation(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := c.AcquireRequest()
	req.Method = "GET"
	req.Path = "/"
	defer c.ReleaseRequest(req)

	_, err := c.DoWithContext(ctx, req)
	if err == nil {
		t.Fatal("expected error due to context timeout, got nil")
	}
}

func TestDoWithContextOverridesReqCtx(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := testClient(t, host, port)

	req := c.AcquireRequest()
	req.WithMethod("GET").WithPath("/").WithContext(context.Background())
	defer c.ReleaseRequest(req)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.DoWithContext(ctx, req)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestDoWithContextNilRequest(t *testing.T) {
	cfg := DefaultConfig()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	_, err = c.DoWithContext(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil request")
	}
}

func TestDoWithInvalidURL(t *testing.T) {
	cfg := DefaultConfig()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	req := c.AcquireRequest()
	defer c.ReleaseRequest(req)
	req.Method = "GET"
	req.URL = "://missing-scheme"

	_, err = c.DoWithContext(context.Background(), req)
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

// --- Chunked / Gzip ---

func TestClientChunkedResponse(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("chunk1"))
		flusher.Flush()
		w.Write([]byte("chunk2"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(resp.Body), "chunk") {
		t.Errorf("body missing chunks: %q", resp.Body)
	}
}

func TestGzipResponseDecompression(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		gz.Write([]byte("hello compressed world"))
		gz.Close()
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if string(resp.Body) != "hello compressed world" {
		t.Errorf("body = %q, want %q", resp.Body, "hello compressed world")
	}
}

// --- Pipelining ---

func TestClientPipelining(t *testing.T) {
	const requests = 30

	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = true
	cfg.MaxPipelinedRequests = 8
	cfg.PoolSize = 8
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	cfg.EnableLogging = false
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	var wg sync.WaitGroup
	var ok atomic.Int64
	var errCount atomic.Int64

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req := c.AcquireRequest()
			req.Method = "GET"
			req.Path = "/"
			resp, err := c.DoWithContext(ctx, req)
			c.ReleaseRequest(req)
			if err != nil {
				errCount.Add(1)
				return
			}
			c.ReleaseResponse(resp)
			ok.Add(1)
		}()
	}
	wg.Wait()

	if errCount.Load() > 0 {
		t.Errorf("pipelining: %d/%d requests failed", errCount.Load(), requests)
	}
}

func TestPipelineCancelNoRace(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte("delayed"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = true
	cfg.MaxPipelinedRequests = 4
	cfg.PoolSize = 4
	cfg.ReadTimeout = 2 * time.Second
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			req := c.AcquireRequest()
			defer c.ReleaseRequest(req)
			req.Method = "GET"
			req.Path = "/"
			_, _ = c.DoWithContext(ctx, req)
		}()
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)
}

// --- Connection management ---

func TestConnectionCloseDetection(t *testing.T) {
	var reqCount atomic.Int32
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n == 1 {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	c.ReleaseResponse(resp)

	resp, err = c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStartWarmUp(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.PoolSize = 8
	cfg.EnablePipelining = false
	cfg.EnableLogging = false

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	err = c.StartN(4)
	if err != nil {
		t.Fatalf("StartN: %v", err)
	}

	healthy := c.GetHealthyConnections()
	if healthy < 4 {
		t.Errorf("healthy connections = %d, want >= 4", healthy)
	}
}

func TestGracefulStop(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 4
	cfg.EnableLogging = false

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.ReleaseResponse(resp)

	drained := c.GracefulStop(5 * time.Second)
	if !drained {
		t.Error("GracefulStop did not drain cleanly")
	}
}

func TestIdleEviction(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.PoolSize = 8
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.IdleTimeout = 200 * time.Millisecond
	cfg.IdleCheckInterval = 100 * time.Millisecond

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.ReleaseResponse(resp)

	before := c.GetHealthyConnections()
	if before == 0 {
		t.Fatal("expected at least 1 healthy connection")
	}

	time.Sleep(500 * time.Millisecond)

	after := c.GetHealthyConnections()
	if after >= before {
		t.Errorf("idle eviction did not reduce connections: before=%d, after=%d", before, after)
	}
}

func TestHTTPSURLRouting(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("tls-ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 80
	cfg.UseTLS = false
	cfg.EnableLogging = false
	cfg.TLSConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	resp, err := c.GetURL(srv.URL, nil)
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	defer c.ReleaseResponse(resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != "tls-ok" {
		t.Errorf("body = %q, want %q", resp.Body, "tls-ok")
	}
}

// --- Expect: 100-continue ---

func TestExpectContinue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 4

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	req := c.AcquireRequest()
	req.WithMethod("POST").
		WithPath("/upload").
		WithBody([]byte("big body data")).
		WithExpectContinue()
	defer c.ReleaseRequest(req)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer c.ReleaseResponse(resp)

	if string(resp.Body) != "ok" {
		t.Errorf("body = %q, want %q", resp.Body, "ok")
	}
}

// --- Streaming ---

func TestDoStreaming(t *testing.T) {
	body := "streaming response body data"
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := testClient(t, host, port)

	req := c.AcquireRequest()
	req.Method = "GET"
	req.Path = "/"

	sr, err := c.DoStreaming(context.Background(), req)
	if err != nil {
		t.Fatalf("DoStreaming: %v", err)
	}
	defer sr.Close()

	data, err := io.ReadAll(sr.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != body {
		t.Errorf("body = %q, want %q", data, body)
	}
	if sr.StatusCode != 200 {
		t.Errorf("status = %d, want 200", sr.StatusCode)
	}
	c.ReleaseRequest(req)
}

func TestStreamingResponseHeader(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "streamed")
		w.WriteHeader(200)
		w.Write([]byte("streaming data"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)
	req := c.AcquireRequest()
	req.Method = "GET"
	req.Path = "/"

	sr, err := c.DoStreaming(context.Background(), req)
	if err != nil {
		t.Fatalf("DoStreaming: %v", err)
	}
	defer sr.Close()

	if sr.Header("X-Custom") != "streamed" {
		t.Errorf("Header(X-Custom) = %q, want 'streamed'", sr.Header("X-Custom"))
	}
	if !sr.HasHeader("x-custom") {
		t.Error("HasHeader(x-custom) should be true")
	}
	c.ReleaseRequest(req)
}

func TestDoReader(t *testing.T) {
	var received []byte
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := testClient(t, host, port)

	bodyData := "hello from reader"
	reader := strings.NewReader(bodyData)
	resp, err := c.DoReader(context.Background(), "POST", "/", reader, int64(len(bodyData)),
		[]Header{{Key: "Content-Type", Value: "text/plain"}})
	if err != nil {
		t.Fatalf("DoReader: %v", err)
	}
	c.ReleaseResponse(resp)

	if string(received) != bodyData {
		t.Errorf("server received %q, want %q", received, bodyData)
	}
}

func TestBodyReader(t *testing.T) {
	data := []byte("hello world streaming body")
	r := newBodyReader(data)

	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Errorf("Read = (%d, %q), want (5, %q)", n, buf, "hello")
	}

	all, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(all) != " world streaming body" {
		t.Errorf("ReadAll = %q", all)
	}
}

// --- Multipart ---

func TestMultipartBuilder(t *testing.T) {
	mb := NewMultipartBuilder()
	if err := mb.AddField("name", "alice"); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if err := mb.AddFileFromBytes("file", "test.txt", []byte("file content")); err != nil {
		t.Fatalf("AddFileFromBytes: %v", err)
	}

	body, ct, err := mb.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(body) == 0 {
		t.Error("body is empty")
	}
	if !strings.Contains(ct, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data", ct)
	}
}

func TestMultipartFromReader(t *testing.T) {
	mb := NewMultipartBuilder()
	mb.AddField("key", "value")
	reader := bytes.NewReader([]byte("reader data"))
	err := mb.AddFileFromReader("upload", "data.bin", reader)
	if err != nil {
		t.Fatalf("AddFileFromReader: %v", err)
	}
	body, ct, err := mb.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(body) == 0 {
		t.Error("body empty")
	}
	if !strings.Contains(ct, "multipart/form-data") {
		t.Errorf("ct = %q", ct)
	}
}

func TestBuildMultipartRequest(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseMultipartForm(10 * 1024 * 1024)
		if err != nil {
			w.WriteHeader(400)
			w.Write([]byte("parse error: " + err.Error()))
			return
		}
		name := r.FormValue("name")
		file, _, _ := r.FormFile("data")
		var fileContent []byte
		if file != nil {
			fileContent, _ = io.ReadAll(file)
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, "name=%s,file=%s", name, string(fileContent))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 4

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	req, err := BuildMultipartRequest(c, "POST", "/",
		map[string]string{"name": "bob"},
		map[string][]byte{"data": []byte("payload")},
	)
	if err != nil {
		t.Fatalf("BuildMultipartRequest: %v", err)
	}
	defer c.ReleaseRequest(req)

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer c.ReleaseResponse(resp)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200; body = %q", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "name=bob") {
		t.Errorf("body = %q, missing name=bob", resp.Body)
	}
}

// --- Retry ---

func TestRetryerBackoff(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 5
	cfg.RetryBaseDelay = 100 * time.Millisecond
	cfg.RetryMaxDelay = 2 * time.Second
	cfg.RetryMultiplier = 2.0
	cfg.RetryJitter = false

	r := NewRetryer(cfg)
	if r == nil {
		t.Fatal("NewRetryer returned nil")
	}

	if d := r.Backoff(0); d != 100*time.Millisecond {
		t.Errorf("Backoff(0) = %v, want 100ms", d)
	}
	if d := r.Backoff(1); d != 200*time.Millisecond {
		t.Errorf("Backoff(1) = %v, want 200ms", d)
	}
	if d := r.Backoff(2); d != 400*time.Millisecond {
		t.Errorf("Backoff(2) = %v, want 400ms", d)
	}
	if d := r.Backoff(10); d > 2*time.Second {
		t.Errorf("Backoff(10) = %v, exceeds max 2s", d)
	}
}

func TestRetryerJitter(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 100 * time.Millisecond
	cfg.RetryJitter = true

	r := NewRetryer(cfg)
	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		seen[r.Backoff(0)] = true
	}
	if len(seen) < 2 {
		t.Error("jitter produced identical backoff values")
	}
}

func TestRetryerShouldRetry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryableStatus = []int{429, 503}

	r := NewRetryer(cfg)

	if !r.ShouldRetry(0, &Response{StatusCode: 429}, nil) {
		t.Error("should retry 429")
	}
	if !r.ShouldRetry(0, &Response{StatusCode: 503}, nil) {
		t.Error("should retry 503")
	}
	if r.ShouldRetry(0, &Response{StatusCode: 200}, nil) {
		t.Error("should not retry 200")
	}
	if r.ShouldRetry(0, &Response{StatusCode: 404}, nil) {
		t.Error("should not retry 404")
	}
	if !r.ShouldRetry(0, nil, WrapError(ErrorTypeNetwork, "conn reset", ErrConnectFailed)) {
		t.Error("should retry network error")
	}
	if r.ShouldRetry(3, &Response{StatusCode: 429}, nil) {
		t.Error("should not retry after max attempts")
	}
}

func TestRetryOnStatus(t *testing.T) {
	var reqCount atomic.Int32
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n <= 2 {
			w.WriteHeader(503)
			w.Write([]byte("unavailable"))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 4
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 10 * time.Millisecond
	cfg.RetryMaxDelay = 50 * time.Millisecond
	cfg.RetryJitter = false
	cfg.RetryableStatus = []int{503}

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.ReleaseResponse(resp)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (after retries)", resp.StatusCode)
	}
	if reqCount.Load() != 3 {
		t.Errorf("server received %d requests, want 3", reqCount.Load())
	}
}

func TestRetryerDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 0
	r := NewRetryer(cfg)
	if r != nil {
		t.Error("NewRetryer should return nil when MaxRetries=0")
	}
}

func TestRetryerWaitContextCancel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 5 * time.Second

	r := NewRetryer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := r.Wait(ctx, 0)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// --- DNS Cache ---

func TestDNSCacheLookup(t *testing.T) {
	cache := NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	addrs, err := cache.LookupHost("127.0.0.1")
	if err != nil || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Errorf("LookupHost(127.0.0.1) = %v, %v", addrs, err)
	}

	addrs, err = cache.LookupHost("localhost")
	if err != nil {
		t.Fatalf("LookupHost(localhost): %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("LookupHost(localhost) returned empty")
	}

	addrs2, err := cache.LookupHost("localhost")
	if err != nil {
		t.Fatalf("LookupHost(localhost) cached: %v", err)
	}
	if len(addrs2) == 0 {
		t.Fatal("cached lookup returned empty")
	}
}

func TestDNSCacheInvalidate(t *testing.T) {
	cache := NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	_, _ = cache.LookupHost("localhost")
	cache.Invalidate("localhost")

	cache.mu.RLock()
	_, exists := cache.entries["localhost"]
	cache.mu.RUnlock()
	if exists {
		t.Error("entry should be invalidated")
	}
}

func TestDNSCacheClear(t *testing.T) {
	cache := NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	_, _ = cache.LookupHost("localhost")
	cache.Clear()

	cache.mu.RLock()
	n := len(cache.entries)
	cache.mu.RUnlock()
	if n != 0 {
		t.Errorf("expected 0 entries after clear, got %d", n)
	}
}

func TestDNSCacheTTLExpiry(t *testing.T) {
	cache := NewDNSCache(100 * time.Millisecond)
	defer cache.Stop()

	_, _ = cache.LookupHost("localhost")
	time.Sleep(200 * time.Millisecond)

	cache.mu.RLock()
	entry, exists := cache.entries["localhost"]
	expired := exists && time.Now().After(entry.expires)
	cache.mu.RUnlock()

	if exists && !expired {
		t.Error("entry should have expired")
	}
}

func TestDNSCacheRefresh(t *testing.T) {
	cache := NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	addrs, err := cache.Refresh("localhost")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("Refresh returned empty")
	}

	cache.mu.RLock()
	_, exists := cache.entries["localhost"]
	cache.mu.RUnlock()
	if !exists {
		t.Error("expected cached entry after Refresh")
	}
}

func TestConcurrentDNSCache(t *testing.T) {
	cache := NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.LookupHost("localhost")
		}()
	}
	wg.Wait()
}

// --- Metrics ---

func TestBuiltinMetrics(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := NewBuiltinMetrics()
	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnableLogging = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 4
	cfg.Metrics = m
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	for i := 0; i < 5; i++ {
		resp, err := c.Get("/", nil)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		c.ReleaseResponse(resp)
	}

	snap := m.Snapshot()
	if snap.RequestsTotal != 5 {
		t.Errorf("RequestsTotal = %d, want 5", snap.RequestsTotal)
	}
	if snap.RequestsOK != 5 {
		t.Errorf("RequestsOK = %d, want 5", snap.RequestsOK)
	}
	// LatencyP50 can be 0 on very fast local requests (e.g. under -race); >= 0 is sufficient.
	if snap.LatencyP50 < 0 {
		t.Error("LatencyP50 should be >= 0")
	}
	if snap.BytesRead <= 0 {
		t.Error("BytesRead should be > 0")
	}
}

func TestBuiltinMetricsLatency(t *testing.T) {
	m := NewBuiltinMetrics()
	start := m.RecordRequest("GET", "example.com")
	time.Sleep(5 * time.Millisecond)
	m.RecordResponse("GET", "example.com", 200, nil, start, 100, 500)

	start2 := m.RecordRequest("POST", "example.com")
	time.Sleep(10 * time.Millisecond)
	m.RecordResponse("POST", "example.com", 201, nil, start2, 200, 1000)

	snap := m.Snapshot()
	if snap.RequestsTotal != 2 {
		t.Errorf("RequestsTotal = %d, want 2", snap.RequestsTotal)
	}
	if snap.BytesWritten != 300 {
		t.Errorf("BytesWritten = %d, want 300", snap.BytesWritten)
	}
	if snap.BytesRead != 1500 {
		t.Errorf("BytesRead = %d, want 1500", snap.BytesRead)
	}
	if snap.LatencyMin <= 0 {
		t.Error("LatencyMin should be > 0")
	}
	if snap.LatencyMax < snap.LatencyMin {
		t.Error("LatencyMax should be >= LatencyMin")
	}
}

func TestBuiltinMetricsPoolEvents(t *testing.T) {
	m := NewBuiltinMetrics()
	m.RecordPoolEvent(PoolEventConnCreated, "host")
	m.RecordPoolEvent(PoolEventConnCreated, "host")
	m.RecordPoolEvent(PoolEventConnReused, "host")
	m.RecordPoolEvent(PoolEventConnClosed, "host")
	m.RecordPoolEvent(PoolEventConnFailed, "host")

	snap := m.Snapshot()
	if snap.ConnsCreated != 2 {
		t.Errorf("ConnsCreated = %d, want 2", snap.ConnsCreated)
	}
	if snap.ConnsReused != 1 {
		t.Errorf("ConnsReused = %d, want 1", snap.ConnsReused)
	}
	if snap.ConnsClosed != 1 {
		t.Errorf("ConnsClosed = %d, want 1", snap.ConnsClosed)
	}
	if snap.ConnsFailed != 1 {
		t.Errorf("ConnsFailed = %d, want 1", snap.ConnsFailed)
	}
}

func TestBuiltinMetricsStatusCategories(t *testing.T) {
	m := NewBuiltinMetrics()
	now := time.Now()
	m.RecordResponse("GET", "h", 100, nil, now, 0, 0)
	m.RecordResponse("GET", "h", 200, nil, now, 0, 0)
	m.RecordResponse("GET", "h", 301, nil, now, 0, 0)
	m.RecordResponse("GET", "h", 404, nil, now, 0, 0)
	m.RecordResponse("GET", "h", 500, nil, now, 0, 0)
	m.RecordResponse("GET", "h", 0, ErrConnectFailed, now, 0, 0)

	snap := m.Snapshot()
	if snap.Requests1xx != 1 {
		t.Errorf("1xx = %d", snap.Requests1xx)
	}
	if snap.RequestsOK != 1 {
		t.Errorf("2xx = %d", snap.RequestsOK)
	}
	if snap.Requests3xx != 1 {
		t.Errorf("3xx = %d", snap.Requests3xx)
	}
	if snap.Requests4xx != 1 {
		t.Errorf("4xx = %d", snap.Requests4xx)
	}
	if snap.Requests5xx != 1 {
		t.Errorf("5xx = %d", snap.Requests5xx)
	}
	if snap.RequestsError != 1 {
		t.Errorf("errors = %d", snap.RequestsError)
	}
}

func TestConcurrentMetrics(t *testing.T) {
	m := NewBuiltinMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := m.RecordRequest("GET", "host")
			m.RecordResponse("GET", "host", 200, nil, start, 10, 20)
			m.RecordPoolEvent(PoolEventConnReused, "host")
		}()
	}
	wg.Wait()

	snap := m.Snapshot()
	if snap.RequestsTotal != 100 {
		t.Errorf("RequestsTotal = %d, want 100", snap.RequestsTotal)
	}
}

func TestClientStats(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := NewBuiltinMetrics()
	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 4
	cfg.EnableLogging = false
	cfg.Metrics = m

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	resp, err := c.Get("/", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.ReleaseResponse(resp)

	stats := c.Stats()
	if stats.HealthyConnections < 1 {
		t.Error("expected at least 1 healthy connection")
	}
	if stats.Metrics.RequestsTotal != 1 {
		t.Errorf("RequestsTotal = %d, want 1", stats.Metrics.RequestsTotal)
	}
}

// --- SOCKS5 ---

func TestSOCKS5DialerNew(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SOCKS5Addr = ""

	_, err := NewSOCKS5Dialer(cfg)
	if err == nil {
		t.Error("expected error for empty SOCKS5 address")
	}

	cfg.SOCKS5Addr = "127.0.0.1:1080"
	cfg.SOCKS5Username = "user"
	cfg.SOCKS5Password = "pass"

	d, err := NewSOCKS5Dialer(cfg)
	if err != nil {
		t.Fatalf("NewSOCKS5Dialer: %v", err)
	}
	if d.proxyAddr != "127.0.0.1:1080" {
		t.Errorf("proxyAddr = %q", d.proxyAddr)
	}
}

// --- Config ---

func TestConfigValidateDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want localhost", cfg.Host)
	}
	if cfg.Port != 80 {
		t.Errorf("Port = %d, want 80", cfg.Port)
	}
	if cfg.PoolSize != 512 {
		t.Errorf("PoolSize = %d, want 512", cfg.PoolSize)
	}
	if cfg.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", cfg.ReadTimeout)
	}
}

func TestConfigValidateTLSDefaults(t *testing.T) {
	cfg := &Config{UseTLS: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Port != 443 {
		t.Errorf("Port = %d, want 443", cfg.Port)
	}
}

func TestHighThroughputConfig(t *testing.T) {
	cfg := HighThroughputConfig()
	if cfg.PoolSize != 1024 {
		t.Errorf("PoolSize = %d, want 1024", cfg.PoolSize)
	}
	if !cfg.EnableDNSCache {
		t.Error("EnableDNSCache should be true")
	}
}

func TestResilientConfig(t *testing.T) {
	cfg := ResilientConfig()
	if cfg.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", cfg.MaxRetries)
	}
	if !cfg.EnableDNSCache {
		t.Error("EnableDNSCache should be true")
	}
}

// --- Error utilities ---

func TestErrorUtilities(t *testing.T) {
	if IsTimeout(nil) {
		t.Error("IsTimeout(nil) should be false")
	}
	if IsRetryable(nil) {
		t.Error("IsRetryable(nil) should be false")
	}

	timeout := WrapError(ErrorTypeTimeout, "timed out", ErrTimeout)
	if !IsTimeout(timeout) {
		t.Error("IsTimeout should detect wrapped timeout")
	}

	network := WrapError(ErrorTypeNetwork, "reset", ErrConnectFailed)
	if !IsRetryable(network) {
		t.Error("IsRetryable should detect network error")
	}

	validation := WrapError(ErrorTypeValidation, "bad input", ErrInvalidURL)
	if IsRetryable(validation) {
		t.Error("validation error should not be retryable")
	}

	de := &DetailedError{Type: ErrorTypeProxy, Message: "proxy failed", Err: ErrProxyFailed}
	if de.Error() == "" {
		t.Error("Error() should not be empty")
	}
	if de.Unwrap() != ErrProxyFailed {
		t.Error("Unwrap should return the inner error")
	}
}

func TestGetVersion(t *testing.T) {
	v := GetVersion()
	if v == "" {
		t.Error("GetVersion returned empty")
	}
	if !strings.HasPrefix(v, "v") {
		t.Errorf("version = %q, want prefix 'v'", v)
	}
}

// --- Internals ---

func TestStrEqualFoldASCII(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Content-Type", "content-type", true},
		{"ABC", "abc", true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "abcd", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := strEqualFoldASCII(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("strEqualFoldASCII(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseHostPort(t *testing.T) {
	cases := []struct {
		key      string
		defHost  string
		defPort  int
		wantHost string
		wantPort int
	}{
		{"example.com:8080", "def", 80, "example.com", 8080},
		{"[::1]:9090", "def", 80, "::1", 9090},
		{"", "default.host", 443, "default.host", 443},
		{"no-port", "def", 80, "no-port", 80},
	}
	for _, tc := range cases {
		gotHost, gotPort := parseHostPort(tc.key, tc.defHost, tc.defPort)
		if gotHost != tc.wantHost || gotPort != tc.wantPort {
			t.Errorf("parseHostPort(%q) = (%q, %d), want (%q, %d)",
				tc.key, gotHost, gotPort, tc.wantHost, tc.wantPort)
		}
	}
}

func TestReadFullHelper(t *testing.T) {
	data := []byte("hello world")
	conn := newFakeConn(data)

	buf := make([]byte, 5)
	n, err := readFull(conn, buf)
	if err != nil {
		t.Fatalf("readFull: %v", err)
	}
	if n != 5 || string(buf) != "hello" {
		t.Errorf("readFull = (%d, %q)", n, buf)
	}
}

// --- Test helpers ---

type fakeConn struct {
	r  *bytes.Reader
	mu sync.Mutex
}

func newFakeConn(data []byte) net.Conn {
	return &fakeConn{r: bytes.NewReader(data)}
}

func (f *fakeConn) Read(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.r.Read(b)
}
func (f *fakeConn) Write(b []byte) (int, error)        { return len(b), nil }
func (f *fakeConn) Close() error                       { return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

func newConnectProxy(t testing.TB) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-stopCh:
					return
				default:
					continue
				}
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				handleProxyConn(c)
			}(conn)
		}
	}()
	stop = func() {
		close(stopCh)
		_ = ln.Close()
		wg.Wait()
	}
	return ln.Addr().String(), stop
}

func handleProxyConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	var reqBuf bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		reqBuf.WriteString(line)
		if line == "\r\n" {
			break
		}
	}
	lines := strings.Split(reqBuf.String(), "\r\n")
	if len(lines) == 0 {
		return
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) < 2 || parts[0] != "CONNECT" {
		return
	}
	target := parts[1]
	targetConn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer targetConn.Close()
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	var proxyWg sync.WaitGroup
	proxyWg.Add(2)
	go func() { defer proxyWg.Done(); io.Copy(targetConn, reader) }()
	go func() { defer proxyWg.Done(); io.Copy(conn, targetConn) }()
	proxyWg.Wait()
}

// TestProxyDialerCONNECTRequest verifies the CONNECT request is correctly
// formed (no null bytes in the Host header) when using an HTTP proxy.
func TestProxyDialerCONNECTRequest(t *testing.T) {
	// Intercept the raw CONNECT bytes sent by ProxyDialer.
	var received []byte
	var mu sync.Mutex

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		mu.Lock()
		received = buf[:n]
		mu.Unlock()
		// Reply with 200 so Dial returns without error.
		conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	}()

	cfg := DefaultConfig()
	cfg.ProxyURL = "http://" + ln.Addr().String()
	cfg.ProxyUsername = "user"
	cfg.ProxyPassword = "pass"
	cfg.ProxyConnectTimeout = 3 * time.Second
	cfg.ProxyReadTimeout = 3 * time.Second

	pd, err := NewProxyDialer(cfg)
	if err != nil {
		t.Fatalf("NewProxyDialer: %v", err)
	}

	conn, err := pd.Dial("example.com", 443)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
	<-done

	mu.Lock()
	raw := string(received)
	mu.Unlock()

	// Must not contain null bytes anywhere.
	for i, b := range received {
		if b == 0 {
			t.Errorf("null byte at position %d in CONNECT request:\n%q", i, raw)
		}
	}

	// Host header value must immediately follow "Host: " without any garbage.
	const marker = "Host: "
	idx := strings.Index(raw, marker)
	if idx == -1 {
		t.Fatalf("Host header not found in CONNECT request:\n%q", raw)
	}
	hostVal := raw[idx+len(marker):]
	if !strings.HasPrefix(hostVal, "example.com:443") {
		t.Errorf("Host header value = %q, want prefix \"example.com:443\"", hostVal)
	}
}

// newHTTPForwardProxy starts an in-process HTTP forward proxy that reads
// absolute-URI requests and forwards them to the origin.
func newHTTPForwardProxy(t testing.TB) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	stopCh := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Forward the request to the origin using the standard http client.
			target := r.URL
			if target.Host == "" {
				http.Error(w, "missing host", 400)
				return
			}
			outReq, err := http.NewRequest(r.Method, target.String(), r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			resp, err := http.DefaultTransport.RoundTrip(outReq)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		}),
	}
	go srv.Serve(ln)
	stop = func() {
		close(stopCh)
		srv.Close()
	}
	return ln.Addr().String(), stop
}

// TestProxyDialerThroughRealProxy exercises the full client → forward proxy → target
// path for HTTP (non-TLS) requests.
func TestProxyDialerThroughRealProxy(t *testing.T) {
	// Start a tiny origin server.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("proxied"))
	}))

	// Start an in-process HTTP forward proxy.
	proxyAddr, stopProxy := newHTTPForwardProxy(t)

	originHost, originPortStr, _ := strings.Cut(strings.TrimPrefix(origin.URL, "http://"), ":")
	var originPort int
	fmt.Sscanf(originPortStr, "%d", &originPort)

	cfg := DefaultConfig()
	cfg.Host = originHost
	cfg.Port = originPort
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 2
	cfg.ProxyURL = "http://" + proxyAddr

	c, err := NewClient(cfg)
	if err != nil {
		stopProxy()
		origin.Close()
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := c.Get("/", nil)
	c.Stop()
	origin.Close()
	stopProxy()

	if err != nil {
		t.Fatalf("GET through forward proxy: %v", err)
	}
	defer c.ReleaseResponse(resp)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != "proxied" {
		t.Errorf("body = %q, want \"proxied\"", resp.Body)
	}
}

// TestForwardProxyRequest verifies that an HTTP (non-TLS) request through a
// forward proxy uses absolute-URI form and injects the Proxy-Authorization
// header — without sending a CONNECT request.
func TestForwardProxyRequest(t *testing.T) {
	// Capture the raw bytes the client sends to the "proxy".
	var received []byte
	var mu sync.Mutex

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		mu.Lock()
		received = buf[:n]
		mu.Unlock()
		// Reply with a minimal HTTP response so the client doesn't hang.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	}()

	cfg := DefaultConfig()
	cfg.Host = "example.com"
	cfg.Port = 80
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.EnableLogging = false
	cfg.PoolSize = 1
	cfg.ProxyURL = "http://" + ln.Addr().String()
	cfg.ProxyUsername = "user"
	cfg.ProxyPassword = "pass"
	cfg.ProxyConnectTimeout = 3 * time.Second
	cfg.ProxyReadTimeout = 3 * time.Second

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	resp, err := c.Get("/test", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c.ReleaseResponse(resp)
	<-done

	mu.Lock()
	raw := string(received)
	mu.Unlock()

	// Must NOT start with CONNECT.
	if strings.HasPrefix(raw, "CONNECT") {
		t.Errorf("forward proxy sent CONNECT instead of absolute-URI request:\n%q", raw)
	}

	// Request line must use absolute-URI form.
	if !strings.HasPrefix(raw, "GET http://example.com/test HTTP/1.1") {
		t.Errorf("request line = %q, want absolute-URI form", strings.SplitN(raw, "\r\n", 2)[0])
	}

	// Must include Proxy-Authorization header.
	if !strings.Contains(raw, "Proxy-Authorization: Basic ") {
		t.Errorf("Proxy-Authorization header missing in:\n%q", raw)
	}
}

// TestProxyCONNECT407 verifies that a 407 proxy rejection includes the status
// code in the error message returned by ProxyDialer.Dial.
func TestProxyCONNECT407(t *testing.T) {
	// Stand up a fake proxy that accepts all connections and always returns 407.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				c.Read(buf)
				c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"))
			}(conn)
		}
	}()

	// Test ProxyDialer.Dial directly — this is where the 407 status code
	// is included in the error, before it gets wrapped by the pool layer.
	cfg := DefaultConfig()
	cfg.ProxyURL = "http://" + ln.Addr().String()
	cfg.ProxyConnectTimeout = 3 * time.Second
	cfg.ProxyReadTimeout = 3 * time.Second

	d, err := NewProxyDialer(cfg)
	if err != nil {
		t.Fatalf("NewProxyDialer: %v", err)
	}

	_, err = d.Dial("example.com", 443)
	if err == nil {
		t.Fatal("expected error for 407 response, got nil")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Errorf("error should mention status 407, got: %v", err)
	}
}

// TestIsForwardProxy verifies the forward-proxy detection logic in the Dialer.
func TestIsForwardProxy(t *testing.T) {
	// No proxy configured → never a forward proxy.
	cfg := DefaultConfig()
	cfg.Host = "example.com"
	d, err := NewDialer(cfg)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	defer d.Stop()
	if d.IsForwardProxy(false) {
		t.Error("IsForwardProxy should be false when no proxy is configured")
	}
	if d.IsForwardProxy(true) {
		t.Error("IsForwardProxy should be false when no proxy is configured (TLS)")
	}

	// HTTP proxy + plain HTTP target → forward proxy.
	cfg2 := DefaultConfig()
	cfg2.ProxyURL = "http://proxy.example.com:3128"
	d2, err := NewDialer(cfg2)
	if err != nil {
		t.Fatalf("NewDialer with proxy: %v", err)
	}
	defer d2.Stop()
	if !d2.IsForwardProxy(false) {
		t.Error("IsForwardProxy should be true for HTTP target through HTTP proxy")
	}
	if d2.IsForwardProxy(true) {
		t.Error("IsForwardProxy should be false for HTTPS target (uses CONNECT instead)")
	}
}

// TestChunkedBodyWithTrailers verifies that chunked responses with trailer
// headers are parsed correctly and trailers are fully drained.
func TestChunkedBodyWithTrailers(t *testing.T) {
	// Craft a chunked response that includes trailers after the terminal chunk.
	raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Checksum\r\n\r\n" +
		"5\r\nhello\r\n" +
		"6\r\n world\r\n" +
		"0\r\n" +
		"X-Checksum: abc123\r\n" + // trailer header
		"\r\n"

	conn := newFakeConn([]byte(raw))
	buf := make([]byte, 4096)
	resp := &Response{bodyBuf: make([]byte, 4096), Headers: make([]Header, 0, 8)}

	err := readResponse(conn, buf, resp, 1<<20, "GET", 4096, 4096)
	if err != nil {
		t.Fatalf("readResponse with trailers: %v", err)
	}
	if string(resp.Body) != "hello world" {
		t.Errorf("body = %q, want %q", resp.Body, "hello world")
	}
}

// TestGracefulStopUnderLoad verifies GracefulStop waits for in-flight requests.
func TestGracefulStopUnderLoad(t *testing.T) {
	slow := make(chan struct{})
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-slow
		w.WriteHeader(200)
		w.Write([]byte("done"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 4
	cfg.EnableLogging = false

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		resp, err := c.Get("/slow", nil)
		if err == nil {
			c.ReleaseResponse(resp)
		}
		errCh <- err
	}()

	// Give the goroutine time to start the request.
	time.Sleep(50 * time.Millisecond)

	drainedCh := make(chan bool, 1)
	go func() {
		drainedCh <- c.GracefulStop(2 * time.Second)
	}()

	// Unblock the server handler.
	close(slow)

	drained := <-drainedCh
	if !drained {
		t.Error("GracefulStop reported timeout, expected clean drain")
	}
	<-errCh
}

// TestMaxRequestsPerConn verifies that connections are cycled after
// MaxRequestsPerConn requests.
func TestMaxRequestsPerConn(t *testing.T) {
	srv, host, port := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 8
	cfg.MaxRequestsPerConn = 2 // recycle after 2 requests

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	// Issue 6 requests with MaxRequestsPerConn=2 → at least 3 connections created.
	for i := 0; i < 6; i++ {
		resp, err := c.Get("/", nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		c.ReleaseResponse(resp)
	}
}

// TestErrInvalidRequest verifies that a nil request returns ErrInvalidRequest.
func TestErrInvalidRequest(t *testing.T) {
	cfg := DefaultConfig()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	_, err = c.DoWithContext(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil request")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest in error chain, got: %v", err)
	}
}

// TestIsTimeoutContextDeadline verifies IsTimeout handles context.DeadlineExceeded.
func TestIsTimeoutContextDeadline(t *testing.T) {
	if !IsTimeout(context.DeadlineExceeded) {
		t.Error("IsTimeout should return true for context.DeadlineExceeded")
	}
	if IsTimeout(context.Canceled) {
		t.Error("IsTimeout should return false for context.Canceled (cancellation != timeout)")
	}
	wrapped := fmt.Errorf("outer: %w", context.DeadlineExceeded)
	if !IsTimeout(wrapped) {
		t.Error("IsTimeout should return true for wrapped context.DeadlineExceeded")
	}
}

// TestIsTimeoutNetError verifies IsTimeout handles net.Error with Timeout()==true.
// Uses a raw listener that accepts but never responds, so the read deadline fires.
func TestIsTimeoutNetError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// Accept connections and hold them open without sending any data.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold open but never write — forces client read timeout.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	host, portStr, _ := strings.Cut(ln.Addr().String(), ":")
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 1
	cfg.ReadTimeout = 50 * time.Millisecond

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	_, err = c.Get("/", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false, want true", err)
	}
}
