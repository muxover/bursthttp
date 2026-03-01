# bursthttp

<div align="center">

[![Go Reference](https://pkg.go.dev/badge/github.com/muxover/bursthttp.svg)](https://pkg.go.dev/github.com/muxover/bursthttp)
[![Go Report Card](https://goreportcard.com/badge/github.com/muxover/bursthttp)](https://goreportcard.com/report/github.com/muxover/bursthttp)
[![CI](https://github.com/muxover/bursthttp/actions/workflows/ci.yml/badge.svg)](https://github.com/muxover/bursthttp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**High-performance HTTP/1.1 client for Go with pipelining and per-host connection pooling.**

[Features](#-features) • [Installation](#-installation) • [Quick Start](#-quick-start) • [API](#-api-overview) • [Configuration](#-configuration) • [Contributing](#-contributing) • [License](#-license)

</div>

---

bursthttp is a zero-dependency HTTP/1.1 client built for high-throughput workloads. It pipelines multiple requests over a single TCP connection, pools and reuses connections per host, and recycles request/response objects to minimize allocations. Built-in retry, DNS caching, metrics, streaming, proxy support, and graceful shutdown are included out of the box.

## ✨ Features

- **HTTP/1.1 Pipelining** — Send multiple requests on one connection without waiting for responses.
- **Per-Host Connection Pooling** — Persistent connections with configurable pool size and idle eviction.
- **Object Pooling** — `sync.Pool`-backed request/response reuse for near-zero allocation.
- **Retry with Exponential Backoff** — Configurable retries with jitter for transient failures.
- **DNS Caching** — In-memory resolution cache with TTL.
- **Pluggable Metrics** — Built-in collector with latency percentiles (p50/p95/p99), byte counters, pool events. Implement `MetricsCollector` for Prometheus/StatsD.
- **Streaming** — `io.ReadCloser` response bodies and `io.Reader` request bodies.
- **Multipart Builder** — Helpers for `multipart/form-data` uploads.
- **Proxy Support** — HTTP CONNECT and SOCKS5 (RFC 1928).
- **Gzip** — Transparent response decompression and request compression.
- **Graceful Shutdown** — Drain in-flight requests before closing.
- **Connection Warm-Up** — Pre-establish connections before first request.
- **URL Routing** — Route to multiple hosts from a single client.
- **Expect: 100-Continue** — Send headers first, body after server confirms.
- **Zero External Dependencies** — Pure Go stdlib.

## 📦 Installation

```bash
go get github.com/muxover/bursthttp
```

Requires **Go 1.22+**.

## 🚀 Quick Start

```go
package main

import (
    "fmt"
    "log"

    bursthttp "github.com/muxover/bursthttp"
)

func main() {
    cfg := bursthttp.DefaultConfig()
    cfg.Host = "httpbin.org"
    cfg.Port = 443
    cfg.UseTLS = true

    client, err := bursthttp.NewClient(cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer client.Stop()

    resp, err := client.Get("/get", nil)
    if err != nil {
        log.Fatal(err)
    }
    defer client.ReleaseResponse(resp)

    fmt.Printf("Status: %d\nBody: %s\n", resp.StatusCode, resp.Body)
}
```

## 📋 API Overview

### Client Lifecycle

| Method | Description |
|---|---|
| `NewClient(cfg)` | Create a client from config |
| `client.Stop()` | Close all connections immediately |
| `client.GracefulStop(timeout)` | Drain in-flight requests, then close |
| `client.StartN(n)` | Pre-establish `n` connections |

### Request Methods

| Method | Description |
|---|---|
| `Get(path, headers)` | GET request |
| `Post(path, body, headers)` | POST request |
| `Put(path, body, headers)` | PUT request |
| `Patch(path, body, headers)` | PATCH request |
| `Delete(path, headers)` | DELETE request |
| `Head(path, headers)` | HEAD request |
| `Options(path, headers)` | OPTIONS request |
| `GetURL(url, headers)` | GET to a full URL (auto-routed) |
| `PostURL(url, body, headers)` | POST to a full URL (auto-routed) |

### Low-Level API

| Method | Description |
|---|---|
| `Do(req)` | Execute a pooled request |
| `DoWithContext(ctx, req)` | Execute with context cancellation |
| `DoStreaming(ctx, req)` | Stream response body as `io.ReadCloser` |
| `DoReader(ctx, method, path, body, size, headers)` | Send `io.Reader` body |
| `AcquireRequest()` / `ReleaseRequest(req)` | Pool a request object |
| `ReleaseResponse(resp)` | Return response to pool |

### Response Helpers

| Method | Description |
|---|---|
| `resp.Header(key)` | Get first header value (case-insensitive) |
| `resp.HasHeader(key)` | Check header existence |
| `resp.HeaderValues(key)` | Get all values for a header |

### Observability

| Method | Description |
|---|---|
| `client.GetHealthyConnections()` | Count of healthy connections |
| `client.Stats()` | Snapshot of client state + metrics |
| `GetVersion()` | Library version string |

## ⚙️ Configuration

### Presets

| Preset | Pool | Pipeline | Retry | DNS Cache | Use Case |
|---|---|---|---|---|---|
| `DefaultConfig()` | 512 | Yes (10) | No | No | General purpose |
| `HighThroughputConfig()` | 1024 | Yes (16) | No | Yes | 100K+ RPS |
| `ResilientConfig()` | 512 | Yes (10) | 3 retries | Yes | Unreliable upstreams |

### Key Fields

| Field | Type | Default | Description |
|---|---|---|---|
| `Host` | `string` | `"localhost"` | Target hostname |
| `Port` | `int` | `80`/`443` | Target port |
| `UseTLS` | `bool` | `false` | Enable TLS |
| `PoolSize` | `int` | `512` | Max connections per host |
| `EnablePipelining` | `bool` | `true` | HTTP/1.1 pipelining |
| `MaxPipelinedRequests` | `int` | `10` | Pipeline depth per connection |
| `ReadTimeout` | `Duration` | `30s` | Response read timeout |
| `WriteTimeout` | `Duration` | `30s` | Request write timeout |
| `DialTimeout` | `Duration` | `10s` | TCP connect timeout |
| `IdleTimeout` | `Duration` | `0` | Idle connection eviction |
| `MaxRetries` | `int` | `0` | Retry attempts (0 = disabled) |
| `RetryBaseDelay` | `Duration` | `100ms` | Initial backoff delay |
| `EnableDNSCache` | `bool` | `false` | In-memory DNS cache |
| `DNSCacheTTL` | `Duration` | `5m` | DNS cache entry lifetime |
| `EnableCompression` | `bool` | `false` | Gzip request compression |
| `ProxyURL` | `string` | `""` | HTTP CONNECT proxy |
| `SOCKS5Addr` | `string` | `""` | SOCKS5 proxy address |
| `Metrics` | `MetricsCollector` | `nil` | Pluggable metrics backend |

## 🏗️ Architecture

```
Client
  ├── Pool (per-host connection pools)
  │     ├── Connection (pipelined or sequential)
  │     │     ├── Writer (request serialization)
  │     │     └── Parser (response parsing, chunked decoding)
  │     └── Idle Evictor
  ├── Dialer
  │     ├── DNS Cache
  │     ├── SOCKS5 Dialer
  │     └── HTTP CONNECT Proxy
  ├── TLS (session caching, handshake timeout)
  ├── Compressor (gzip)
  ├── Retryer (exponential backoff + jitter)
  └── Metrics Collector (pluggable)
```

## 🤝 Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Open an [issue](https://github.com/muxover/bursthttp/issues) or [pull request](https://github.com/muxover/bursthttp/pulls) on GitHub.

## 📄 License

Licensed under [MIT](LICENSE).

## 🔗 Links

- **Repository**: https://github.com/muxover/bursthttp
- **Issues**: https://github.com/muxover/bursthttp/issues
- **Changelog**: [CHANGELOG.md](CHANGELOG.md)
- **Go Reference**: https://pkg.go.dev/github.com/muxover/bursthttp

---

<div align="center">

Made with ❤️ by Jax (@muxover)

</div>
