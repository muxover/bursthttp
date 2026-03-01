package client_test

import (
	"context"
	"fmt"
	"log"
	"time"

	client "github.com/muxover/bursthttp"
)

func ExampleNewClient() {
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Stop()

	fmt.Println("client created")
	// Output: client created
}

func ExampleClient_Get() {
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Stop()

	resp, err := c.Get("/get", nil)
	if err != nil {
		log.Fatal(err)
	}
	defer c.ReleaseResponse(resp)
	fmt.Printf("status: %d\n", resp.StatusCode)
}

func ExampleClient_Post() {
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Stop()

	resp, err := c.Post("/post", []byte(`{"key":"value"}`), []client.Header{
		{Key: "Content-Type", Value: "application/json"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.ReleaseResponse(resp)
	fmt.Printf("status: %d\n", resp.StatusCode)
}

func ExampleClient_DoWithContext() {
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := c.AcquireRequest()
	defer c.ReleaseRequest(req)

	req.WithMethod("GET").
		WithURL("https://httpbin.org/get").
		WithHeader("Accept", "application/json")

	resp, err := c.DoWithContext(ctx, req)
	if err != nil {
		log.Fatal(err)
	}
	defer c.ReleaseResponse(resp)
	fmt.Printf("status: %d\n", resp.StatusCode)
}

func ExampleHighThroughputConfig() {
	cfg := client.HighThroughputConfig()
	cfg.Host = "api.example.com"
	cfg.Port = 443
	cfg.UseTLS = true

	fmt.Printf("pool size: %d, pipelining: %v\n", cfg.PoolSize, cfg.EnablePipelining)
	// Output: pool size: 1024, pipelining: true
}

func ExampleResilientConfig() {
	cfg := client.ResilientConfig()
	fmt.Printf("retries: %d, base delay: %v\n", cfg.MaxRetries, cfg.RetryBaseDelay)
	// Output: retries: 3, base delay: 200ms
}

func ExampleResponse_Header() {
	resp := &client.Response{
		StatusCode: 200,
		Headers: []client.Header{
			{Key: "Content-Type", Value: "application/json"},
			{Key: "X-Request-Id", Value: "abc123"},
		},
	}
	fmt.Println(resp.Header("content-type"))
	fmt.Println(resp.HasHeader("X-Request-Id"))
	// Output:
	// application/json
	// true
}

func ExampleResponse_HeaderValues() {
	resp := &client.Response{
		StatusCode: 200,
		Headers: []client.Header{
			{Key: "Set-Cookie", Value: "a=1"},
			{Key: "Set-Cookie", Value: "b=2"},
		},
	}
	for _, v := range resp.HeaderValues("Set-Cookie") {
		fmt.Println(v)
	}
	// Output:
	// a=1
	// b=2
}

func ExampleNewBuiltinMetrics() {
	m := client.NewBuiltinMetrics()
	cfg := client.DefaultConfig()
	cfg.Metrics = m

	fmt.Println("metrics configured")
	// Output: metrics configured
}

func ExampleBuiltinMetrics_Snapshot() {
	m := client.NewBuiltinMetrics()
	start := m.RecordRequest("GET", "example.com")
	m.RecordResponse("GET", "example.com", 200, nil, start, 50, 1024)
	m.RecordPoolEvent(client.PoolEventConnCreated, "example.com")

	snap := m.Snapshot()
	fmt.Printf("total: %d, ok: %d\n", snap.RequestsTotal, snap.RequestsOK)
	fmt.Printf("bytes written: %d, read: %d\n", snap.BytesWritten, snap.BytesRead)
	fmt.Printf("conns created: %d\n", snap.ConnsCreated)
	// Output:
	// total: 1, ok: 1
	// bytes written: 50, read: 1024
	// conns created: 1
}

func ExampleMultipartBuilder() {
	mb := client.NewMultipartBuilder()
	mb.AddField("username", "alice")
	mb.AddField("email", "alice@example.com")
	mb.AddFileFromBytes("avatar", "avatar.png", []byte("fake-png-data"))

	body, contentType, err := mb.Finish()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("has body: %v, has boundary: %v\n", len(body) > 0, len(contentType) > 0)
	// Output: has body: true, has boundary: true
}

func ExampleNewRetryer() {
	cfg := client.DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryBaseDelay = 100 * time.Millisecond
	cfg.RetryMaxDelay = 5 * time.Second
	cfg.RetryMultiplier = 2.0
	cfg.RetryJitter = true

	r := client.NewRetryer(cfg)
	if r != nil {
		fmt.Printf("backoff attempt 0: %v\n", r.Backoff(0) > 0)
		fmt.Printf("backoff attempt 2: %v\n", r.Backoff(2) > 0)
	}
	// Output:
	// backoff attempt 0: true
	// backoff attempt 2: true
}

func ExampleNewDNSCache() {
	cache := client.NewDNSCache(5 * time.Minute)
	defer cache.Stop()

	addrs, err := cache.LookupHost("localhost")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("resolved: %v\n", len(addrs) > 0)
	// Output: resolved: true
}

func ExampleClient_GracefulStop() {
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}

	drained := c.GracefulStop(5 * time.Second)
	fmt.Printf("drained: %v\n", drained)
	// Output: drained: true
}

func ExampleClient_Stats() {
	m := client.NewBuiltinMetrics()
	cfg := client.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true
	cfg.Metrics = m

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Stop()

	stats := c.Stats()
	fmt.Printf("healthy: %d, total requests: %d\n", stats.HealthyConnections, stats.Metrics.RequestsTotal)
	// Output: healthy: 0, total requests: 0
}

func ExampleGetVersion() {
	v := client.GetVersion()
	fmt.Printf("starts with v: %v\n", v[0] == 'v')
	// Output: starts with v: true
}

func ExampleDefaultConfig() {
	cfg := client.DefaultConfig()
	fmt.Printf("host: %s, pool: %d, pipelining: %v\n", cfg.Host, cfg.PoolSize, cfg.EnablePipelining)
	// Output: host: localhost, pool: 512, pipelining: true
}

func ExampleIsTimeout() {
	err := client.ErrTimeout
	fmt.Println(client.IsTimeout(err))
	fmt.Println(client.IsTimeout(nil))
	// Output:
	// true
	// false
}

func ExampleIsRetryable() {
	fmt.Println(client.IsRetryable(client.ErrConnectFailed))
	fmt.Println(client.IsRetryable(client.ErrInvalidURL))
	// Output:
	// true
	// false
}

func ExampleRequest_fluent() {
	req := client.AcquireRequest()
	defer client.ReleaseRequest(req)

	req.WithMethod("POST").
		WithPath("/api/v1/data").
		WithBody([]byte(`{"key":"value"}`)).
		WithHeader("Content-Type", "application/json").
		WithContext(context.Background())

	fmt.Printf("method: %s, path: %s\n", req.Method, req.Path)
	// Output: method: POST, path: /api/v1/data
}
