# bursthttp

<div align="center">

[![Go Reference](https://pkg.go.dev/badge/github.com/muxover/bursthttp.svg)](https://pkg.go.dev/github.com/muxover/bursthttp)
[![Go Report Card](https://goreportcard.com/badge/github.com/muxover/bursthttp)](https://goreportcard.com/report/github.com/muxover/bursthttp)
[![CI](https://github.com/muxover/bursthttp/actions/workflows/ci.yml/badge.svg)](https://github.com/muxover/bursthttp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**High-performance Go HTTP/1.1 client with pipelining and pooling.**

</div>

---

bursthttp is a zero-dependency HTTP/1.1 client built for high-throughput workloads. It pipelines multiple requests over a single TCP connection, pools and reuses connections per host, and recycles request/response objects to minimize allocations. Built-in retry, DNS caching, metrics, streaming, proxy support, and graceful shutdown are included out of the box.

## Features

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
- **Pre-encoded requests** — Cache the header block with `BuildPreEncodedHeaderPrefix` and `Request.PreEncodedHeaderPrefix`; send many requests with the same headers without re-encoding.
- **Lock-free connection pool** — Host lookup via `sync.Map`, connection list via `atomic.Pointer` and CAS (no mutex on the get path).
- **Header zero-copy** — Request headers sent with vectored write (`net.Buffers`); response header values via `Response.HeaderBytes(key)` as a slice into the raw buffer.
- **Request Scheduler** — Opt-in per-host bounded queue with worker goroutine pool; replaces spin-wait with a proper blocking queue for stable latency under overload (`EnableScheduler`).
- **Async DNS** — In-flight deduplication (singleflight), background prefetch at 80% TTL, round-robin IP selection, stale fallback on errors, negative cache for failed lookups (`DNSNegativeTTL`).
- **Connection Health Scoring** — Per-connection latency EWMA and error rate produce a 0–100 score; pool selection prefers higher-scoring connections (`EnableHealthScoring`).
- **Pipeline Auto-Tuning** — Pipeline depth adjusts per connection based on measured latency: fast servers get deeper pipelines, slow servers get shallower ones (`EnablePipelineAutoTune`).
- **TCP socket tuning** — `TCPFastOpen` and `TCPReusePort` on Linux reduce connection setup cost.
- **Zero External Dependencies** — Pure Go stdlib.

## Installation

```bash
go get github.com/muxover/bursthttp
```

Requires **Go 1.22+**.

## Quick Start

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

## API Overview

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
| `resp.HeaderBytes(key)` | Get first header value as `[]byte` (zero-copy) |

### Observability

| Method | Description |
|---|---|
| `client.GetHealthyConnections()` | Count of healthy connections |
| `client.Stats()` | Snapshot of client state + metrics |
| `GetVersion()` | Library version string |

## Configuration

### Presets

| Preset | Pool | Pipeline | Retry | DNS Cache | Scheduler | Health Scoring | Auto-Tune | Use Case |
|---|---|---|---|---|---|---|---|---|
| `DefaultConfig()` | 512 | Yes (10) | No | No | No | Yes | No | General purpose |
| `HighThroughputConfig()` | 1024 | Yes (auto) | No | Yes | Yes | Yes | Yes | 100K+ RPS |
| `ResilientConfig()` | 512 | Yes (10) | 3 retries | Yes | No | Yes | No | Unreliable upstreams |

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
| `WriteTimeout` | `Duration` | `10s` | Request write timeout |
| `DialTimeout` | `Duration` | `10s` | TCP connect timeout |
| `IdleTimeout` | `Duration` | `90s` | Idle connection eviction |
| `MaxRetries` | `int` | `0` | Retry attempts (0 = disabled) |
| `RetryBaseDelay` | `Duration` | `100ms` | Initial backoff delay |
| `EnableDNSCache` | `bool` | `false` | In-memory DNS cache |
| `DNSCacheTTL` | `Duration` | `5m` | DNS cache entry lifetime |
| `EnableCompression` | `bool` | `false` | Gzip request compression |
| `ProxyURL` | `string` | `""` | HTTP CONNECT proxy |
| `SOCKS5Addr` | `string` | `""` | SOCKS5 proxy address |
| `Metrics` | `MetricsCollector` | `nil` | Pluggable metrics backend |
| `EnableHealthScoring` | `bool` | `true` | Per-connection latency/error scoring |
| `EnableScheduler` | `bool` | `false` | Per-host request queue + worker pool |
| `SchedulerWorkers` | `int` | `0` (=PoolSize) | Worker goroutines per host |
| `SchedulerQueueDepth` | `int` | `0` (=workers×4) | Max queued requests per host |
| `EnablePipelineAutoTune` | `bool` | `false` | Auto-adjust pipeline depth by latency |
| `TCPFastOpen` | `bool` | `false` | Linux: TCP Fast Open on connect |
| `TCPReusePort` | `bool` | `false` | Linux: SO_REUSEPORT on sockets |
| `DNSNegativeTTL` | `Duration` | `5s` | Cache duration for failed DNS lookups |

## Architecture

```
Client
  ├── Scheduler (optional, per-host queue + worker pool)
  ├── Pool (per-host connection pools)
  │     ├── Connection (pipelined or sequential)
  │     │     ├── Health Scoring (latency EWMA + error rate)
  │     │     ├── Pipeline Auto-Tuning (dynamic depth by latency)
  │     │     ├── Writer (request serialization)
  │     │     └── Parser (response parsing, chunked decoding)
  │     └── Idle Evictor
  ├── Dialer
  │     ├── DNS Cache (async, prefetch, round-robin, negative cache)
  │     ├── TCP tuning (FastOpen, ReusePort — Linux)
  │     ├── SOCKS5 Dialer
  │     └── HTTP CONNECT Proxy
  ├── TLS (session caching, handshake timeout)
  ├── Compressor (gzip)
  ├── Retryer (exponential backoff + jitter)
  └── Metrics Collector (pluggable)
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Licensed under [MIT](LICENSE).

## Links

- Repository: https://github.com/muxover/bursthttp
- Issues: https://github.com/muxover/bursthttp/issues
- Changelog: [CHANGELOG.md](CHANGELOG.md)
- Go Reference: https://pkg.go.dev/github.com/muxover/bursthttp

---

<p align="center">Made with ❤️ by Jax (@muxover)</p>
