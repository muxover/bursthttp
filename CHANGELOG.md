# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.3] - 2026-03-27

### Added
- **Request scheduler**: New `Scheduler` type with per-host bounded queue and fixed worker goroutine pool. Replaces the 20×500µs spin-wait loop in `GetConnection` with a blocking channel — stable latency under overload, no busy-wait CPU burn. Opt-in via `config.EnableScheduler`; enabled in `HighThroughputConfig`. Configurable via `SchedulerWorkers` and `SchedulerQueueDepth`.
- **Async DNS**: In-flight deduplication (singleflight — concurrent misses for the same host share one lookup), background prefetch at 80% TTL, round-robin IP selection via atomic index, stale fallback on DNS errors, janitor at TTL/4 intervals.
- **Connection health scoring**: Per-connection latency EWMA (α=1/8) and rolling error rate (50-request window) produce a 0–100 health score. `getIdleConnection` scans up to 16 connections and returns the highest-scoring one; short-circuits on score=100. Gated by `config.EnableHealthScoring` (on by default).
- New config fields: `EnableHealthScoring`, `EnableScheduler`, `SchedulerWorkers`, `SchedulerQueueDepth`.
- `Connection.HealthScore()` public method returns the current health score.

### Changed
- `getIdleConnection` scan window increased from 8 to 16 connections; selection is now score-weighted rather than first-available.
- `HighThroughputConfig` enables `EnableHealthScoring` and `EnableScheduler`.

## [0.1.2] - 2026-03-11

### Added
- **Pre-encoded requests**: `Request.PreEncodedHeaderPrefix` and `Client.BuildPreEncodedHeaderPrefix(req, host, port, useTLS)` to cache the request header block and send multiple requests with the same headers without re-encoding; only the body may change between sends.
- **Lock-free connection pool**: Host pool lookup uses `sync.Map`; per-host connection list uses `atomic.Pointer[[]*Connection]` with CAS for add/remove so the get path is lock-free.
- **Header zero-copy**: Request headers are sent via `net.Buffers` (vectored write) so custom header bytes are not copied into the main write buffer; response headers support zero-copy access via `Response.HeaderBytes(key)` returning a slice into the raw header buffer (valid until `ReleaseResponse`).
- Tests: `TestHeaderBytesZeroCopy`, `TestPreEncodedHeaderPrefix`.

### Changed
- Connection write path uses `writeRequestPart1` + `req.headerBuf` + `writeRequestPart3` with `net.Buffers.WriteTo` for request header zero-copy when `PreEncodedHeaderPrefix` is not set.
- `parseHeaders` stores a copy of the header block in `Response.rawHeaderBuf` for `HeaderBytes()`.

## [0.1.1] - 2026-03-01

### Added
- `ErrInvalidRequest` sentinel error for nil request validation.
- Proxy rejection error now includes HTTP status code (e.g. `proxy CONNECT rejected (HTTP 407)`).
- Benchmarks: `BenchmarkWriteRequest`, `BenchmarkWriteRequestForwardProxy`, `BenchmarkReadResponse`, `BenchmarkViaHTTPProxy`.
- Tests: `TestProxyCONNECT407`, `TestIsForwardProxy`, `TestChunkedBodyWithTrailers`, `TestGracefulStopUnderLoad`, `TestMaxRequestsPerConn`, `TestErrInvalidRequest`, `TestIsTimeoutContextDeadline`, `TestIsTimeoutNetError`.

### Changed
- `IsTimeout` now uses `errors.Is` / `errors.As` — handles `ErrTimeout`, `context.DeadlineExceeded`, `*DetailedError`, and `net.Error.Timeout()`.
- `IsRetryable` now uses `errors.Is` / `errors.As` instead of direct comparison.
- `connection.go`: explicit `useTLS := tlsConfig != nil` instead of inlined `IsForwardProxy(tlsConfig != nil)`.
- `examples/basic/main.go`: User-Agent uses `bursthttp.GetVersion()` instead of hardcoded version string.
- `ExampleMultipartBuilder`: output check uses `len(body) > 0` and `len(contentType) > 0` (boundary is random).

### Fixed
- **parser.go**: `readChunkedBody` now drains all trailer headers after the terminal `0\r\n` chunk (RFC 7230 §4.1 compliance).
- **connection.go**: Removed 2-attempt retry loop in `connectLocked` — dial is single attempt (avoids doubling proxy timeouts, e.g. 30s → 60s per `createConnection`).
- **pool.go**: In `GetConnection`, if `createConnection()` returns nil after >1ms, return nil immediately instead of retrying (avoids 20× timeout cascade with slow or dead proxies).
- **client.go**: Nil request check now returns `ErrInvalidRequest` (was incorrectly `ErrInvalidResponse`).
- **client.go**: `DoReader` URL detection uses `strings.HasPrefix("http://")` / `strings.HasPrefix("https://")` instead of magic length checks.

### Removed
- Dead `ReaderRequest` struct and `AddHeader` method from `streaming.go` (unused).

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

[Unreleased]: https://github.com/muxover/bursthttp/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/muxover/bursthttp/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/muxover/bursthttp/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/muxover/bursthttp/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/muxover/bursthttp/releases/tag/v0.1.0
