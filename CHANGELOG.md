# Changelog

All notable changes to `go-llm` are documented here. Downstream consumers
(Firn IDE, Flux ML, Quantum Trader) should consult this before any
`go get -u github.com/kstruzzieri/go-llm`.

## [Unreleased]

### Changed — golem: interrupted approvals record `canceled`, not `error`

A Ctrl-C during an interactive approval prompt now always records the run's
trace and telemetry status as `canceled` and renders `canceled`. Previously
the recorded status raced the interrupt watcher's context cancellation and
could land as `error` with an `error: interrupted` line. `runOnce` now
synchronizes the run context whenever the run returns `context.Canceled`, so
the classification no longer depends on scheduler order. Telemetry consumers
that keyed on `status == "error"` for interrupted approvals will see those
runs as `canceled` from this version on.

### Added — mixed-domain context assembly, slice 3b of #331

The agent runtime can now assemble model context from RAG results,
conversation spans and agent-memory records at MIXED fidelity under one global
token budget, instead of retaining or dropping each tool result whole. Tools
declare what they COULD contribute (a `ContextSet` of per-subject
alternatives); `ContextManager` picks at most one alternative per subject and
reports every choice in a content-free trace.

Opt-in via `ContextManager.Mixed`. Off, model-visible messages stay
byte-identical and the new trace is the zero value.

Mixed assembly preserves `RecencyCompactor`'s recency semantics: it replaces
the compactor rather than layering on it, and under pressure it retains the
same messages the compactor would have. Two orderings are involved. Retention
PRIORITY by kind is the reverse of the compactor's drop order (current-run
plain exchanges, then completed tool chains, then prior history). WITHIN each
of those, the NEWEST members are retained, exactly as dropping oldest-first
does. A consumer switching a pressured session to `Mixed` therefore does not
have to re-discover which turns the model still sees.

**Two behavior changes reach consumers who do not opt in:**

- `agent/tools.Retrieve.Progressive` is now a HARD CONTRACT on `R`. Setting it
  with a retriever that does not implement `RenderProgressiveWithGroups` fails
  every call instead of silently serving the legacy `BuildContext` path with
  its over-crediting attribution and no `ContextSet`. `*rag.Retriever`
  satisfies it; only a consumer-supplied retriever is affected.
- `agent/tools.Retrieve` now clamps the model-supplied `k` to 20 on the
  LEGACY path too, before the backend call. A consumer whose model asks for
  50 results silently gets 20, and the legacy attribution set (which credits
  every retrieved result) shrinks with it. Unbounded model-supplied `k` is a
  resource vector in flat mode as well; the new `Retrieve.MaxK` field is the
  escape hatch. In progressive mode `MaxK` is additionally capped at 20 and a
  larger value is rejected per call, because the capability projection emits
  3(k+1) alternatives per fresh source and 21 would exceed the carrier bound.
- `golem -progressive` now also sets `ContextManager.Mixed`, not only the
  summary generation described below. That rewrites the model-visible bytes of
  every tool anchor and shifts which `Pressure.Cause` bucket a pressured run
  reports. The library-level `golem.Options.Progressive` sets ONLY the mixed
  flag; a library host wires the tool's own `Progressive` field itself.

New public API:

- `agent`: `ContextManager.AssembleWithTrace`, `ContextManager.Mixed`,
  `ContextSet` / `ContextGroup` / `ContextAlternative`,
  `ContextAssemblyTrace` / `ContextSubjectTrace`,
  `ContextAssemblyObserver` / `ContextAssemblyEvent`, `ErrMixedCompactor`,
  and the `Decision*` / `Omit*` trace vocabulary.
- `agent/tools`: `Retrieve.MaxK`.
- `golem`: `Options.Progressive`.
- `rag`: `Retriever.RenderProgressiveWithGroups`, `ProgressiveGroup`,
  `ProgressiveAlternative`.
- `contextdepth`: the descriptor vocabulary these carry (`SubjectRef`,
  `GroupDesc`, `AlternativeDesc`, `RepresentationDesc`).

`Retriever.RenderProgressiveWithGroups` returns the same output, trace and
error as `RenderProgressive` for the same request, on every path. A blank
`Chunk.Source` on any result yields NO groups for that call rather than
failing it — such a result has no subject id, and a partial projection would
lose its blocks under mixed assembly, which replaces the anchor's flat content
with the selected alternatives.

Under mixed assembly a fresh source is offered the deterministic metadata
overview as its cheapest alternative, matching the flat renderer's own
budget fallback, so a source that does not fit at summary depth still
contributes a short block instead of vanishing. Its note line reads
`summary omitted: budget` — never `no summary`, which would be false.

**`Pressure` gains a field, and one existing field changes meaning.** Read
this before upgrading a telemetry consumer.

- NEW `Pressure.AnchorOmissions` counts subjects mixed assembly dropped from a
  RETAINED structured anchor. The usual cause is a full anchor byte cap
  (`Message.OutputCap`, 64 KB for `retrieve`), which a large retrieval hits
  routinely — a 20-source projection is far more alternative text than the cap
  can hold. Before this counter such a drop was invisible: `Evicted` stayed 0,
  `UsedPct` stayed low, `Level` stayed `ok`, and `ToolResult.Truncated`
  describes the DISCARDED flat rendering, not the mixed one. Always 0 with
  `Mixed` off.
- `Pressure.Evicted` is unchanged and still counts WHOLE groups (spans and
  chains). Within-anchor omissions are deliberately NOT folded into it: five
  sources shed from one retained anchor is not five evicted groups.
- CHANGED `Pressure.Compactions` and `Pressure.Mitigation`: under mixed
  assembly a turn that only shed subjects from a retained anchor now reports
  `Compactions: 1` / `MitigationEvict` (previously `0` / `MitigationNone`).
  The orchestrator emits its `compaction` `EventRecord` for such a turn, and a
  consumer counting compactions will see turns it did not see before. Legacy
  (`Mixed` off) values are byte-for-byte unchanged.
- `Pressure.Level` deliberately does NOT react. The bands measure TOKEN-budget
  usage; a byte-cap omission is orthogonal (a turn can shed a quarter of its
  retrieval at 8% of budget), and promoting `Level` would make the bands mean
  two different things and break existing level histograms.
- `internal/agenttrace` carries the count as `anchor_omissions` on the
  `model_step` and `runtime_stage` spans. Additive within `SchemaVersion` 2:
  the key is omitted when zero, so legacy and lossless turns emit the same
  bytes as before.

`golem -telemetry` now emits a `context_assembly` span per mixed assembly,
pairing with `anchor_omissions`: token totals, subject counts,
`verbatim_shortfalls`, rendered bytes, and `by_decision` / `by_omission_reason`
breakdowns keyed on agent's fixed vocabulary. It is a NEW span kind, so it is
additive within `SchemaVersion` 2 and only mixed turns emit it. The breakdowns
are counts only — persisted telemetry does not retain the per-subject rows that
carry source paths, memory record IDs, and tool call IDs. `-trace` likewise
does not serialize structured `ContextAssemblyTrace` rows or row fields, though
its content-full model-visible messages can independently contain those
identifiers. The rows themselves are available only to a live
`ContextAssemblyObserver`.

`golem` no longer prints `(truncated)` on a tool-result line when mixed
assembly replaced that result's content. `ToolResult.Truncated` describes the
DISCARDED flat rendering, and the flag cannot be recomputed at that point:
assembly runs against a global budget before the next step's model call. Plain
tools under mixed, and every tool with `Mixed` off, are unaffected.

MEMORY: with `Mixed` on, each tool result's projection is cloned onto its
anchor message and retained for the rest of the run. For `Retrieve` that is
quadratic in `k` — 1.35 MB per call at `MaxK` 20 over 2 KB chunks, so a
20-step run holds roughly 27 MB. That worst case needs every one of a call's
`k` results to land on ONE source; results spread over 4 or more sources cost
under 0.4 MB per call. With `Mixed` off nothing is cloned.

### Added — model-backed progressive source summaries, slice 2 of #189

Golem can now generate and serve the existing L0 abstract/L1 overview ladder
with the explicit `-progressive` flag. Generation runs only in an unpublished
index generation, routes through the configured `summarize` role (including
its existing `analysis`/`chat` fallback), and records the model that actually
served the request. Default indexing and retrieval still make zero summary
model calls.

`SQLiteStore.GenerateSourceSummaries` refreshes missing or stale summaries
from stored indexed chunks. It copies `ContentHash` and `VectorSpaceID`
byte-for-byte from `SourceProvenanceBatch` and compare-and-swaps the write
against both values, so a concurrent reindex cannot publish stale model text.
Rows below `SourceSummaryFormatVersion` regenerate; rows above it remain
unreadable and are not overwritten by an older binary. A per-source model or
validation failure leaves that source on the deterministic metadata fallback,
continues the remaining summaries, and warns without blocking index publication.
That warning leads with a `N of M sources failed` tally, because callers print
only its first line.

Degradation rules, so `-progressive` fails visibly rather than quietly:

- A source larger than the prompt budget is summarized from its leading chunks
  instead of being refused, and the model is told outside the fence how much it
  is seeing. Refusing would leave large sources permanently unsummarizable and
  re-erroring on every index run.
- Model output wrapped in a single Markdown code fence is accepted, since local
  models emit that despite instructions. The rest of the contract stays strict:
  unknown fields, trailing objects, blank fields, and a multi-line abstract are
  all still rejected.
- `-progressive` now warns when no `summarize`/`analysis`/`chat` default
  resolves. Previously the flag was accepted and did nothing at all — including
  on the zero-config path where no `models.json` is discovered.

`SourceProvenanceBatch` and `SourceSummaryBatch` now read in bounded batches.
They were introduced for retrieval-result-sized inputs;
`GenerateSourceSummaries` passes every source in the index, which would exceed
SQLite's 32766-variable ceiling on a large enough workspace and fail the whole
read rather than degrade.

`internal/promptfence.FlattenLine` and `internal/modeltext.StripCodeFence` hold
the single copy of two rules that now have multiple callers. No public API
change: `analysis` and `agent/tools` forward to them.

### Added — `rag` progressive source summaries, slice 1 of #189

A store and renderer for per-source L0/L1 summaries, so retrieval context can
mix short orientation text with full chunk evidence under hard token and byte
budgets instead of concatenating whole chunks until a limit is hit.

**Nothing here runs unless you opt in.** `Retriever.BuildContext` is unchanged
and remains the default path; Golem's slice-2 opt-in is described above.

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
