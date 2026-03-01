# Contributing to bursthttp

Thank you for your interest in contributing!

---

## Table of Contents

- [Getting started](#getting-started)
- [Running tests](#running-tests)
- [Running benchmarks](#running-benchmarks)
- [Code style](#code-style)
- [Submitting changes](#submitting-changes)
- [Releasing](#releasing)
- [Reporting issues](#reporting-issues)

---

## Getting started

**Requirements:**
- Go 1.22+

```bash
git clone https://github.com/muxover/bursthttp.git
cd bursthttp
go build ./...
```

## Running tests

```bash
go test -v -race -count=1 ./...
```

All tests must pass with the race detector enabled before submitting a PR.

## Running benchmarks

```bash
go test -bench=. -benchmem -count=3 ./...
```

When submitting performance-related changes, include before/after benchmark results.

## Code style

- Run `gofmt` and `go vet` before committing. CI enforces both.
- All exported types, functions, and methods must have godoc comments.
- No dead code, commented-out blocks, or debug prints.
- Functions should do one thing. Files should have a clear responsibility.
- Error handling must be explicit — no swallowed errors.
- Prefer editing existing files over creating new ones.

## Submitting changes

1. **Open an issue first** for significant changes.
2. Fork and branch from `main`.
3. Add or update tests for any new or changed behavior.
4. Ensure CI checks pass (`go vet`, `go test -race`).
5. Open a PR with a clear description.

One logical change per PR.

## Releasing

1. Bump version in doc/API if needed; update `CHANGELOG.md` (move Unreleased items to the new version).
2. Add `release-notes/vX.Y.Z.md`.
3. Commit, then tag: `git tag vX.Y.Z` and `git push origin vX.Y.Z`.
4. Create a GitHub Release from the tag (optional; link to release-notes and changelog).

## Reporting issues

Open an issue at [github.com/muxover/bursthttp/issues](https://github.com/muxover/bursthttp/issues). Include:

- Go version (`go version`)
- OS and architecture
- Minimal reproducer
- Full error message or panic trace
