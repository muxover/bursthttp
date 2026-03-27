package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
}

func benchmarkClientParallel(b *testing.B, cfg *Config) {
	c, err := NewClient(cfg)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := c.AcquireRequest()
			req.Method = "GET"
			req.Path = "/"

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := c.DoWithContext(ctx, req)
			if err == nil {
				c.ReleaseResponse(resp)
			}
			cancel()
			c.ReleaseRequest(req)
		}
	})
}

func benchmarkClientSerial(b *testing.B, cfg *Config) {
	c, err := NewClient(cfg)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req := c.AcquireRequest()
		req.Method = "GET"
		req.Path = "/"

		resp, err := c.Do(req)
		if err == nil {
			c.ReleaseResponse(resp)
		}
		c.ReleaseRequest(req)
	}
}

func BenchmarkDirectPipelined(b *testing.B) {
	srv := newTestServer([]byte("ok"))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = true
	cfg.MaxPipelinedRequests = 10
	cfg.PoolSize = 256

	benchmarkClientParallel(b, cfg)
}

func BenchmarkDirectSequential(b *testing.B) {
	srv := newTestServer([]byte("ok"))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 64

	benchmarkClientSerial(b, cfg)
}

func BenchmarkDirectLargeBody(b *testing.B) {
	body := make([]byte, 64*1024)
	for i := range body {
		body[i] = byte(i % 256)
	}
	srv := newTestServer(body)
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = true
	cfg.PoolSize = 128

	benchmarkClientParallel(b, cfg)
}

func BenchmarkWithMetrics(b *testing.B) {
	srv := newTestServer([]byte("ok"))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = true
	cfg.PoolSize = 128
	cfg.Metrics = NewBuiltinMetrics()

	benchmarkClientParallel(b, cfg)
}

func BenchmarkWithRetry(b *testing.B) {
	srv := newTestServer([]byte("ok"))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 64
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 10 * time.Millisecond
	cfg.RetryableStatus = []int{429, 502, 503}

	benchmarkClientSerial(b, cfg)
}

func BenchmarkRetryerBackoff(b *testing.B) {
	cfg := DefaultConfig()
	cfg.MaxRetries = 5
	cfg.RetryBaseDelay = 100 * time.Millisecond
	cfg.RetryMaxDelay = 5 * time.Second
	cfg.RetryMultiplier = 2.0
	cfg.RetryJitter = true
	r := NewRetryer(cfg)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Backoff(i % 5)
	}
}

func BenchmarkDNSCacheLookup(b *testing.B) {
	cache := NewDNSCache(5*time.Minute, 5*time.Second)
	defer cache.Stop()
	_, _ = cache.LookupHost("localhost")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cache.LookupHost("localhost")
	}
}

func BenchmarkDNSCacheLookupIP(b *testing.B) {
	cache := NewDNSCache(5*time.Minute, 5*time.Second)
	defer cache.Stop()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cache.LookupHost("127.0.0.1")
	}
}

func BenchmarkResponseHeaderLookup(b *testing.B) {
	resp := &Response{
		bodyBuf: make([]byte, 1),
		Headers: []Header{
			{Key: "Content-Type", Value: "application/json"},
			{Key: "Content-Length", Value: "1234"},
			{Key: "X-Request-Id", Value: "abc-123-def-456"},
			{Key: "Cache-Control", Value: "no-cache"},
			{Key: "X-Custom-Header", Value: "some-value"},
			{Key: "Server", Value: "nginx"},
			{Key: "Date", Value: "Mon, 01 Jan 2024 00:00:00 GMT"},
			{Key: "Connection", Value: "keep-alive"},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = resp.Header("X-Request-Id")
	}
}

func BenchmarkResponseHasHeader(b *testing.B) {
	resp := &Response{
		bodyBuf: make([]byte, 1),
		Headers: []Header{
			{Key: "Content-Type", Value: "application/json"},
			{Key: "X-Request-Id", Value: "abc"},
			{Key: "Connection", Value: "keep-alive"},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = resp.HasHeader("connection")
	}
}

func BenchmarkMetricsRecordResponse(b *testing.B) {
	m := NewBuiltinMetrics()
	start := time.Now()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.RecordResponse("GET", "example.com", 200, nil, start, 100, 500)
		}
	})
}

func BenchmarkMetricsSnapshot(b *testing.B) {
	m := NewBuiltinMetrics()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		m.RecordResponse("GET", "host", 200, nil, start, 10, 20)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Snapshot()
	}
}

func BenchmarkStrEqualFoldASCII(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = strEqualFoldASCII("Content-Type", "content-type")
	}
}

func BenchmarkRequestAcquireRelease(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := AcquireRequest()
		req.Method = "GET"
		req.Path = "/test"
		req.SetHeader("X-Test", "value")
		ReleaseRequest(req)
	}
}

func BenchmarkMultipartBuild(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mb := NewMultipartBuilder()
		mb.AddField("name", "alice")
		mb.AddField("email", "alice@example.com")
		mb.AddFileFromBytes("file", "test.txt", []byte("file content here"))
		mb.Finish()
	}
}

func BenchmarkParseStatusCode(b *testing.B) {
	buf := []byte("HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseStatusCode(buf)
	}
}

func BenchmarkParseContentLength(b *testing.B) {
	buf := []byte("HTTP/1.1 200 OK\r\nContent-Length: 42\r\n\r\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseContentLength(buf)
	}
}

func BenchmarkFindHeaderEnd(b *testing.B) {
	buf := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 1234\r\nServer: nginx\r\n\r\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		findHeaderEnd(buf)
	}
}

func BenchmarkWriteRequest(b *testing.B) {
	req := &Request{
		Method:    "GET",
		Path:      "/api/v1/users",
		headerBuf: make([]byte, 4096),
	}
	req.SetHeader("Accept", "application/json")
	req.SetHeader("X-Request-Id", "bench-123")

	cfg := DefaultConfig()
	cfg.KeepAlive = true
	buf := make([]byte, 4096)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = writeRequest(buf, req, cfg, "api.example.com", 443, true, nil, false, false, nil)
	}
}

func BenchmarkWriteRequestForwardProxy(b *testing.B) {
	req := &Request{
		Method:    "GET",
		Path:      "/api/v1/users",
		headerBuf: make([]byte, 4096),
	}
	req.SetHeader("Accept", "application/json")

	cfg := DefaultConfig()
	cfg.KeepAlive = true
	buf := make([]byte, 4096)
	authHeader := []byte("Proxy-Authorization: Basic dXNlcjpwYXNz\r\n")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = writeRequest(buf, req, cfg, "proxy.example.com", 3128, false, nil, false, true, authHeader)
	}
}

func BenchmarkReadResponse(b *testing.B) {
	srv := newTestServer([]byte(`{"status":"ok","message":"benchmark response body"}`))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	port := 80
	fmt.Sscanf(portStr, "%d", &port)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 1

	b.ReportAllocs()
	b.ResetTimer()
	benchmarkClientSerial(b, cfg)
}

func BenchmarkViaHTTPProxy(b *testing.B) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("proxied-ok"))
	}))
	defer origin.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(r.RequestURI)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	proxyHost, proxyPortStr, _ := strings.Cut(strings.TrimPrefix(proxy.URL, "http://"), ":")
	proxyPort := 8080
	fmt.Sscanf(proxyPortStr, "%d", &proxyPort)

	cfg := DefaultConfig()
	cfg.Host = origin.URL[7:] // strip http://
	cfg.Port = 80
	cfg.UseTLS = false
	cfg.EnablePipelining = false
	cfg.PoolSize = 8
	cfg.ProxyURL = proxy.URL
	cfg.ProxyConnectTimeout = 5 * time.Second
	cfg.ProxyReadTimeout = 5 * time.Second

	_ = proxyHost
	_ = proxyPort

	c, err := NewClient(cfg)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	defer c.Stop()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := c.AcquireRequest()
			req.Method = "GET"
			req.Path = "/"

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := c.DoWithContext(ctx, req)
			if err == nil {
				c.ReleaseResponse(resp)
			}
			cancel()
			c.ReleaseRequest(req)
		}
	})
}
