// Basic usage of bursthttp.
package main

import (
	"context"
	"fmt"
	"time"

	bursthttp "github.com/muxover/bursthttp"
)

func main() {
	cfg := bursthttp.DefaultConfig()
	cfg.Host = "httpbin.org"
	cfg.Port = 443
	cfg.UseTLS = true

	client, err := bursthttp.NewClient(cfg)
	if err != nil {
		fmt.Printf("Failed to create client: %v\n", err)
		return
	}
	defer client.GracefulStop(5 * time.Second)

	// Pre-warm 4 connections so the first requests don't pay TCP+TLS cost.
	if err := client.StartN(4); err != nil {
		fmt.Printf("Warm-up failed (non-fatal): %v\n", err)
	}

	fmt.Println("1. GET request")
	resp, err := client.Get("/get", []bursthttp.Header{
		{Key: "User-Agent", Value: "bursthttp/" + bursthttp.GetVersion()},
	})
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
	} else {
		fmt.Printf("   Status: %d, Body: %d bytes\n", resp.StatusCode, len(resp.Body))
		fmt.Printf("   Content-Type: %s\n", resp.Header("Content-Type"))
		client.ReleaseResponse(resp)
	}

	fmt.Println("\n2. POST request")
	resp, err = client.Post("/post", []byte(`{"hello":"world"}`), []bursthttp.Header{
		{Key: "Content-Type", Value: "application/json"},
	})
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
	} else {
		fmt.Printf("   Status: %d, Body: %d bytes\n", resp.StatusCode, len(resp.Body))
		client.ReleaseResponse(resp)
	}

	fmt.Println("\n3. Context with 2s timeout")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := client.AcquireRequest()
	req.WithMethod("GET").
		WithPath("/delay/1").
		WithHeader("Accept", "application/json")

	resp, err = client.DoWithContext(ctx, req)
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
	} else {
		fmt.Printf("   Status: %d\n", resp.StatusCode)
		client.ReleaseResponse(resp)
	}
	client.ReleaseRequest(req)

	fmt.Println("\n4. Full URL routing")
	resp, err = client.GetURL("https://httpbin.org/ip", nil)
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
	} else {
		fmt.Printf("   Status: %d, Body: %s\n", resp.StatusCode, string(resp.Body))
		client.ReleaseResponse(resp)
	}

	fmt.Println("\n5. Request reuse (3 calls)")
	req = client.AcquireRequest()
	defer client.ReleaseRequest(req)
	req.Method = "GET"
	req.Path = "/get"
	req.SetHeader("User-Agent", "bursthttp/0.1.0")

	for i := 1; i <= 3; i++ {
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("   Request %d: Error: %v\n", i, err)
			continue
		}
		fmt.Printf("   Request %d: Status=%d\n", i, resp.StatusCode)
		client.ReleaseResponse(resp)
	}

	fmt.Printf("\n6. Healthy connections: %d\n", client.GetHealthyConnections())
}
