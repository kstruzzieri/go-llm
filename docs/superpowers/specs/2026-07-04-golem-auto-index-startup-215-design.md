# Golem Auto-Index Startup (#215) Design

**Date:** 2026-07-04
**Issue:** [#215](https://github.com/kstruzzieri/go-llm/issues/215)
**Review note:** The review request says ticket #219, but GitHub #219 is the
closed tool-capability probe. The filenames, issue body, and decisions all map
to #215, so this spec covers #215.
**Status:** Review-updated design; ready for implementation plan.

## Problem

`golem index` can build a per-workspace RAG store, and plain `golem` can open a
valid existing auto index. It does not yet make a fresh or stale index "just
work" on startup. A repo with no index starts without `retrieve`, and a repo
with edits keeps serving the previous point-in-time snapshot until the user runs
`golem index` manually.

The existing pieces are good enough to reuse:

- `cmd/golem/index.go` has `executeIndex`, sidecar writing, vector-space probing,
  chmod hardening, and partial-but-usable sidecar behavior.
- `cmd/golem/retrieve_enable.go` has the current read-only `retrieve` open path.
- `rag.Indexer.IndexDirectoryWithStatus` already performs incremental updates for
  discovered files.
- `newChainEmbedder` exercises the same routing and vector-space path used for
  indexing and retrieval.

## Review Findings

1. **#215 says deleted files must be dropped, but current `rag.IndexDirectory`
   does not prune them.** `IndexDirectoryWithStatus` walks eligible files and
   indexes them, but never removes sources missing from the walk. The plan adds
   a small opt-in prune path for auto refresh. Do not claim existing incremental
   indexing already handles deletes.
2. **`executeIndex` returns non-zero for partial indexes even when it writes a
   usable sidecar.** Auto startup must not treat `errIndexFailed` alone as final
   failure. After `executeIndex`, inspect whether a valid sidecar and retriever
   can be opened; partial-but-usable becomes ready with a warning.
3. **The ready-gated tool must be mutex-guarded.** After #235, read-only tools
   can run in parallel. `retrieve` is read-only and approval-free, so any shared
   state inside a wrapper must be protected with a mutex/RWMutex and copied before
   delegating.
4. **Probe with the chain embedder, not provider health.** A one-input embed
   call through `newChainEmbedder` validates routing, fallback-chain resolution,
   model availability, and vector-space identity on the exact path that indexing
   uses. This is the right "Health() equivalent" for #215.
5. **Self-heal is safe only for the private auto index path.** The auto DB lives
   at `<data>/golem/indexes/<sha16>.db`, never at a user-supplied `-rag-db`
   path. It can remove torn or stale private artifacts. Manual `golem index` and
   explicit `-rag-db` must continue to fail closed unless the user asks for
   `golem index -full`.
6. **One-shot mode should skip only the background writer.** `-p` exits after one
   turn, so a background write risks process termination mid-index. Existing
   valid indexes may still be opened immediately, matching today's behavior.
7. **Auto-ready retrieval must retain the feedback DB handle.**
   `buildGatedRetriever` may open a `behavioralWeighterHandle`; callers own that
   DB and must keep it open while retrieval is live. The ready wrapper should own
   and close this handle when auto mode is used.

## Goals

1. Default REPL startup starts a background auto-index job when `-rag-db`,
   `-no-rag`, `-no-auto-index`, and `-p` are absent.
2. The REPL is usable immediately. The `retrieve` tool is registered as a
   ready-gated wrapper and returns model-visible steering while warming or failed.
3. When the background job succeeds, the wrapper opens the immutable read-only
   retriever after the writer closes and then serves a static snapshot for the
   rest of the session.
4. The background job reuses `executeIndex` and the existing sidecar/vector-space
   gates instead of duplicating indexing logic.
5. Auto refresh drops deleted or now-ignored files from the private index.
6. Embedder unavailable, invalid private DB, and vector-space mismatch degrade
   without crashing the REPL.
7. `-no-auto-index` restores today's immediate-open behavior exactly.

## Non-Goals

- No file watcher or live re-index during a session.
- No parallel/multi-process writer coordination. A single Golem process owns the
  startup job.
- No changes to explicit `-rag-db` safety.
- No new user config for concurrency, refresh interval, extensions, or excludes.
- No dynamic tool registration. Tools are fixed per turn, so the wrapper is the
  stable tool surface.

## CLI Behavior

New flag:

```text
-no-auto-index
```

Behavior matrix:

| Mode | Background writer | Retrieve registration |
| --- | --- | --- |
| default REPL, no `-rag-db`, no `-no-rag`, no `-no-auto-index` | yes | ready-gated wrapper |
| `-no-auto-index` | no | current immediate auto-open behavior |
| `-p` one-shot | no | current immediate auto-open behavior |
| `-rag-db <path>` | no | current explicit path behavior |
| `-no-rag` | no | no retrieve tool |

`-no-auto-index` is intentionally separate from `-no-rag`: it disables only the
startup writer, not retrieval from an already-valid auto index.

## Startup Flow

1. Resolve `root`, config, backend, provider bundle, agent chain, feedback DB,
   memory, and other existing startup state as today.
2. Resolve the auto index path with `indexDBPathForWorkspace(os.Getenv, root)`.
   If that fails in default REPL mode, warn and fall back to file tools.
3. If auto-index is disabled by flags or one-shot mode, call `enableRetrieve`
   exactly as today.
4. Otherwise:
   - create a `readyRetrieve` wrapper in `warming` state;
   - register it in the tool list;
   - start a background auto-index job;
   - print a startup line such as
     `retrieve: auto-index warming in background`.
5. The background job:
   - runs a 30s one-input embed probe through the chain embedder;
   - self-heals private auto artifacts when safe;
   - runs `executeIndex` into a buffer;
   - closes the write store;
   - opens retrieval through `buildGatedRetriever`;
   - keeps any returned feedback handle alive with the ready wrapper;
   - atomically switches the wrapper to `ready` or `failed`;
   - prints one completion notice to stderr.

## Ready-Gated Retrieve

`readyRetrieve` implements `agent.Tool` and delegates `Spec`/`Effect` to the real
retrieve tool shape:

- `warming`: `Invoke` returns a non-error `agent.ToolResult` explaining that the
  index is warming and the model should use `read_file`, `search`, `glob`, and
  `list` for now.
- `failed`: `Invoke` returns a non-error result explaining the failure and
  steering to file tools.
- `ready`: `Invoke` delegates to the opened `agenttools.Retrieve`.

State is guarded by a mutex/RWMutex. `Invoke` copies the state and delegate
under lock, releases the lock, then calls the delegate. The lock is never held
while doing retrieval.

When the delegate is installed, the wrapper also retains any
`behavioralWeighterHandle` returned by `buildGatedRetriever`. Main should defer a
wrapper `close()` method so that handle is closed during normal shutdown.

The warming/failed responses are non-error on purpose: they are not malformed
tool calls, and the model can recover by using file tools.

## Embed Probe

The startup probe is:

```go
ctx, cancel := context.WithTimeout(parent, 30*time.Second)
defer cancel()
_, err := embedder.Embed(ctx, embChain[0], []string{"golem startup index probe"})
```

The response must contain one vector. A missing vector or embed error marks
auto-index as failed for this run. Nothing is persisted and the REPL keeps
running.

This is better than `Provider.Health()` because it exercises:

- provider routing;
- strict embedding fallback chain;
- model load under llama-swap style backends;
- the configured embedding model;
- vector-space metadata returned by the exact indexing path.

## Self-Heal Policy

Self-heal applies only to `autoDBPath`:

- DB exists but sidecar missing, corrupt, wrong schema, or wrong workspace:
  remove DB, WAL, SHM, and sidecar, then full rebuild.
- DB exists with a valid sidecar but vector-space gate rejects it:
  remove artifacts, then full rebuild.
- DB absent:
  normal first build.
- DB valid and vector-space-compatible:
  incremental refresh.

Manual paths keep existing behavior:

- `golem index` without `-full` still refuses invalid/mismatched existing DBs.
- `-rag-db` never self-heals or deletes anything.

## Deleted Source Prune

Auto refresh must drop sources that no longer appear in the eligible workspace
walk. The minimal change is an opt-in `rag.WithPruneDeleted()` directory option:

1. `IndexDirectoryWithStatus` already collects the eligible file list.
2. When prune is enabled, after the walk and file indexing, list current stored
   sources from `*rag.SQLiteStore`.
3. For stored sources under the workspace root that are not in the eligible file
   set, call `DeleteBySource`.
4. Sources outside the workspace root are ignored defensively.

Manual `golem index` does not need to change unless the implementation chooses to
share the prune option there explicitly. For #215, the startup auto refresh is
the required prune path.

## Partial Indexes

`executeIndex` already writes a sidecar for partial-but-usable runs and returns
`errIndexFailed`. The background job must interpret the result through the store,
not through the exit sentinel alone:

- sidecar valid and retriever opens: `ready`, plus a warning notice using the
  existing partial sidecar fields;
- no valid sidecar or retriever cannot open: `failed`.

## Notices

Startup and completion lines are concise:

- startup: `retrieve: auto-index warming in background`
- ready: `retrieve: auto-index ready, <n> sources, <vector-space-id>, updated <timestamp>`
- partial ready: `warning: retrieve auto-index partial, <n> sources, <m> errors; retrieval enabled`
- failed: `warning: retrieve auto-index failed: <reason>; using file/search tools`
- self-heal: `warning: retrieve auto-index rebuilding private store: <reason>`

The completion notice may be printed by the background goroutine to stderr. Keep
it one line so it is tolerable if it appears near a prompt.

## Testing

Required focused tests:

- flag parsing for `-no-auto-index`;
- default REPL chooses ready-gated auto mode, while `-p`, `-no-auto-index`,
  `-rag-db`, and `-no-rag` do not start the writer;
- `readyRetrieve` warming/failed/ready states and race-safe transition;
- embed probe uses `newChainEmbedder` route path and honors timeout;
- auto self-heals invalid private sidecar and vector-space mismatch;
- partial usable index becomes ready with a warning;
- deleted/ignored sources are pruned in auto refresh;
- `go test -race ./cmd/golem ./agent ./rag` clean, then full `go test ./...`.
