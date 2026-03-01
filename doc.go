// Package client provides a high-performance HTTP/1.1 client with pipelining,
// per-host connection pooling, object reuse, retry, DNS caching, metrics,
// streaming, proxy support, and graceful shutdown. Zero external dependencies.
//
// Create a client with [NewClient] and a [Config] (or use [DefaultConfig],
// [HighThroughputConfig], [ResilientConfig] presets). Use convenience methods
// like [Client.Get] and [Client.Post] for common operations, or the low-level
// [Client.Do] / [Client.DoWithContext] for full control with pooled requests.
//
//	cfg := client.DefaultConfig()
//	cfg.Host = "api.example.com"
//	cfg.Port = 443
//	cfg.UseTLS = true
//
//	c, err := client.NewClient(cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer c.Stop()
//
//	resp, err := c.Get("/v1/users", nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer c.ReleaseResponse(resp)
package client
