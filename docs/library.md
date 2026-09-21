# go-llm as a Go library

Deep-dive reference for embedding go-llm's packages in your own application:
chat and streaming, tool calling, embeddings, RAG indexing and retrieval, the
Ollama client, Fill-in-the-Middle completion, model configuration, Parquet
export, and analysis helpers. For installation and backend setup, start from
the [README](../README.md) and [Getting Started](GETTING_STARTED.md).

## Packages

| Package | Description |
|---------|-------------|
| `ollama/` | HTTP client for the Ollama REST API — chat, text generation, embeddings, model management, tool calling. Streaming support via callbacks. |
| `config/` | Model configuration loader (`models.json`) with provider settings, role-based defaults, fallback chain resolution, role lifecycle, selector overrides, and credential scrub via a secret-literal-preserving atomic writer. |
| `configview/` | Pure projection of a config for panels/CLI/MCP — a versioned wire contract with tri-state candidate eligibility, no I/O. Consumed by `golem models -json`, the MCP configview resource, and the Firn config panel. |
| `configio/` | Explicit I/O tier for the config stack — provider inventory refresh and consent-gated per-model probes with bounded error codes. Never implicit; values in, values out. |
| `profiles/` | Profile catalog — curated embedded configs (credential-free by pinned rule) plus a user store under a private directory boundary, with stable IDs and bounded error codes. |
| `agent/` | Agent runtime — plan-act-observe loop, tool registry, observers, budgets, approval seams, and the sandboxed exec backends (Seatbelt, Bubblewrap). |
| `golem/` | Embeddable Golem runtime — the system prompt and agent wiring behind `cmd/golem`, for consumers that embed the agent instead of shelling out. |
| `agentflow/` | AgentFlow integration — locked plan validation, journaled execution, and proof artifacts for task-mode runs. |
| `memory/` | Explicit user-controlled local memories and agent-memory records (SQLite, scope-filtered FTS5 search). Backs Golem `/remember` and the MCP agent-memory tools; see [agent-memory provenance and integrity](memory.md). |
| `mcpclient/` | MCP client — adapts external MCP servers' tools into agent tools over stdio or streamable HTTP. |
| `consult/` | `/consult` seam — a bounded host runner plus the Claude and Codex subscription adapters, producing unsigned `consult-result/v1` receipts. Not a provider, a Router member, or a tool; see [consulting an external subscription CLI](consult.md). |
| `projectcontext/` | AGENTS.md-style project-context loader — discovery, safe capped reads, and deterministic ordering. |
| `recipe/` | Versioned JSON prompt bundles — `Parse` for embedded bytes, `Load` for explicit paths with regular-file and identity checks and a 64 KiB bound. Closed schema, strict keys, advisory role/use-case hints; see [docs/recipes.md](recipes.md). |
| `provider/` | Intelligent model routing — Router with circuit breakers, warmth tracking, token budget, sticky routing, and multi-model scoring. |
| `rag/` | Code-aware text chunking, SQLite vector store with cosine similarity and FTS5 hybrid search, concurrent file/directory indexer with `.gitignore` support, diff-aware incremental reindexing, and context-building retriever. |
| `rag/parquet/` | Parquet dataset exporter for ML pipeline interop — exports vector store contents with quality metrics and configurable precision. |
| `completion/` | IDE inline completion via Fill-in-the-Middle (FIM) with context window management. Sync and streaming APIs. |
| `analysis/` | Domain-specific analysis helpers — code review (with optional RAG context), ML training metrics, and trading strategy analysis. |
| `mcp/` | MCP server exposing go-llm as tools, prompts, and resources over stdio and HTTP/2 transports. Tool calls flow through `provider.Router`. |
| `conversation/` | Persistent conversation storage with SQLite. |
| `feedback/` | Implicit user behavioral signal collection for retrieval quality improvement. |
| `fingerprint/` | Model profiling — latency benchmarks and capability detection. |
| `prefetch/` | Predictive cache-warming engine for RAG retrieval. |
| `compat/` | OpenAI-compatible endpoint shim — chat, completions, model aliases, and a concurrency limiter so clients that speak OpenAI's API can target local models served through go-llm (distinct from the `openai-compat` *provider*, which consumes an upstream OpenAI `/v1` server such as llama.cpp). |
| `cmd/golem/` | Terminal coding agent built on `agent/`, `provider.Router`, file/search tools, optional RAG retrieval, persistent sessions, and approval-gated write/exec. |
| `cmd/go-llm-mcp/` | Standalone MCP server binary with stdio and HTTP/2 support. |
| `cmd/fim-smoke/` | Smoke-test harness for Fill-in-the-Middle completion against a running backend. |
| `cmd/llm-bench/` | Model evaluation harness — replays trace corpora against candidate models (llama.cpp via `openai-compat`, or Ollama) and reports AnswerQuality, tool-use, tool-restraint, latency, and tokens with paired deltas and bootstrap CIs. |


## Use as a Go library

### Chat with a local model

```go
package main

import (
    "context"
    "fmt"
    "github.com/kstruzzieri/go-llm/ollama"
)

func main() {
    client := ollama.NewClient()

    resp, err := client.Chat(context.Background(), ollama.ChatRequest{
        Model: "gemma4:31b",
        Messages: []ollama.ChatMessage{
            {Role: "user", Content: "Explain walk-forward validation for trading strategies"},
        },
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(resp.Message.Content)
}
```

### Streaming chat

```go
err := client.ChatStream(ctx, ollama.ChatRequest{
    Model:    "gemma4:31b",
    Messages: []ollama.ChatMessage{{Role: "user", Content: "Hello"}},
}, func(resp ollama.ChatResponse) error {
    fmt.Print(resp.Message.Content)
    return nil
})
```

### Tool calling

```go
// Define a tool with the builder API
weatherTool := ollama.NewTool(
    "get_weather",
    "Get current weather for a location",
    ollama.ObjectParams(
        ollama.Param("location", ollama.ParamTypeString, "City name"),
        ollama.Param("unit", ollama.ParamTypeString, "Temperature unit").
            WithEnum("celsius", "fahrenheit"),
    ).Required("location"),
)

// Send a chat request with tools
resp, _ := client.Chat(ctx, ollama.ChatRequest{
    Model:    "gemma4:31b",
    Messages: []ollama.ChatMessage{{Role: "user", Content: "What's the weather in NYC?"}},
    Tools:    []ollama.Tool{weatherTool},
})

// The model may respond with tool calls
if len(resp.Message.ToolCalls) > 0 {
    call := resp.Message.ToolCalls[0]
    // Execute the tool, then return the result
    result := ollama.ToolResultMessageFor(call, `{"temp": 72, "unit": "fahrenheit"}`)
    // Continue the conversation with the tool result...
}
```

### Generate embeddings

```go
embedding, err := client.Embed(ctx, "qwen3-embedding:8b", "mean reversion strategy")
// embedding is []float64 with 4096 dimensions
```

### Index a codebase for RAG

```go
import (
    "github.com/kstruzzieri/go-llm/ollama"
    "github.com/kstruzzieri/go-llm/rag"
)

client := ollama.NewClient()
store, _ := rag.NewSQLiteStore("vectors.db")
defer store.Close()

indexer := rag.NewIndexer(client, store,
    rag.WithEmbeddingModel("qwen3-embedding:8b"),
)
indexer.IndexDirectory(ctx, "/path/to/project")
```

### Query with RAG context

```go
retriever := rag.NewRetriever(client, store,
    rag.WithRetrieverModel("qwen3-embedding:8b"),
)
results, _ := retriever.Retrieve(ctx, "how does the pairs trading strategy work?", 5)
context := retriever.BuildContext(results, 4096)

// Feed context into a chat completion
resp, _ := client.Chat(ctx, ollama.ChatRequest{
    Model: "gemma4:31b",
    Messages: []ollama.ChatMessage{
        {Role: "system", Content: "Answer using the following code context:\n\n" + context},
        {Role: "user", Content: "How does the pairs trading strategy calculate hedge ratios?"},
    },
})
```

## Conversation persistence

`conversation.SQLiteStore` saves complete conversation snapshots. `Load` returns
the stored `Conversation.Revision`; pass that revision back to `Save` with the
edited snapshot. Revision zero means create-only. A successful create stores
revision 1, and each successful update stores the submitted revision plus one.
`Save` takes a value and does not mutate it: if you retain the submitted snapshot,
increment its revision only after `Save` returns nil. Do not load a newer revision
and attach it to old messages; load and reconcile the complete snapshot instead.

An existing ID on create, a stale revision, or a missing ID on update returns an
error matching `conversation.ErrConflict`. Use `errors.As` with
`*conversation.ConflictError` to inspect its `ID` and `ExpectedRevision`.
Negative revisions and the maximum int64 revision are rejected without writing.
The transcript, durable summary, revision, and search projection commit in one
transaction; a conflict leaves the winning snapshot and index intact.

Golem returns a completed answer alongside `golem.ErrSessionPersistence` and the
underlying conflict if the raw turn could not be saved. Its terminal event is
`run.failed` with code `session_conflict`; it does not automatically retry or
replay tools. The CLI REPL reports such a turn as an error rather than a
"session not saved" warning. SQLite busy errors, including lock timeouts, are
treated the same way and retain their original database error rather than
becoming CAS conflicts: both mean another writer holds or advanced the session,
unlike an environmental disk failure, which stays a warning. Only the REPL
persists sessions; one-shot `-p` runs imply `-no-session` and never reach this
path. A subsequent explicit turn loads current durable history. The interactive
notice after a refused save (conflict or busy) explains that this history
excludes the unsaved turn and that `/new` starts a separate session. Explicit
`CompactThread` conflicts return an unchanged report and the typed error.
Automatic compression runs after the raw turn has committed, so its conflict is
an `OnWarning` notification and the turn remains successful. Hosts that omit
`OnWarning` retain quiet best-effort compression behavior.

Implementations supplied through `golem.Options.SessionStore` must implement
the same atomic revision check and exact revision-plus-one success contract,
even though the Go method signatures are unchanged. The runtime advances its
retained revision between the raw save and an automatic compression save.

Opening an existing database applies migration v4 through
`conversation_schema_version`, preserving its data and giving existing rows
revision 1. Upgrade and restart all processes writing a shared sessions database
together: older binaries can still perform unconditional writes and bypass CAS.
The guarantee applies while a row continuously exists. `Delete` and `/clear`
remove it, and recreating the same ID restarts at revision 1; an old snapshot may
then match that reused revision. This release does not add incarnation tokens,
tombstones, or protection against that deletion/recreation race (#542).

Conversation and memory migration runners coordinate concurrent openers through
SQLite's write lock. Each step claims its version before running, then commits
the version row and schema changes together. A competing opener skips a step
already committed by another opener. Failed steps roll back while earlier
committed steps remain. Callers must configure `busy_timeout` on every connection
that may migrate (for example, with the DSN `_pragma=busy_timeout(5000)`); a zero
or expired timeout can still return a busy error. Cancellation takes effect
between statements: a claim already waiting on the write lock keeps waiting, up
to the busy timeout, before the canceled context is reported. Current-schema
opens require only reads; memory record signing initialization is separate and
may write. This coordination covers the migration runners. Caller setup,
including the initial `journal_mode=WAL` switch, must complete before racing the
runners.

## RAG Details

### Chunking

The code-aware chunker splits files at function/method/class boundaries for Go, Python, TypeScript, JavaScript, Rust, Java, and Ruby. Unknown file types fall back to a sliding window chunker.

```go
chunker := rag.NewCodeChunker(
    rag.WithMaxChunkSize(1500),
    rag.WithOverlap(200),
)
```

### Vector Store

SQLite-backed with brute-force cosine similarity search. Performant for codebases up to ~100k chunks (~50ms search). In-memory mode available for testing.

```go
// File-backed (production)
store, _ := rag.NewSQLiteStore("vectors.db")

// In-memory (testing)
store, _ := rag.NewSQLiteStore(":memory:")
```

### Indexing

- **Concurrent**: configurable worker pool (default: 4 workers) via `golang.org/x/sync/errgroup`
- **Atomic**: existing data is preserved if embedding fails mid-index
- **`.gitignore`-aware**: automatically loads root and nested `.gitignore` files (globs, `**` wildcards, directory-only rules). Note: negation patterns (`!`) cannot re-include files inside an ignored directory because the directory tree is skipped eagerly
- Configurable file extensions and exclusion patterns

```go
indexer.IndexDirectory(ctx, dir,
    rag.WithExtensions(".go", ".py", ".ts", ".md"),
    rag.WithExclude("node_modules", ".git", "vendor"),
    rag.WithConcurrency(8), // default: 4
)
```

## Ollama Client

### Options

```go
client := ollama.NewClient(
    ollama.WithBaseURL("http://localhost:11434"),  // default
    ollama.WithTimeout(5 * time.Minute),           // default
)
```

### Model Management

```go
models, _ := client.ListModels(ctx)
info, _ := client.ShowModel(ctx, "gemma4:31b")
client.PullModel(ctx, "qwen3:8b", func(status string, completed, total int64) {
    fmt.Printf("%s: %d/%d\n", status, completed, total)
})
```

## Inline Completion (FIM)

Fill-in-the-Middle completion for IDE integration with automatic context window management.

```go
import "github.com/kstruzzieri/go-llm/completion"

// cfg.FIM comes from the resolved model profile; the model must support
// native prefix+suffix FIM. See cmd/fim-smoke for the full registry wiring.
cfg, err := completion.ProviderConfigFromProfile(profile) // profile: *provider.ModelProfile
if err != nil {
    log.Fatal(err) // model does not support native FIM
}

prov, err := completion.NewProvider(client, "qwen3-coder-next", cfg)
if err != nil {
    log.Fatal(err)
}

req := completion.FIMRequest{
    Prefix:    "func fibonacci(n int) int {\n\t",
    Suffix:    "\n}",
    FilePath:  "math.go",
    MaxTokens: 128,
}

resp, err := prov.Complete(ctx, req)
if err != nil {
    log.Fatal(err)
}
fmt.Println(resp.Completion)

// Streaming variant
err = prov.CompleteStream(ctx, req, func(token string) error {
    fmt.Print(token)
    return nil
})
```

## Model Configuration

Load model settings from `models.json` with provider configs, role-based defaults, and fallback chains that resolve against available provider models.

`go-llm` does not hard-code a model roster — `models.json` is the sole source of truth. Substitute any model your configured provider can load by editing `models.json`; capabilities (chat / embedding / tool-call) are detected at runtime by `fingerprint/`. See [`docs/llm/`](llm/) for the reference lineup shipped by default and the full BYO guide.

Model entries may set static sampling defaults with `options`:

```json
"coding": {
  "name": "qwen3-coder-next:latest",
  "provider": "llamacpp",
  "type": "moe",
  "options": { "temperature": 0.15, "top_p": 0.9, "top_k": 40 }
}
```

Defaults are keyed by provider/model identity; roles that share the same model
must declare identical options. Explicit request values, including zero, win.
`top_k` is a llama.cpp/Ollama extension, so omit it for strict hosted OpenAI
endpoints that reject unknown request fields.

```go
import "github.com/kstruzzieri/go-llm/config"

cfg, _ := config.Default() // auto-discovers models.json

// Simple lookup
model := cfg.ModelFor("chat") // e.g., "gemma4:31b"

// Resolve with fallback chain (checks which models are actually available)
resolved, _ := cfg.Resolve(ctx, client, "chat")
fmt.Printf("Using %s (fallback: %v)\n", resolved.Name, resolved.IsFallback)
```

### Auxiliary model defaults

`models.json` can optionally define side-task defaults for runtime helpers:
`summarize`, `route`, `rerank`, `verify`, `extract`, `approval`, and `vision`.
If one is omitted, go-llm falls back to existing defaults:

| Side task | Fallback defaults |
| --- | --- |
| `summarize` | `analysis`, then `chat` |
| `route` | `analysis`, then `chat` |
| `rerank` | `analysis`, then `chat` |
| `verify` | `analysis`, then `chat` |
| `extract` | `analysis`, then `chat` |
| `approval` | `agent`, then `chat` |
| `vision` | `chat` |

Explicit side-task defaults always win:

```json
{
  "defaults": {
    "chat": "general",
    "analysis": "general",
    "agent": "agent",
    "summarize": "lightweight"
  }
}
```

`ModelFor`, `Resolve`, `ResolveCandidates`, and `RoleFallbackChain` all apply
this fallback behavior. `ResolveAll` only enumerates defaults explicitly present
in `models.json`.

The `vision` slot is model selection only; image message payload support is
tracked separately.

The auxiliary use-case keys are exported as untyped string constants
(`config.UseCaseSummarize`, `config.UseCaseRerank`, and the rest), enumerated by
`config.SideTaskUseCases()`, and resolved to a model role by
`cfg.RoleForUseCase(useCase)` — the same fallback semantics, exposed for callers
that pick a side-task model without walking the full chain.
## Parquet Export

Export vector store contents to Parquet format for ML pipeline interop.

```go
import "github.com/kstruzzieri/go-llm/rag/parquet"

info, _ := parquet.ExportVectorStore(ctx, store, "dataset.parquet",
    parquet.WithDType(parquet.Float32),
    parquet.WithSourcePattern("*.go"),
    parquet.WithModel("qwen3-embedding:8b"),
)
fmt.Printf("Exported %d rows (%d clean, %d flagged)\n",
    info.RowCount, info.Quality.CleanRows, info.Quality.FlaggedRows)
```

## Analysis

Domain-specific analysis helpers that leverage Ollama models.

```go
import "github.com/kstruzzieri/go-llm/analysis"

// Code review (optionally backed by RAG context)
reviewer, _ := analysis.NewCodeReviewer(client, retriever, "gemma4:31b")
review, _ := reviewer.Review(ctx, code, analysis.WithLanguage("go"))

// ML training metrics analysis
analyzer, _ := analysis.NewMetricsAnalyzer(client, "gemma4:31b")
insight, _ := analyzer.AnalyzeTraining(ctx, analysis.TrainingMetrics{
    Epoch: 10, Loss: 0.42, LearningRate: 1e-4,
})
```
