# Golem auto-build + incremental RAG re-index on startup (#215)

Date: 2026-07-04
Issue: #215
Base: develop@c88e45b
Lineage: #202 (`golem index` subcommand), #225-#231 (RAG quality track), #235 (parallel read-only dispatch)

## 1. Problem

The RAG index is only built by the explicit `golem index` subcommand. A first
`golem` run in an un-indexed repo silently omits the `retrieve` tool
(`os.Stat(autoDBPath)` miss in `cmd/golem/retrieve_enable.go`), and an existing
index is a point-in-time snapshot that goes stale after edits until the user
remembers to re-run `golem index`. The rag engine already supports incremental
indexing (`rag.WithIncremental`, content-hash `sourceSignature` with
embedding-model awareness, chunk-diff, stable keys); making indexing automatic
is Golem-side orchestration.

## 2. Goals

- On startup, auto-build the index if absent and incrementally refresh it if
  present, in the background, without blocking the REPL.
- `retrieve` registers as a ready-gated tool that reports "warming" until the
  background build completes, then serves; a notice announces the transition.
- Embedder unavailable => clear warning, file tools still work, no crash.
- `-no-auto-index` opt-out; explicit `golem index`, `-rag-db`, `-no-rag`
  unchanged.

## 3. Non-goals

- No file-watching / live re-index during a session (warm once at startup).
- No change to the `rag` incremental engine.
- No multi-writer / cross-process index locking (see 9. Concurrency).
- No change to `golem index` semantics (it still requires explicit `-full` for
  torn or mismatched stores; the AUTO path is allowed to self-heal because the
  DB lives in golem's private data dir).

## 4. Current state (verified against develop@c88e45b)

- `cmd/golem/main.go run()`: computes `autoDBPath`/`workspaceID` via
  `indexDBPathForWorkspace`, then calls `enableRetrieve` once, synchronously.
  `retrieve == nil` => `retrieveOmitted`, generic "retrieve unavailable" notice.
- `enableRetrieve` (retrieve_enable.go): auto-discovery requires DB + valid
  sidecar, then `buildGatedRetriever` opens the store with
  `rag.OpenSQLiteStoreReadOnly` (`immutable=1`) — the open MUST NOT be
  concurrent with a writer.
- `runIndex`/`executeIndex` (index.go): `prepareIndexStore` (preflight existing
  index: valid sidecar + vector-space match, else refuse) -> `IndexDirectoryWithStatus(ctx,
  root, rag.WithIncremental(), rag.WithExclude(indexExcludes...))` -> usable-store
  gate -> `chmodIndexDBFiles` -> sidecar write. First build IS the incremental
  path on an empty store.
- `agenttools.Retrieve`: `Spec()` and `Effect()` are static (no field
  dependence) — a wrapper can mirror them exactly.
- `replSession.tools` is fixed for the process; #235 dispatches read-only tools
  in parallel, so a late-binding tool must be race-safe.
- One-shot `-p` mode forces no-session/no-compress/no-memory and exits after a
  single turn.

## 5. Design

### 5.1 Approaches considered

1. **Background warm + ready-gated wrapper tool (chosen).** REPL usable
   immediately; `retrieve` is registered up front as a wrapper whose inner tool
   is swapped in when the background build finishes. Matches the decided design
   in the issue.
2. Blocking foreground refresh with progress output. Rejected: violates the
   non-blocking goal; a cold build on a big repo would stall the first prompt
   for minutes.
3. Serve the stale index immediately (read-only) while rebuilding into a temp
   DB, atomic-rename swap on completion. Rejected: the issue explicitly decided
   warm-once-then-static; double disk, rename-under-immutable-handle
   complexity, and stale results served silently in the interim.

### 5.2 Components

New file `cmd/golem/autoindex.go` (+ `_test.go`), new file
`cmd/golem/retrieve_ready.go` (+ `_test.go`). Changes to `main.go`, `repl.go`
(one line in `/tools`), flag plumbing.

**a) `readyRetrieve` wrapper (retrieve_ready.go)** — implements `agent.Tool`.

- `Spec()`/`Effect()`: return `agenttools.Retrieve{}.Spec()` / `.Effect()`
  verbatim (static), so `toolSchemaHash` is stable across the swap.
- Tri-state: `warming` -> `ready(inner agent.Tool)` | `failed(reason)`.
  State guarded by a mutex (or `atomic.Pointer` + atomic state word); Invoke may
  be called from parallel dispatch goroutines (#235).
- `Invoke` while `warming`: returns `agent.ToolResult{IsError: false, Content:
  "retrieve: the workspace index is still warming in the background; use the
  file/search tools for now and retry retrieve later in this session."}`.
  Non-error: an unavailable-yet capability is a normal observation, and an
  error result would push some models to abandon the tool for the session.
- `Invoke` while `failed`: same shape, content "retrieve: index unavailable:
  <reason>; using file tools" (IsError: false).
- `Invoke` while `ready`: delegate to inner tool.
- `SetReady(tool agent.Tool)`, `SetFailed(reason string)`, `StateLine() string`
  (for `/tools` and the completion notice).

**b) Auto-index decision (autoindex.go)** — two stages.

Stage 1, synchronous at startup (pure function `autoIndexMode(f flags,
autoErr error, embChainErr error) (on bool, skipReason string)`):

- OFF when any of: `-no-rag`, `-rag-db` set, `-no-auto-index`, one-shot
  (`f.promptSet`), `autoErr != nil` (no resolvable index path), embedding chain
  unresolvable (no `defaults.embedding`).
- Each OFF cause keeps today's behavior exactly (see 5.4 matrix): the flow
  falls through to the existing `enableRetrieve` path, whose warnings/notices
  (including the generic "retrieve unavailable" line and the autoErr warning)
  already cover every OFF case. `autoIndexMode` therefore returns only a bool.

Stage 2, inside the background goroutine (`runAutoIndex`), in order:

1. **Embedder probe.** One 1-input embed through the same `newChainEmbedder`
   the indexer will use (input: a short constant string), bounded by a
   30-second timeout (model cold-load headroom; configurable only by code
   constant). Failure => `SetFailed("embedder unavailable: <err>")` + warning
   notice; goroutine ends. This is the "Health() or equivalent" probe from the
   issue: it exercises routing + model availability on the exact path indexing
   uses, provider-agnostic.
2. **Store preparation with self-heal.** `prepareAutoIndexStore`:
   - DB absent: `prepareIndexStore(..., full=false)` (fresh store; first build
     is incremental-on-empty).
   - DB present: try the existing `preflightExistingIndex`. On success open
     write store (incremental refresh). On preflight failure (torn sidecar,
     workspace mismatch, vector-space mismatch, probe failure): **self-heal**
     — `removeIndexArtifacts` + fresh store + notice ("rebuilding index:
     <reason>"). Safety argument: this applies ONLY to `autoDBPath`, which
     lives under golem's private data dir (`<base>/golem/indexes/<sha16>.db`,
     validated outside the workspace) and is keyed to this workspace; the
     explicit `golem index` command keeps requiring `-full` so destruction in
     the manual path stays explicit. An interrupted background build (torn
     state) therefore heals itself on the next startup instead of demanding a
     manual `-full`. A changed embedding model rebuilds the whole corpus, which
     is what a vector-space flip means semantically; the vs gate is honored
     because the store is never served or appended to across spaces.
3. **Index run.** Reuse `executeIndex` verbatim with the job's `out` set to a
   `bytes.Buffer` (its summary/errors are not interleaved into the REPL).
   `ctx` is the warmer's cancellable context, so REPL exit aborts the walk.
4. **Outcome classification.** After the run, re-read the sidecar:
   - sidecar valid now => the store is usable (executeIndex only writes it
     behind the usable-store gate). Proceed to open. If `executeIndex`
     returned `errIndexFailed` with a valid sidecar, the run was PARTIAL:
     serve + warning notice mirroring today's partial `autoLine`.
   - sidecar invalid/absent => build failed before usability:
     `SetFailed(<first buffer line or indexErr>)` + warning notice with the
     buffered output capped to a few lines.
   - ctx canceled => exit silently (process is shutting down).
5. **Open-after-write.** Only now call `buildGatedRetriever(ctx, cfg, router,
   autoDBPath, expected, feedbackDB)` — the immutable read-only open is
   sequenced strictly after the writer store is closed. Success =>
   `SetReady(tool)`, inject behavioral weighter handle ownership into the
   warmer, emit ready notice built from the store stats/sidecar (same shape as
   today's `autoLine`). Failure => `SetFailed` + warning.

**c) `indexWarmer` lifecycle (autoindex.go).**

- Holds: cancel func, done channel, notify `func(string)` (writes lines to
  stderr; safe for async use), the feedback handle once opened, and the
  `readyRetrieve` wrapper.
- `Close()`: cancel, wait for the goroutine, close the feedback DB handle if
  set. Called via defer in `run()` before the provider bundle closes. The
  read-only store itself follows the existing precedent: process-lifetime,
  closed by the OS at exit.
- The write store is always closed inside the goroutine before the read-only
  open (defer + explicit close before open).

### 5.3 Wiring in main.go

- New flag: `-no-auto-index` ("disable automatic background index build/refresh
  on startup; an existing index is still used as-is"). No validateFlags
  coupling: it composes harmlessly with every other flag (it only narrows
  behavior).
- Flow replaces the current single `enableRetrieve` call site:
  - auto mode OFF => exactly today's path (`enableRetrieve`, immediate
    read-only open of an existing valid index).
  - auto mode ON => do NOT open the store at startup (even when present:
    warm-once-then-static; the immutable open must not race the refresh
    writer). Register the `readyRetrieve` wrapper in the tool list,
    `retrieveOmitted = false`, startup notice one of:
    - "retrieve: building workspace index in the background (first build)"
      (DB absent)
    - "retrieve: refreshing workspace index in the background" (DB present)
    The generic "retrieve unavailable: no RAG index configured" notice never
    fires in auto mode.
  - Start the goroutine after startup notices are printed (the warmer only
    emits async lines).
- `replSession` gains the warmer handle (nil when auto mode off) so `/tools`
  can print `warmer.wrapper.StateLine()`; `retrieveOmitted` semantics for
  non-auto paths unchanged.
- Async notices write to stderr as they happen. They may interleave with the
  `golem> ` prompt visually; accepted (startup warnings already share the
  terminal), and queuing them to the next prompt would delay them arbitrarily
  because the REPL blocks on ReadLine.

### 5.4 Behavior matrix

| Condition | Behavior |
|---|---|
| default, no index | wrapper registered (warming), background full build, ready notice on completion |
| default, valid index, unchanged repo | wrapper (warming), background incremental refresh (hash checks only), near-instant ready |
| default, valid index, edits | wrapper (warming), only new/modified files re-embedded, deleted dropped |
| default, torn state (DB without valid sidecar) | self-heal: remove artifacts, full rebuild, notice |
| default, vector-space mismatch (embedding model changed) | self-heal: remove artifacts, full rebuild, notice |
| embedder down | probe fails: warning notice, wrapper -> failed, file tools unaffected |
| `-no-auto-index` | today's behavior byte-for-byte (existing valid index opens immediately; none => omitted notice) |
| `-no-rag` | unchanged (no retrieve at all) |
| `-rag-db <path>` | unchanged (explicit DB, no auto anything) |
| `-p` one-shot | auto-index skipped (process too short-lived; a mid-write kill would tear the store) |
| no `defaults.embedding` | auto-index off; today's behavior (generic "retrieve unavailable" notice) |
| REPL exits mid-build | warmer ctx canceled, goroutine drains; torn store self-heals next startup |
| partial run (per-file errors, usable store) | serve + partial warning (mirrors `autoLine` partial shape) |

### 5.5 Error handling

- Every failure path lands in exactly one of: startup warning (sync),
  `SetFailed` + async warning notice, or silent exit on cancellation. The
  goroutine never panics the process: it only touches injected deps and
  recovers nothing (a panic in rag/sqlite code is a real bug we want loud —
  no blanket recover).
- Buffered `executeIndex` output is surfaced only on failure/partial, capped.

### 5.6 Concurrency

- Wrapper: mutex-guarded state; Invoke concurrent-safe (#235 parallel
  dispatch); `go test -race` covers via a dedicated concurrent test.
- Exactly one writer goroutine per process. Two golem processes on the same
  workspace can race on the same DB file; SQLite locking makes this safe
  (worst case: one refresh fails busy => failed state, that session uses file
  tools). Documented limitation, per issue non-goal.
- Warmer goroutine must not outlive `run()`: `Close()` waits on the done
  channel (bounded by ctx cancellation propagating into the indexer walk and
  the embed probe).

### 5.7 Testing (unit; no live embedder)

Existing patterns: fake embedder via pre-built indexer/store (index_test.go),
temp data dir via getenv injection.

1. `autoIndexMode` matrix: every OFF cause + ON case (pure, table-driven).
2. Wrapper: warming/failed/ready Invoke results; Spec/Effect equal
   `agenttools.Retrieve`'s; concurrent Invoke+SetReady under `-race`.
3. Background build when absent: fake embedder, tiny tree => sidecar written,
   wrapper ready, ready notice emitted, chmod applied (0600).
4. Incremental refresh when present: build once, touch one file, rerun warm =>
   embed calls only for changed file (counting fake embedder), deleted file
   dropped.
5. Embedder-down: probe returns error => failed state, warning notice, no DB
   artifacts created (probe precedes store prep — a dead embedder must not
   leave an empty DB behind).
6. Self-heal torn: DB present, sidecar missing => artifacts removed, rebuilt.
7. Self-heal vs-mismatch: sidecar with different vector space => rebuilt.
8. Cancel mid-build: Close() during a slow fake embed returns promptly, no
   goroutine leak (race detector + done-channel assertion).
9. One-shot / `-no-auto-index` / `-no-rag` / `-rag-db`: no warmer started.
   Decision-level coverage via the mode matrix suffices: the OFF path is the
   byte-identical existing `enableRetrieve` flow, already covered by
   `retrieve_enable_test.go` (immediate open of an existing valid index).
10. Partial: fake embedder failing for one file => usable sidecar partial,
    serve + partial notice.

### 5.8 Acceptance criteria mapping

- First run un-indexed + embedder up => background build, retrieve becomes
  available without blocking (tests 3, wrapper tests).
- Subsequent runs incremental; unchanged => near-instant (test 4).
- Embedder down => warning, file tools work, no crash (test 5).
- `-no-auto-index` disables; `golem index`/`-rag-db`/`-no-rag` unchanged
  (tests 1, 9; `golem index` untouched by construction).
- `go test -race ./...` clean; gofmt/vet clean.
