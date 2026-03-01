# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-03-01

### Added
- HTTP/1.1 client with persistent connection pooling and pipelining.
- Per-host connection pools with round-robin selection and idle eviction.
- Object pooling for requests and responses via `sync.Pool`.
- Fluent request builder (`WithMethod`, `WithPath`, `WithBody`, `WithHeader`, etc.).
- Convenience methods: `Get`, `Post`, `Put`, `Patch`, `Delete`, `Head`, `Options`.
- URL routing: `GetURL`, `PostURL`, `DoWithContext` with full URL parsing.
- Context cancellation and timeout support.
- Configurable retry with exponential backoff and jitter (`Retryer`).
- Retryable status codes (e.g. 429, 502, 503, 504).
- In-memory DNS cache with TTL (`DNSCache`).
- Pluggable metrics interface (`MetricsCollector`) with built-in implementation.
- Latency percentiles (p50, p95, p99), request counters, byte counters, pool events.
- HTTP CONNECT proxy with authentication.
- SOCKS5 proxy (RFC 1928) with username/password auth.
- Streaming response body as `io.ReadCloser` (`DoStreaming`).
- Streaming request body from `io.Reader` (`DoReader`).
- Multipart form-data builder (`MultipartBuilder`, `BuildMultipartRequest`).
- Gzip request compression and transparent response decompression.
- `Expect: 100-continue` support.
- TLS with session caching and configurable handshake timeout.
- Connection warm-up (`StartN`).
- Graceful shutdown with drain timeout (`GracefulStop`).
- Idle connection eviction with configurable interval.
- Connection rotation after N requests (`MaxRequestsPerConn`).
- Header injection protection (CRLF rejection).
- Configuration presets: `DefaultConfig`, `HighThroughputConfig`, `ResilientConfig`.
- Comprehensive test suite with race detector coverage.
- Benchmarks for client operations and internal components.
- Godoc examples for all major APIs.

[Unreleased]: https://github.com/muxover/bursthttp/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/muxover/bursthttp/releases/tag/v0.1.0
