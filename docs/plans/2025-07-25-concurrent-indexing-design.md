# Concurrent File Indexing in IndexDirectory

**Issue:** #5
**Date:** 2025-07-25
**Status:** Approved

## Problem

`WithConcurrency(n)` is accepted but is a no-op. `IndexDirectory` processes files sequentially. The bottleneck is the `EmbedBatch` network call to Ollama per file — CPU-bound chunking and disk I/O are fast by comparison. Concurrent embedding requests would significantly reduce total indexing time for large directory trees.

## Design

### Approach: `errgroup.Group` + `SetLimit` as bounded worker pool

Use `golang.org/x/sync/errgroup` purely as a bounded worker pool. Do **not** use `errgroup.WithContext` — we collect all errors rather than fail-fast, so errgroup's context cancellation semantics don't apply. The parent `ctx` handles cancellation naturally through `IndexFile` → `EmbedBatch`.

### Structural change: write mutex on `Indexer`

Add `storeMu sync.Mutex` to `Indexer`. Lock it in `replaceSource` — the single funnel point for all store writes. This serializes only the fast DB write while file reads, chunking, and embedding API calls remain fully concurrent.

This makes `IndexFile` thread-safe as a public API, not just within `IndexDirectory`. Consumers (Firn IDE, Flux ML, Quantum Trader) can call it from multiple goroutines without external coordination.

### Two-phase `IndexDirectory`

**Phase 1 — Walk & Collect:** `filepath.Walk` collects eligible file paths into a `[]string` and walk-level errors into a separate slice. Separating discovery from processing keeps the walk callback simple and side-effect-free.

**Phase 2 — Concurrent Index:**

```go
var g errgroup.Group
g.SetLimit(cfg.concurrency)

for _, path := range files {
    if ctx.Err() != nil {
        break
    }
    g.Go(func() error {
        if err := idx.IndexFile(ctx, path); err != nil {
            mu.Lock()
            indexErrors = append(indexErrors, ...)
            mu.Unlock()
        }
        return nil // collect errors, don't fail-fast
    })
}
g.Wait()
```

Workers always return `nil`. Errors are collected in a mutex-protected slice and reported in aggregate — matching the current sequential behavior.

### Context cancellation

Three layers:
1. **Loop check** — `ctx.Err()` before each `g.Go` stops launching new workers.
2. **Worker propagation** — `IndexFile` passes `ctx` to `EmbedBatch`, which fails fast on cancelled context.
3. **Natural drain** — cancelled workers fail fast, releasing semaphore slots for remaining queued closures to also fail fast.

### Concurrency validation

Clamp `cfg.concurrency` to minimum 1 if caller passes 0 or negative.

### New dependency

`golang.org/x/sync` — quasi-stdlib package maintained by the Go team. Already consistent with the existing indirect `golang.org/x/exp` and `golang.org/x/sys` dependencies.

## Files to modify

| File | Change |
|------|--------|
| `rag/indexer.go` | Add `storeMu` to `Indexer`, lock in `replaceSource`, refactor `IndexDirectory` to two-phase concurrent, update `WithConcurrency` doc, add `sync` import |
| `rag/indexer_test.go` | Add concurrent indexing tests, cancellation test, error aggregation test, concurrency validation test |
| `go.mod` | Add `golang.org/x/sync` |
| `CLAUDE.md` | Add `golang.org/x/sync` to approved dependencies |

## Testing

1. **`TestIndexerConcurrentIndexDirectory`** — multiple files indexed, all present in store, works with various concurrency values
2. **`TestIndexerConcurrentContextCancellation`** — cancel during concurrent indexing, verify prompt stop
3. **`TestIndexerConcurrentErrorAggregation`** — mix valid/invalid files, verify all errors reported and valid files still indexed
4. **`TestIndexerConcurrencyValidation`** — `WithConcurrency(0)` and negative values clamped to 1

## What this design does NOT include

- Progress callbacks or `IndexStatus` reporting (not in issue scope)
- Per-file retry logic (YAGNI)
- Configurable error strategies (fail-fast vs collect-all)
