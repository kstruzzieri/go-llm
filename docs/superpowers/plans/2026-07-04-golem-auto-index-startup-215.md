# Golem Auto-Index Startup (#215) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Golem auto-build and refresh the private RAG index on REPL startup,
without blocking the prompt, while preserving manual control paths.

**Architecture:** Register a mutex-guarded `readyRetrieve` wrapper in default
auto mode, run one background indexing job, then hot-swap the wrapper to a
read-only retriever after the writer closes. Reuse `executeIndex`,
`buildGatedRetriever`, sidecars, and vector-space gates. Add one small opt-in
RAG prune path because #215 requires deleted sources to be dropped.

**Tech Stack:** Go stdlib, existing `agent`, `agent/tools`, `cmd/golem`, and
`rag` packages.

**Spec:** `docs/superpowers/specs/2026-07-04-golem-auto-index-startup-215-design.md`

**Conventions:**
- Run shell commands through `rtk`.
- Run Go commands as `env -u GOROOT go ...` on this machine.
- Keep commits small. No emoji.
- This plan has exactly 7 tasks.

---

## Review Findings Folded Into This Plan

1. Current code does not prune deleted files from an incremental directory
   refresh. Task 2 adds the missing opt-in prune path.
2. `executeIndex` returns `errIndexFailed` for partial-but-usable indexes. Task 5
   verifies readiness by opening retrieval after the run, not by checking only
   `exitErr`.
3. The ready gate must be mutex/RWMutex guarded because #235 can run read-only
   tools in parallel.
4. The embed probe must call the chain embedder, not provider health.
5. Self-heal deletes only private auto artifacts, never explicit user paths.
6. Auto-ready retrieval must keep any `behavioralWeighterHandle` returned by
   `buildGatedRetriever` open until shutdown.

---

### Task 1: Add the `-no-auto-index` flag and mode selection

**Files:**
- Modify: `cmd/golem/main.go`
- Test: `cmd/golem/main_test.go`

- [ ] Add `noAutoIndex bool` to `flags`.
- [ ] Register `-no-auto-index` in `parseFlags` with help text:
  `disable startup auto-index refresh; existing auto indexes may still be used`.
- [ ] Add tests that:
  - `parseFlags([]string{"-no-auto-index"})` sets `noAutoIndex`;
  - `-p` remains valid and implies no background auto-index later;
  - `-no-auto-index` does not conflict with `-rag-db` or `-no-rag`.
- [ ] Add a tiny helper in `main.go`:

```go
func shouldStartAutoIndex(f flags) bool {
	return !f.promptSet && !f.noAutoIndex && !f.noRag && f.ragDB == ""
}
```

- [ ] Test the helper for default, `-p`, `-no-auto-index`, `-no-rag`, and
  `-rag-db`.
- [ ] Run:

```bash
rtk env -u GOROOT go test ./cmd/golem -run 'NoAutoIndex|AutoIndex' -v
```

Expected: targeted tests pass.

---

### Task 2: Add opt-in deleted-source pruning to RAG directory indexing

**Files:**
- Modify: `rag/indexer.go`
- Modify: `rag/sqlite_store.go`
- Test: `rag/indexer_incremental_test.go`
- Test: `rag/sqlite_store_test.go`

- [ ] Add a private config field and option:

```go
func WithPruneDeleted() IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.pruneDeleted = true
	}
}
```

- [ ] Add `(*SQLiteStore).ListSources(ctx context.Context) ([]string, error)`:

```sql
SELECT DISTINCT source FROM chunks ORDER BY source
```

- [ ] In `IndexDirectoryWithStatus`, after file indexing completes and before
  the final error return, if `cfg.pruneDeleted` is true and the store supports
  `ListSources`, remove stored sources under the workspace root that are absent
  from the eligible `files` set.
- [ ] Do not prune if the directory walk itself fails. Per-file embed errors are
  okay: eligible failed files remain in the keep set.
- [ ] Add tests:
  - `ListSources` returns distinct sorted source paths.
  - `WithPruneDeleted` removes a deleted file after an incremental refresh.
  - `WithPruneDeleted` removes a now-ignored/excluded source.
  - default `WithIncremental()` without prune preserves deleted sources, so
    existing manual semantics remain explicit.
- [ ] Run:

```bash
rtk env -u GOROOT go test ./rag -run 'ListSources|PruneDeleted|WithIncremental' -v
```

Expected: targeted tests pass.

---

### Task 3: Add the mutex-guarded `readyRetrieve` wrapper

**Files:**
- Create: `cmd/golem/ready_retrieve.go`
- Test: `cmd/golem/ready_retrieve_test.go`

- [ ] Implement a small tri-state wrapper:

```go
type retrieveReadyState int

const (
	retrieveWarming retrieveReadyState = iota
	retrieveReady
	retrieveFailed
)

type readyRetrieve struct {
	mu      sync.RWMutex
	state   retrieveReadyState
	tool    agent.Tool
	feedback *behavioralWeighterHandle
	message string
}
```

- [ ] `Spec` returns the normal `retrieve` spec. Use an empty
  `agenttools.Retrieve{}` value only for the spec/effect shape; do not build a
  retriever.
- [ ] `Effect` returns read-only approval-never.
- [ ] `Invoke`:
  - warming: returns `ToolResult{Content: "...use read_file/search/glob/list..."}`
    with `IsError:false`;
  - failed: returns the stored failure message with `IsError:false`;
  - ready: copies the delegate under lock, unlocks, delegates.
- [ ] Add `markReady(tool agent.Tool, message string)` and
  `markFailed(message string)`. If the implementation passes feedback through
  `markReady`, store it on the wrapper.
- [ ] Add `close()` on the wrapper that closes a retained feedback DB handle.
- [ ] Tests:
  - warming response is non-error and names file tools;
  - failed response is non-error and names file tools;
  - ready delegates exactly once;
  - close closes a retained feedback DB handle;
  - a concurrent mark-ready/invoke loop is race-clean.
- [ ] Run:

```bash
rtk env -u GOROOT go test -race ./cmd/golem -run ReadyRetrieve -v
```

Expected: targeted race test passes.

---

### Task 4: Add exact-path embed probe and auto self-heal classifier

**Files:**
- Create: `cmd/golem/auto_index.go`
- Test: `cmd/golem/auto_index_test.go`

- [ ] Add `probeAutoIndexEmbedder(ctx, embedder, model)`:
  - creates a 30s child context;
  - calls `embedder.Embed(child, model, []string{"golem startup index probe"})`;
  - requires exactly one returned vector;
  - returns an error that is safe to show in a startup warning.
- [ ] Add a classifier that decides whether auto refresh should run incremental
  or full:
  - absent DB: incremental is fine on an empty store;
  - present DB with missing/corrupt/invalid sidecar: full rebuild, notice reason;
  - present DB with vector-space mismatch/inconsistent gate: full rebuild,
    notice reason;
  - present DB with compatible sidecar/vector-space: incremental.
- [ ] The classifier must only operate on `autoDBPath`. It must not be reused for
  `-rag-db`.
- [ ] Use `rag.OpenSQLiteStoreReadOnly` for vector-space classification, matching
  existing preflight behavior that avoids creating WAL/SHM.
- [ ] Tests:
  - probe calls the supplied embedder with one input and configured model;
  - probe times out/fails as an ordinary error;
  - missing/corrupt/wrong-workspace sidecar selects full rebuild;
  - vector-space mismatch selects full rebuild;
  - compatible sidecar selects incremental.
- [ ] Run:

```bash
rtk env -u GOROOT go test ./cmd/golem -run 'AutoIndex|ProbeAutoIndex' -v
```

Expected: targeted tests pass.

---

### Task 5: Implement the background auto-index runner

**Files:**
- Modify: `cmd/golem/auto_index.go`
- Test: `cmd/golem/auto_index_test.go`

- [ ] Add a small options struct for testable dependencies:

```go
type autoIndexJob struct {
	root        string
	dbPath      string
	sidecarPath string
	workspaceID string
	cfg         *config.Config
	router      *provider.Router
	embedder    rag.Embedder
	embChain    []string
	feedbackDB  string
	ready       *readyRetrieve
	notice      func(string)
}
```

- [ ] Build the write store with `prepareIndexStore`, passing `full=true` only
  when the self-heal classifier selected a rebuild.
- [ ] Build the indexer with:

```go
rag.NewIndexerWithEmbedder(embedder, store, rag.WithEmbeddingModel(embChain[0]))
```

- [ ] Reuse `executeIndex` with a `bytes.Buffer` for output and pass
  `rag.WithPruneDeleted()` by adding a small field to `indexJob` such as
  `pruneDeleted bool` that appends the option in `executeIndex`.
- [ ] Close the write store before opening read-only retrieval.
- [ ] Call `buildGatedRetriever` after the writer closes.
- [ ] Retain any `feedback` handle returned by `buildGatedRetriever` by storing
  it on `readyRetrieve`; do not close it immediately or behavioral ranking will
  lose its DB.
- [ ] Treat readiness like this:
  - retriever opens: `ready.markReady(tool, noticeLine)`;
  - sidecar status partial: ready plus warning notice;
  - retriever cannot open: `ready.markFailed(...)`;
  - embed probe fails: `ready.markFailed(...)` and do not write.
- [ ] Cap failure messages to one line; include the first useful error from the
  buffer or returned error.
- [ ] Tests:
  - first run builds and marks ready;
  - partial usable run marks ready and emits warning;
  - embedder-down marks failed and does not create/write the DB;
  - invalid private sidecar is removed via full rebuild;
  - writer closes before read-only retriever opens.
- [ ] Run:

```bash
rtk env -u GOROOT go test ./cmd/golem -run AutoIndex -v
```

Expected: targeted tests pass.

---

### Task 6: Wire default REPL startup to the ready gate

**Files:**
- Modify: `cmd/golem/main.go`
- Modify: `cmd/golem/retrieve_enable.go` only if helper signatures need small
  reuse
- Test: `cmd/golem/main_test.go`

- [ ] In `run`, after resolving the auto index path and feedback DB:
  - if `shouldStartAutoIndex(f)` is false, keep the existing `enableRetrieve`
    path exactly;
  - if true, create `ready := newReadyRetrieve("retrieve index warming...")`,
    set `retrieve = ready`, set `retrieveLine = "retrieve: auto-index warming in background"`,
    and start the background job.
- [ ] Defer `ready.close()` in the auto path so any feedback DB opened after the
  background job completes is closed during normal shutdown.
- [ ] The background job should receive a notice function that writes one line to
  stderr. It is okay if this appears while the REPL is live; keep it short.
- [ ] Preserve existing behavior for:
  - `-no-rag`;
  - explicit `-rag-db`;
  - `-p`;
  - `-no-auto-index`;
  - auto path resolution failure.
- [ ] Startup notice rules:
  - default auto mode should not print the generic "retrieve unavailable" line;
  - `-no-auto-index` with no existing index should print today's generic line;
  - auto path resolution failure should warn and fall back to file tools.
- [ ] Tests:
  - default REPL includes the wrapper tool and warming startup line;
  - `-no-auto-index` uses immediate-open behavior;
  - `-p` does not start auto job;
  - explicit `-rag-db` path unchanged.
- [ ] Run:

```bash
rtk env -u GOROOT go test ./cmd/golem -run 'Startup|NoAutoIndex|OneShot|Retrieve' -v
```

Expected: targeted tests pass.

---

### Task 7: Final race, integration, and docs check

**Files:**
- Modify tests only as needed from failures.
- Update user-facing docs only if help text or behavior needs a short mention.

- [ ] Run focused packages with race:

```bash
rtk env -u GOROOT go test -race ./cmd/golem ./agent ./rag
```

Expected: all pass.

- [ ] Run the full test suite:

```bash
rtk env -u GOROOT go test ./...
```

Expected: all pass.

- [ ] Run vet:

```bash
rtk env -u GOROOT go vet ./...
```

Expected: clean.

- [ ] Manual smoke, if an embedding backend is available:

```bash
rtk env -u GOROOT go run ./cmd/golem -root . -no-session -no-compress
```

Expected: startup shows `retrieve: auto-index warming in background`, then a
ready or failed notice. REPL remains usable either way.

- [ ] Confirm docs and code agree:
  - `-p` skips the writer but can still use an existing valid index;
  - `-no-auto-index` restores immediate-open behavior;
  - auto self-heal never touches explicit `-rag-db`;
  - deleted-source prune is enabled for auto refresh.
