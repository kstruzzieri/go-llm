# Changelog

All notable changes to `go-llm` are documented here. Downstream consumers
(Firn IDE, Flux ML, Quantum Trader) should consult this before any
`go get -u github.com/kstruzzieri/go-llm`.

## [Unreleased]

### Added — `rag` progressive source summaries, slice 1 of #189

A store and renderer for per-source L0/L1 summaries, so retrieval context can
mix short orientation text with full chunk evidence under hard token and byte
budgets instead of concatenating whole chunks until a limit is hit.

**Nothing here runs unless you opt in, and it renders no summary text until
slice 2.** No shipping caller sets `tools.RetrieveConfig.Progressive`, and
nothing in the repo writes a `source_summaries` row — generation is slice 2.
A caller who opts in today gets the deterministic metadata overview plus
evidence for every source, with `missing` in its validity reasons.
`Retriever.BuildContext` is unchanged and remains the default path.

New exported surface in `rag`, all additive:

- `Retriever.RenderProgressive`, with `ProgressiveRenderRequest`,
  `ProgressiveTrace`, `ProgressiveSourceTrace`, `RenderedEvidence`, `PinRef`,
  and the `Depth*` and `Decision*` constants.
- `SourceSummary`, with `SQLiteStore.UpsertSourceSummary`,
  `SourceSummaryBatch`, and `DeleteSourceSummary`. Writers MUST take
  `ContentHash` and `VectorSpaceID` from `SQLiteStore.SourceProvenanceBatch`
  for the same source; any other value stores a row that permanently derives
  stale and never renders, with no error reported.
- `SourceProvenance`, with `SQLiteStore.SourceProvenanceBatch` and
  `SQLiteStore.ChunkContentDigestBatch`.
- `ValidityReason` and its nine `ValidityReason*` constants.
- Schema migration **v8** adds the `source_summaries` table. Writable
  databases migrate on open; a v7 database opened read-only keeps working and
  degrades to summary-missing rather than failing retrieval.

Rendered source paths and managed document titles are untrusted text that
reaches the model. Newline-based forgery of a whole block is blocked;
same-line forgery of a block's own label is not — see the security note on
`RenderProgressive`.

## [0.1.0] - 2026-07-22

First tagged release of `go-llm`. Prior to this tag, downstream consumers
(Firn IDE, Flux ML, Quantum Trader) tracked `develop` via pseudo-versions;
`v0.1.0` is the first stable ref to pin. Semantic versioning applies from
here — `0.x` means the public API may still change between minor versions.

### Initial surface

- **Local LLM backends** — llama.cpp via its OpenAI-compatible server
  (`provider/openaicompat`, the primary/recommended backend) and native
  Ollama (`ollama/`), selected per-provider by `models.json` `api_format`.
- **Use-case-aware routing** (`provider/`) — chat/FIM/embedding/reasoning/
  analysis/code-review/agent profiles, circuit breakers, warmth, sticky
  routing, scoring, and fallback chains.
- **RAG** (`rag/`) — chunking, SQLite-backed vector store, indexing,
  scored/hybrid retrieval, and a managed document registry with stable IDs
  and freshness tracking.
- **Golem CLI** (`cmd/golem`) — local-first terminal coding agent: read/
  write/exec tools, RAG retrieval, project-context (`AGENTS.md`) loading,
  persistent sessions, conversation compression, explicit and agent-authored
  memory, MCP client attachment, and AgentFlow plan/task execution.
- **MCP** — server (`mcp/`, `cmd/go-llm-mcp`) exposing tools/prompts/
  resources over stdio and HTTP/2, and a client (`mcpclient/`) adapting
  external MCP servers' tools for the agent.
- **Memory** (`memory/`) and **conversation** (`conversation/`) — SQLite
  persistence with scope-filtered FTS5/bm25 search.
- **Supporting packages** — `completion/` (FIM), `analysis/`, `feedback/`,
  `fingerprint/`, `prefetch/`, `compat/` (OpenAI-compatible shim),
  `projectcontext/`, and the `cmd/llm-bench` evaluation harness.
- **Distribution** — `go install github.com/kstruzzieri/go-llm/cmd/golem@v0.1.0`,
  or prebuilt `golem` / `go-llm-mcp` binaries (darwin/linux/windows,
  amd64/arm64) attached to the GitHub release. `golem -version` reports the
  build identity.

The consumer-facing notes below describe the state of the `models.json`
defaults and the router API as shipped in this release.

### Breaking for consumers of `models.json` defaults

The root `models.json` lineup has been retargeted for 2026 model releases.
**Consumers that read `models.json` via `config.Load` and expect specific
model names will observe different models at runtime.**

| Role          | Was                    | Now                       |
|---------------|------------------------|---------------------------|
| `general`     | `qwen3.5:27b`          | `gemma4:31b`              |
| `analysis`    | `qwen3.5:27b`          | `gemma4:31b`              |
| `fast`        | `qwen3.5:35b-a3b`      | `qwen3.6:35b-a3b`         |
| `agent` (new) | —                      | `gemma4:31b`              |
| `coding`      | `qwen3-coder-next:latest` | unchanged              |
| `lightweight` | `qwen3:8b`             | unchanged                 |
| `embedding`   | `qwen3-embedding:8b`   | unchanged                 |

**Before upgrading, consumers MUST:**

1. `ollama pull gemma4:31b` and `ollama pull qwen3.6:35b-a3b` on every
   deployment host. Absent pulls will produce 404s from Ollama at first
   use (circuit breaker will trip and fall back per the `fallbacks`
   chain, so behavior degrades rather than crashes — but quality will
   drop).
2. If you pinned to `qwen3.5:27b` or `qwen3.5:35b-a3b` in app-level
   code, that pinning is unaffected by this change (the library only
   reads from `models.json`). You can keep pinning explicitly.
3. Review `docs/llm/recommendation.md` for the rationale and the
   GLM-5.1 parallel experiment plan.

### Added

- **`agent` role** in `models.json` defaults, pointing at `gemma4:31b`.
- **`agent` / `tool-use` weight profiles** in
  `provider/router_score.go:defaultWeightProfiles` so the new role
  gets meaningful scoring instead of falling back to `chat`.
- **`ollama.ChatRequest.KeepAlive`** field exposing Ollama's
  `keep_alive` directive. Useful for benchmark runs that want a model
  to stay warm longer than the 5-minute default.
- **`cmd/llm-bench`** — scaffold for model A/B comparison on captured
  traces. See `docs/llm/benchmark-plan.md`.
- **`qwen3.6`, `qwen3-coder-next`, `gemma4`** families in
  `provider/catalog.json`.
- **`docs/llm/`** — 2026 local-LLM analysis, three candidate setups,
  recommendation, and benchmark plan.

### Changed

- Catalog: `qwen3-coder-next` gains a `latest` variant alias kept in
  sync with `80b` (guarded by test).

### Router: provider-instance pinning (#81)

- **`provider.RoutingRequest.Provider`** — new optional `string` field that
  hard-scopes routing to a specific provider *instance* (the config-time
  name, e.g. `ollama-local-a`, `vllm-prod-1`). Acts as a pre-score filter:
  empty `Model` + `Provider` scopes `Recommend`; unqualified `Model` +
  `Provider` pins `ModelKey{Provider, Model}` via `Lookup`; qualified
  `Model` (`provider/model`) + non-empty `Provider` must agree on identity
  or `Router.Route` returns the new `ErrProviderMismatch` sentinel before
  candidate resolution. `PreferredChain` is authoritative when set —
  chain selectors carry their own provider identity and the per-request
  `Provider` hint is ignored under chain routing.
- **`provider.ChatRequest.Provider`, `GenerateRequest.Provider`,
  `EmbedRequest.Provider`** — optional `string` fields (`json:"provider,omitempty"`)
  forwarded by `Router.Chat / ChatStream / Generate / GenerateStream / Embed`
  into `RoutingRequest.Provider`. Router selection metadata only; not
  forwarded to the concrete provider's execution call (the provider already
  knows its own identity).
- **`provider.RecommendOpts.RestrictToProvider`** — single-string hard
  filter on the recommendation path. Distinct from the still-unused soft
  `PreferredProviders`. An unknown provider name surfaces as a provider
  resolution error rather than degrading to a silent empty result.
- **Sticky-key derivation** — `RoutingRequest.Provider` participates in
  `StickyKey` so two scoped requests with identical affinity/model/use-case
  keep independent sticky entries. Empty `Provider` produces byte-identical
  keys to pre-change behavior, preserving existing affinity warmth.
- **JSON wire / Go literals** — unset request-level `provider` fields are
  omitted on the wire (`omitempty`). Keyed Go struct literals
  (`provider.ChatRequest{Model: ..., Messages: ...}`) are
  additive-compatible. Unkeyed composite literals for the changed exported
  structs (`provider.RoutingRequest{...}`, `provider.ChatRequest{...}`,
  `provider.GenerateRequest{...}`, `provider.EmbedRequest{...}`) will fail
  to compile because positional arguments now shift by one slot; convert
  to keyed literals (recommended) or insert an explicit empty `Provider`
  positional value. All call sites within this repo (`analysis/*`,
  `provider/route_plan.go`, `provider/router.go`, etc.) use keyed
  literals and are unaffected. External consumers — Firn IDE, Flux ML,
  Quantum Trader — live in separate repos and should audit their own
  call sites; they will get a compile error rather than silent
  misbehavior on `go get -u`.
