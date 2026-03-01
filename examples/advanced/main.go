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
