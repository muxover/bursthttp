# Contributing

## Getting started

- **Requirements:** Go 1.22+

```bash
git clone https://github.com/muxover/bursthttp.git
cd bursthttp
go build ./...
```

## Running tests

```bash
go test -race -count=1 ./...
```

All tests must pass with the race detector enabled before submitting a PR.

## Running benchmarks

```bash
go test -bench=. -benchmem -count=3 ./...
```

When submitting performance-related changes, include before/after benchmark results.

## Code style

- Run `gofmt` and `go vet` before committing. CI enforces both.
- Exported types, functions, and methods must have godoc comments.
- No dead code, commented-out blocks, or debug prints.
- One responsibility per function; clear responsibility per file.
- Error handling must be explicit — no swallowed errors.
- Prefer editing existing files over adding new ones.

## Submitting changes

1. Open an issue first for significant changes.
2. Fork and branch from `main` (e.g. `feat/your-feature`, `fix/your-fix`).
3. Add or update tests for any new or changed behavior.
4. Ensure CI checks pass (`go vet`, `go test -race`).
5. Open a PR with a clear description.

One logical change per PR.

## Reporting issues

Open an issue at [github.com/muxover/bursthttp/issues](https://github.com/muxover/bursthttp/issues). Include:

- Go version (`go version`)
- OS and architecture
- bursthttp version (or commit)
- Minimal reproducer
- Full error message or panic trace
