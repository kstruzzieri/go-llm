# AGENTS.md

## Project Overview

`go-llm` is a shared Go module providing local LLM integration (chat, completions, embeddings) and a lightweight RAG layer with SQLite-backed vector storage.

**Local backends:** each `models.json` provider picks its wire API with `api_format`. **llama.cpp (`llama-server`, `api_format: openai-compat`) is the primary backend**; Ollama (`api_format: ollama`, the default when omitted) is fully supported. `ollama/` is the native Ollama client; `provider/openaicompat` + `provider.Router` reach any OpenAI `/v1` server (llama.cpp, vLLM, LM Studio). `cmd/llm-bench` runs against llama.cpp.

**Consumers:** Firn IDE (custom Wails IDE), Flux ML (Wails ML dev environment), Quantum Trader (Go+Python trading platform)

## Architecture

```
go-llm/
├── ollama/          # Ollama REST API client (chat, generate, embeddings, models)
├── config/          # Model configuration (models.json load, resolve, fallback) + Document (revision-aware load, secret-preserving atomic writer, role lifecycle, credential scrub) + `CapabilityPolicy`
├── configview/      # Pure config projection for panels/CLI/MCP (`Build`: v1 wire contract, no I/O) + offline capability and listing explanations (`ExplainCapabilities`, `ExplainListing`)
├── configio/        # Explicit I/O tier: `RefreshInventory` (provider listing → `configview.Inventory`; publishes to the evidence store only via `WithPublish`) + consent-gated `ProbeToolCall`, `PlanCheck`, `RunCheck`; bounded error codes (`CodeOf`)
├── profiles/        # Profile catalog: curated credential-free go:embed configs + a 0700 user store; the Firn config-panel write path
├── provider/        # Use-case-aware Router: circuit breakers, warmth, slot-aware admission, sticky routing, scoring, fallback chains; `WithEvidenceLog` capability resolution; `CanonicalEndpoint` keys evidence; `ExplainEvidence` is the one capability-evidence precedence core
│   └── openaicompat/ # OpenAI /v1 client — reaches llama.cpp (primary), vLLM, LM Studio via api_format: openai-compat
├── rag/             # RAG: chunking, SQLite vector store, indexing, retrieval, managed document registry (stable IDs, lifecycle, freshness)
├── rag/ast/         # Scoped structural symbol graph: Extractor + SymbolStore interfaces (skeleton)
├── completion/      # IDE inline completion (Fill-in-the-Middle)
├── analysis/        # Domain-specific analysis helpers (code review, ML metrics, trading)
├── agent/           # Agent runtime: Orchestrator loop, effect-aware tools, serial/parallel dispatch, opt-in interceptor pipeline (#436)
├── agent/interceptor/ # Opt-in defaults: injection detectors, secret/payment-card blocking, argument-invariant guards, exec egress labels, scoped-child refusal reporting (#555)
├── mcp/             # MCP server: tools, prompts, resources over stdio/HTTP/2 — wired through provider.Router
├── mcpclient/       # MCP client: adapts external MCP servers' tools into agent.Tool (stdio/streamable-HTTP); consumed by cmd/golem
├── consult/         # /consult seam: bounded host runner + Claude and Codex subscription adapters (#382, #546); unsigned consult-result/v1 receipts
├── conversation/    # Persistent conversation storage with SQLite
├── memory/          # User memories + agent-memory records (SQLite, FTS5 search) + shared hardened-open primitives (open.go); backs Golem /remember, memory_search and MCP agent_memory_* tools
├── projectcontext/  # AGENTS.md-style project-context loader (discovery, safe read, ordering; consumed by cmd/golem)
├── recipe/          # Versioned JSON prompt bundles (v1): `Parse`/`Load`, closed schema, strict keys; expansion + discovery are #353
├── signing/         # Detached signatures over canonical JSON (ZT-301): Ed25519 + HMAC-SHA256, Keyring rotation, hardened key files; knows no record schema
├── feedback/        # Implicit user behavioral signal collection
├── fingerprint/     # Model profiling + the shared capability store (#734): append-only evidence log, per-credential listings (account HMAC never exported); `OpenSharedStore` is the one hardened opener; `CapabilityProber` + `probers/`
├── prefetch/        # Predictive cache-warming engine for RAG retrieval
├── compat/          # OpenAI-compatible endpoint shim (chat, completions, model aliases, concurrency limiter)
├── cmd/
│   ├── golem/       # Golem CLI agent REPL; input goes through one lineSource seam (x/term line editor on a TTY, bufio.Scanner otherwise)
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
- `golang.org/x/sys` — already required transitively; imported directly (build-tagged) by `agent/tools/` (exit watching, CoW cloning, no-replace install, exec directory read, Windows workspace identity), `cmd/golem/`'s PTY tests, and `profiles/`'s Windows directory-fsync
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

AI tools must not read, print, or transmit real credentials, a user's `models.json`, or the directories above unless the user asks in the current session, and must never commit them or paste them into an issue or PR; use the synthetic fixtures and mock servers. Never commit or `git add -f` `docs/superpowers/` or `.superpowers/`: they are private, and CI rejects them. In review, all code outside `docs/` and test data is security-relevant; highest risk: `agent/tools/` (exec sandbox), Golem trust and approval code, `projectcontext/`, `mcpclient/`, `agent/interceptor/`, `internal/secretscan/`, `config/`, `signing/`, `consult/`, `provider/`.

**AI policy:** no organization AI policy applies to this project.

## Changelog

Outside the stamp PR or a pre-tag fix to the newest version's body, never edit `CHANGELOG.md`; concurrent PRs conflict under `## [Unreleased]`. Add `changelog.d/<issue>-<slug>.md` holding the full `### <Category> — <title> (#<issue>)` section (see `changelog.d/README.md`). CI rejects other direct edits; `scripts/changelog-fold` folds fragments at release.

## Testing

```bash
# Unit tests (no Ollama needed)
go test ./...

# Integration tests (requires running Ollama)
go test -tags integration ./...
```

Mocks of a neighboring go-llm package test each side against your assumptions, not against each other. When separately built packages are joined, add a test in the default (untagged) run that wires the real packages together, mocking only the external server. Vary inputs on one instance (one prefetch cache asked for two different `k`), since identical repeated calls cannot reveal a key that omits a parameter, and round-trip every public field meant to persist.

When a review names one instance of a bug, fix the class: find every place the same assumption holds (every input to a cache key, every path that emits a counted event, the empty case of a cleanup) before calling the finding resolved.

## Code Style

- Standard Go conventions (gofmt, golint)
- Table-driven tests
- Descriptive error messages with `fmt.Errorf("ollama: %w", err)` pattern
- Package-level doc comments on every exported type and function
