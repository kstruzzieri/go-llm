# AGENTS.md

## Project Overview

`go-llm` is a shared Go module providing local LLM integration (chat, completions, embeddings) and a lightweight RAG layer with SQLite-backed vector storage.

**Local backends:** models are reached per-provider via `models.json` `api_format`. **llama.cpp (via its OpenAI-compatible `llama-server`, `api_format: openai-compat`) is the primary/recommended backend** — best local performance. Ollama (native REST, `api_format: ollama`, the default when omitted) is fully supported. The `ollama/` package is the native Ollama client; `provider/openaicompat` + `provider.Router` reach any OpenAI `/v1` server (llama.cpp, vLLM, LM Studio). Local benchmarking and the `cmd/llm-bench` harness run against llama.cpp.

**Consumers:** Firn IDE (custom Wails IDE), Flux ML (Wails ML dev environment), Quantum Trader (Go+Python trading platform)

## Architecture

```
go-llm/
├── ollama/          # Ollama REST API client (chat, generate, embeddings, models)
├── config/          # Model configuration loader (models.json, resolve, fallback) + Document (origin/revision-aware load, secret-literal-preserving atomic writer, role lifecycle + selector overrides + credential scrub) + `CapabilityPolicy` (one provider/model selector's capability policy: explicit list, type-derived capabilities, type floor; matches bootstrap's override and floor builders)
├── configview/      # Pure projection of a config for panels/CLI/MCP (`Build`: v1 wire contract, tri-state candidate eligibility, no I/O; consumed by golem models -json, MCP configview resource, Firn config panel) + offline capability and listing explanations for the Firn config panel (`ExplainCapabilities`, `ExplainListing`: at most one evidence-store read each, no other I/O)
├── configio/        # Explicit I/O tier for the config stack: RefreshInventory (provider listing → configview.Inventory value via the read-only ProjectListedModels projection; publishes listings and provider facts to the evidence store only through the explicit `WithPublish` option) + ProbeToolCall (consent-gated per-model probe, ProbeOutcome{State,Persisted}) — never implicit, values in/out, bounded error codes (CodeOf), cancellation unclassified
├── profiles/        # Profile catalog: curated go:embed configs (credential-free by pinned rule) + user store under a 0700 profiles/ boundary; stable IDs, bounded error codes, SaveOutcome writes (nil error whenever persisted) — the Firn config-panel write path
├── provider/        # Use-case-aware Router (chat/fim/embedding/reasoning/analysis/code-review/agent profiles), circuit breakers, warmth, slot-capacity discovery + slot-aware admission, sticky routing, scoring, fallback chains; `WithEvidenceLog` attaches the shared capability store, after which on-demand tool_call resolution reads and writes only its log (24 h cooldown, two-request automatic allowance, a result whose save failed kept in memory and marked unsaved); `CanonicalEndpoint` is the one endpoint spelling evidence is keyed by; `ExplainEvidence` is the one precedence core that ranks capability evidence (explicit, direct, provider, catalog, type), shared on the evidence-log path by routing, the listing projection and `ExplainToolCall`, and by `configview.ExplainCapabilities`
│   └── openaicompat/ # OpenAI /v1 client — reaches llama.cpp (primary), vLLM, LM Studio via api_format: openai-compat
├── rag/             # RAG: chunking, SQLite vector store, indexing, retrieval, managed document registry (stable IDs, lifecycle, freshness)
├── rag/ast/         # Scoped structural symbol graph: Extractor + SymbolStore interfaces (skeleton)
├── completion/      # IDE inline completion (Fill-in-the-Middle)
├── analysis/        # Domain-specific analysis helpers (code review, ML metrics, trading)
├── agent/           # Agent runtime: Orchestrator loop, effect-aware tools, serial/parallel dispatch, opt-in interceptor pipeline (#436: ingress hooks on frozen values, allow/tag/block/abort, per-run RiskReport, provenance)
├── agent/interceptor/ # Opt-in defaults: origin-sensitive injection detectors (zero-width, encoded instructions, typoglycemia), all-origin secret/payment-card blocking, argument-invariant guards, exec egress labels, and native scoped-child refusal reporting (#555)
├── mcp/             # MCP server: tools, prompts, resources over stdio/HTTP/2 — wired through provider.Router
├── mcpclient/       # MCP client: adapts external MCP servers' tools into agent.Tool (stdio/streamable-HTTP); consumed by cmd/golem
├── consult/         # /consult seam: bounded host runner + Claude and Codex subscription adapters (#382, #546); unsigned consult-result/v1 receipts
├── conversation/    # Persistent conversation storage with SQLite
├── memory/          # Explicit user-controlled local memories + agent-memory records (SQLite, scope-filtered FTS5/bm25 search); shared hardened-open primitives (open.go); separate from conversation + RAG; backs Golem /remember + memory_search AND MCP agent_memory_* tools
├── projectcontext/  # AGENTS.md-style project-context loader (discovery, safe read, ordering; consumed by cmd/golem)
├── recipe/          # Versioned JSON prompt bundles (v1): Parse for embedded bytes, Load for explicit paths (regular-file + identity checks, 64 KiB bound, nonblocking Unix open); closed schema, strict keys, advisory role/use-case hints. Template expansion + discovery are #353.
├── signing/         # Detached signatures over canonical JSON (ZT-301): Signer/Verifier seam, canonical form v1, Ed25519 + HMAC-SHA256 backends, Keyring rotation, hardened key files. Consumed by #445/#446/#447/#450; knows no record schema.
├── feedback/        # Implicit user behavioral signal collection
├── fingerprint/     # Model profiling (latency benchmarks, capability detection) + the shared capability store (#734): append-only evidence log (per-endpoint/adapter/digest answers with expiry + pruning), per-credential listings (account HMAC never exported), registry reads; `OpenSharedStore` is the one hardened opener (file, memory-only fallback, disabled; golem and other hosts use it); `CapabilityProber` + probe recipes (`probers/` streamed tools check answers chat, stream and tool_call; `Detail*` inconclusive codes)
├── prefetch/        # Predictive cache-warming engine for RAG retrieval
├── compat/          # OpenAI-compatible endpoint shim (chat, completions, model aliases, concurrency limiter)
├── cmd/
│   ├── golem/       # Golem CLI: agent REPL. Interactive input goes through one lineSource seam — a golang.org/x/term line editor on a TTY (arrow editing, per-workspace goal history, bracketed-paste composition, `/edit`, Ctrl-C arm/quit) or the bufio.Scanner for pipes, -no-editor, dumb terminals, and Windows
│   ├── go-llm-mcp/  # Standalone MCP server binary (stdio + HTTP/2)
│   ├── fim-smoke/   # FIM smoke-test harness
│   └── llm-bench/   # Model evaluation harness (AnswerQuality, tool-use, tool-restraint, latency, tokens; paired Δ + bootstrap CIs; llama.cpp via openai-compat)
├── docs/            # Reference documentation (BYO models, design notes)
└── testdata/        # Test fixtures
```

## Module Path

```
module github.com/kstruzzieri/go-llm
```

## Dependencies

Keep minimal. Allowed external dependencies:
- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- `golang.org/x/sync` — concurrency primitives (errgroup for bounded worker pools)
- `golang.org/x/net` — h2c HTTP/2 cleartext transport (only imported by `mcp/`)
- `golang.org/x/term` — VT100 line editor for the Golem REPL prompt (only imported by `cmd/golem/`). Pinned to v0.42.0, the version already selected transitively, so promoting it moves no other module.
- `golang.org/x/sys` — already required transitively; imported directly by `agent/tools/` (build-tagged: darwin/linux background exit watching, CoW cloning via clonefile/FICLONE, the no-replace promotion install for #443, the exec screen's directory read for #789, and Windows workspace identity capture), `cmd/golem/`'s Linux and Darwin PTY tests, and `profiles/`'s Windows directory-fsync (build-tagged, mirrors `config/`'s pair)
- `github.com/modelcontextprotocol/go-sdk` — official MCP Go SDK (imported by `mcp/` server side, `mcpclient/` client side, and `cmd/llm-bench/`)
- `github.com/parquet-go/parquet-go` — Parquet file writer (only imported by `rag/parquet/`)
- `github.com/santhosh-tekuri/jsonschema/v6` — JSON Schema validator (only imported by `cmd/llm-bench/`)

Everything else uses stdlib (`net/http`, `encoding/json`, `math`, `context`, etc.)

## Design Principles

1. **Public methods performing cancellable work take `context.Context`** — cancellation is critical (IDE completions get cancelled constantly); established synchronous APIs, including `config.Document` and its local-file operations, remain context-free
2. **Streaming is first-class** — both chat and completions support streaming via callback functions
3. **No global state** — multiple Client instances can coexist with different configs
4. **Interfaces for extensibility** — `VectorStore` interface allows swapping SQLite for other backends
5. **Errors are informative** — wrap with context, include HTTP status codes from Ollama
6. **Tests use mock HTTP servers** — no Ollama dependency for unit tests; integration tests behind build tag

## Ollama API Reference

This is the native `ollama/` client's wire API (the `ollama` provider format). The **llama.cpp / `openai-compat`** path instead speaks the OpenAI `/v1` API (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, `/v1/models`) — handled by `provider/openaicompat`; pass the server root as base URL (go-llm appends `/v1`).

Base URL: `http://localhost:11434`

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/chat` | POST | Chat completions (streaming/non-streaming) |
| `/api/generate` | POST | Raw text generation |
| `/api/embed` | POST | Generate embeddings |
| `/api/tags` | GET | List available models |
| `/api/show` | POST | Get model info |
| `/api/pull` | POST | Pull a model |

### Chat Request
```json
{
  "model": "gemma4:31b",
  "messages": [{"role": "user", "content": "hello"}],
  "stream": true,
  "options": {
    "temperature": 0.7,
    "num_predict": 2048,
    "num_ctx": 32768
  }
}
```

Model names in examples reflect the current `models.json` defaults but are
not required by `go-llm` — any model the configured provider can load
works. See `docs/llm/` for the reference lineup and BYO guidance.

### Embed Request
```json
{
  "model": "qwen3-embedding:8b",
  "input": "text to embed"
}
```

Response: `{"embeddings": [[0.1, 0.2, ...]]}`

### Streaming
When `stream: true`, response is newline-delimited JSON. Each chunk has `done: false` until the final one which has `done: true` and includes timing stats.

## Data Sensitivity

The repository holds no production, personal, or client data. At runtime, go-llm and Golem handle the user's own data on the user's machine:

- **User data directories:** the Golem data dir (`$XDG_DATA_HOME/golem/`, else `~/.local/share/golem/`: sessions, memories, goal history, checkpoints, code indexes, `-trace` traces, telemetry, capability evidence, the signing key), `~/.local/share/go-llm/` (MCP server RAG store), and `<os.UserConfigDir>/go-llm/`. They may hold private source, anything the user typed, and anything a tool returned.
- **Credentials:** `api_key` in a user's `models.json`, signing keys, Claude and Codex logins used by `/consult`, and secrets in the environment or workspace. Every committed secret-shaped string is synthetic (e.g. `internal/secretscan/secrettest/cases.json`, `agent/testdata/hardening/secrets/`); never swap in a live value.
- **Data leaving the machine:** an endpoint is local only if its `base_url` is; `api_format` says nothing about location. Remote providers, `/consult` consultants (`docs/consult.md`), `-mcp-http` servers, and stdio MCP servers given `-mcp-env` receive prompts, tool arguments, or secrets; exec commands reach the network unless denied.

AI tools must not read, print, or transmit real credentials, a user's `models.json`, or the directories above unless the user asks in the current session, and must never commit them or paste them into an issue or PR; use the synthetic fixtures and mock servers. In review, all code outside `docs/` and test data is security-relevant; highest risk: `agent/tools/` (exec sandbox), Golem trust and approval code, `projectcontext/`, `mcpclient/`, `agent/interceptor/`, `internal/secretscan/`, `config/`, `signing/`, `consult/`, `provider/`.

**AI policy:** no organization AI policy applies to this project.

## Changelog

Outside the fold-and-stamp release PR, never edit `CHANGELOG.md`; concurrent PRs conflict at the shared insertion point under `## [Unreleased]`. Add `changelog.d/<issue>-<slug>.md` holding the full `### <Category> — <title> (#<issue>)` section instead (see `changelog.d/README.md`). CI rejects direct edits; `scripts/changelog-fold` folds fragments at release.

## Testing

```bash
# Unit tests (no Ollama needed)
go test ./...

# Integration tests (requires running Ollama)
go test -tags integration ./...
```

## Code Style

- Standard Go conventions (gofmt, golint)
- Table-driven tests
- Descriptive error messages with `fmt.Errorf("ollama: %w", err)` pattern
- Package-level doc comments on every exported type and function
