// Advanced bursthttp usage: high-throughput, retry, metrics, streaming, multipart.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bursthttp "github.com/muxover/bursthttp"
)

func main() {
	highThroughput()
	fmt.Println()
	retryAndMetrics()
	fmt.Println()
	streamingAndMultipart()
	fmt.Println()
	batchRequests()
	fmt.Println()
	preEncodedHeaders()
	fmt.Println()
	gzipCompression()
	fmt.Println()
	connectionInspector()
	fmt.Println()
	rateLimiter()
}

// highThroughput demonstrates pipelining with connection warm-up.
func highThroughput() {
	fmt.Println("═══ High Throughput (50 goroutines × 100 requests) ═══")

	cfg := bursthttp.HighThroughputConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	m := bursthttp.NewBuiltinMetrics()
	cfg.Metrics = m

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.GracefulStop(10 * time.Second)

	if err := client.StartN(32); err != nil {
		fmt.Printf("Warm-up: %v\n", err)
	}

	var success, errors atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				resp, err := client.Get("/get", nil)
				if err != nil {
					errors.Add(1)
					continue
				}
				success.Add(1)
				client.ReleaseResponse(resp)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	snap := m.Snapshot()
	fmt.Printf("  Success: %d, Errors: %d\n", success.Load(), errors.Load())
	fmt.Printf("  Duration: %s, RPS: %.0f\n", elapsed, float64(success.Load())/elapsed.Seconds())
	fmt.Printf("  Latency p50: %s, p95: %s, p99: %s\n", snap.LatencyP50, snap.LatencyP95, snap.LatencyP99)
	fmt.Printf("  Bytes sent: %d, received: %d\n", snap.BytesWritten, snap.BytesRead)
	fmt.Printf("  Conns created: %d, reused: %d\n", snap.ConnsCreated, snap.ConnsReused)
}

// retryAndMetrics demonstrates automatic retry with metrics.
func retryAndMetrics() {
	fmt.Println("═══ Retry + Metrics ═══")

	cfg := bursthttp.ResilientConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true
	cfg.EnableDNSCache = true

	m := bursthttp.NewBuiltinMetrics()
	cfg.Metrics = m

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	fmt.Printf("  Config: MaxRetries=%d, BaseDelay=%s, DNSCache=%v\n",
		cfg.MaxRetries, cfg.RetryBaseDelay, cfg.EnableDNSCache)

	resp, err := client.Get("/get", nil)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
		return
	}
	fmt.Printf("  Status: %d, Content-Type: %s\n", resp.StatusCode, resp.Header("Content-Type"))
	client.ReleaseResponse(resp)

	// 429 would trigger retry automatically — here we show a normal request.
	snap := m.Snapshot()
	fmt.Printf("  Total requests: %d, Retries: %d\n", snap.RequestsTotal, snap.RetriesTotal)
}

// batchRequests demonstrates Client.Batch — concurrent fan-out with ordered results.
func batchRequests() {
	fmt.Println("═══ Batch API (fan-out 5 concurrent requests) ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	results := client.BatchWithContext(context.Background(), func(b *bursthttp.Batch) {
		b.Get("/get", nil)
		b.Get("/ip", nil)
		b.Get("/user-agent", nil)
		b.Post("/post", []byte(`{"batch":true}`), []bursthttp.Header{{Key: "Content-Type", Value: "application/json"}})
		b.Get("/headers", nil)
	})

	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("  [%d] error: %v\n", r.Index, r.Err)
			continue
		}
		fmt.Printf("  [%d] status=%d bytes=%d\n", r.Index, r.Response.StatusCode, len(r.Response.Body))
		client.ReleaseResponse(r.Response)
	}
}

// preEncodedHeaders demonstrates BuildPreEncodedHeaderPrefix — encode headers once,
// reuse across many requests without re-encoding on every send.
func preEncodedHeaders() {
	fmt.Println("═══ Pre-Encoded Headers (zero-alloc hot path) ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	// Build a template request with fixed headers.
	tmpl := client.AcquireRequest()
	tmpl.Method = "GET"
	tmpl.Path = "/get"
	tmpl.SetHeader("User-Agent", "bursthttp/"+bursthttp.GetVersion())
	tmpl.SetHeader("Accept", "application/json")
	tmpl.SetHeader("X-Service", "example")
	defer client.ReleaseRequest(tmpl)

	prefix, err := client.BuildPreEncodedHeaderPrefix(tmpl, "httpbin.org", 443, true)
	if err != nil {
		fmt.Printf("BuildPreEncodedHeaderPrefix: %v\n", err)
		return
	}
	fmt.Printf("  Header prefix encoded once: %d bytes\n", len(prefix))

	// Subsequent requests skip all header encoding — only body changes between sends.
	for i := 1; i <= 3; i++ {
		req := client.AcquireRequest()
		req.Method = "GET"
		req.Path = "/get"
		req.PreEncodedHeaderPrefix = prefix

		resp, err := client.Do(req)
		client.ReleaseRequest(req)
		if err != nil {
			fmt.Printf("  req %d: error: %v\n", i, err)
			continue
		}
		fmt.Printf("  req %d: status=%d\n", i, resp.StatusCode)
		client.ReleaseResponse(resp)
	}
}

// gzipCompression demonstrates EnableCompression (request body gzip) and
// transparent response decompression when the server returns Content-Encoding: gzip.
func gzipCompression() {
	fmt.Println("═══ Gzip Compression ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true
	cfg.EnableCompression = true // gzip-encode outgoing request bodies; response decompression is automatic

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	payload := []byte(`{"data":"` + strings.Repeat("bursthttp ", 50) + `"}`)
	resp, err := client.Post("/post", payload, []bursthttp.Header{
		{Key: "Content-Type", Value: "application/json"},
		{Key: "Accept-Encoding", Value: "gzip"},
	})
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
		return
	}
	defer client.ReleaseResponse(resp)
	fmt.Printf("  Status: %d, original payload: %d bytes, response: %d bytes\n",
		resp.StatusCode, len(payload), len(resp.Body))
}

// connectionInspector demonstrates Client.ConnectionPool() — a live snapshot of
// every pooled connection with health score, latency EWMA, and byte counters.
func connectionInspector() {
	fmt.Println("═══ Connection Inspector ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true
	cfg.EnableHealthScoring = true

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	if err := client.StartN(4); err != nil {
		fmt.Printf("Warm-up: %v\n", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get("/get", nil)
			if err == nil {
				client.ReleaseResponse(resp)
			}
		}()
	}
	wg.Wait()

	conns := client.ConnectionPool()
	fmt.Printf("  Live connections: %d\n", len(conns))
	for _, c := range conns {
		fmt.Printf("  [conn %d] %s:%d healthy=%v score=%d latency=%s active=%d bytes_r=%d bytes_w=%d\n",
			c.ID, c.Host, c.Port, c.Healthy, c.HealthScore,
			c.LatencyEWMA.Round(time.Millisecond),
			c.ActiveReqs, c.BytesRead, c.BytesWritten)
	}
}

// rateLimiter demonstrates RequestsPerSecond + BurstSize token-bucket throttling.
func rateLimiter() {
	fmt.Println("═══ Rate Limiter (10 RPS, burst 3) ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true
	cfg.RequestsPerSecond = 10
	cfg.BurstSize = 3

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	start := time.Now()
	for i := 0; i < 6; i++ {
		resp, err := client.Get("/get", nil)
		if err != nil {
			fmt.Printf("  req %d: error: %v\n", i+1, err)
			continue
		}
		fmt.Printf("  req %d: status=%d elapsed=%s\n", i+1, resp.StatusCode, time.Since(start).Round(time.Millisecond))
		client.ReleaseResponse(resp)
	}
	fmt.Printf("  Total: %s for 6 requests at 10 RPS\n", time.Since(start).Round(time.Millisecond))
}

// streamingAndMultipart demonstrates streaming responses and multipart uploads.
func streamingAndMultipart() {
	fmt.Println("═══ Streaming + Multipart ═══")

	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed: %v\n", err)
		return
	}
	defer client.Stop()

	// Streaming response.
	fmt.Println("  Streaming GET...")
	req := client.AcquireRequest()
	req.WithMethod("GET").WithPath("/get")
	sr, err := client.DoStreaming(context.Background(), req)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  Status: %d, Content-Type: %s\n", sr.StatusCode, sr.Header("Content-Type"))
		sr.Close()
	}
	client.ReleaseRequest(req)

	// Streaming request body (io.Reader).
	fmt.Println("  DoReader POST...")
	body := strings.NewReader(`{"streamed": true}`)
	resp, err := client.DoReader(context.Background(), "POST", "/post", body, 18,
		[]bursthttp.Header{{Key: "Content-Type", Value: "application/json"}})
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  Status: %d\n", resp.StatusCode)
		client.ReleaseResponse(resp)
	}

	// Multipart upload.
	fmt.Println("  Multipart POST...")
	mpReq, err := bursthttp.BuildMultipartRequest(client, "POST", "/post",
		map[string]string{"name": "alice", "role": "admin"},
		map[string][]byte{"config": []byte(`{"debug": true}`)},
	)
	if err != nil {
		fmt.Printf("  Build error: %v\n", err)
		return
	}
	defer client.ReleaseRequest(mpReq)

	resp, err = client.Do(mpReq)
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	} else {
		fmt.Printf("  Status: %d, Body: %d bytes\n", resp.StatusCode, len(resp.Body))
		client.ReleaseResponse(resp)
	}
}
