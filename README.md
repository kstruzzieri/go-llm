# go-llm

A batteries-included Go module for building local-first AI features on top of [Ollama](https://ollama.com). Provides a complete pipeline from model management and configuration through RAG-powered retrieval to domain-specific analysis — all running locally with no cloud dependencies or API keys.

Designed for embedding into Go applications that need LLM capabilities: chat, tool calling, code completion, retrieval-augmented generation, and more. Also runs as a standalone [MCP server](#mcp-server) for use as a local AI service without embedding. Pure Go with minimal dependencies (no CGo).

### What's included

- **Ollama client** — chat, completions, embeddings, model management, and tool calling with streaming support
- **RAG pipeline** — code-aware chunking, SQLite vector store, concurrent indexing with `.gitignore` support, and context-building retrieval
- **FIM completion** — Fill-in-the-Middle for IDE inline suggestions with context window management
- **Model config** — `models.json`-driven configuration with provider settings, role-based defaults, and fallback chain resolution
- **Parquet export** — ML pipeline interop with quality metrics and configurable precision
- **Analysis helpers** — code review, ML training metrics, and trading strategy analysis

## Packages

| Package | Description |
|---------|-------------|
| `ollama/` | HTTP client for the Ollama REST API — chat, text generation, embeddings, model management, tool calling. Streaming support via callbacks. |
| `config/` | Model configuration loader (`models.json`) with provider settings, role-based defaults, and fallback chain resolution against available models. |
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
| `compat/` | OpenAI-compatible endpoint shim — chat, completions, model aliases, and a concurrency limiter for clients that speak OpenAI's API but want to target local Ollama models. |
| `cmd/go-llm-mcp/` | Standalone MCP server binary with stdio and HTTP/2 support. |
| `cmd/fim-smoke/` | Smoke-test harness for Fill-in-the-Middle completion against a running Ollama. |
| `cmd/llm-bench/` | Latency benchmark for the configured model lineup. |

## Requirements

- Go 1.25+
- [Ollama](https://ollama.com) running locally (default: `http://localhost:11434`)

## Installation

```bash
go get github.com/kstruzzieri/go-llm
```

## Quick Start

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

provider := completion.NewProvider(client, "qwen3-coder-next")

resp, _ := provider.Complete(ctx, completion.FIMRequest{
    Prefix:    "func fibonacci(n int) int {\n\t",
    Suffix:    "\n}",
    FilePath:  "math.go",
    MaxTokens: 128,
})
fmt.Println(resp.Completion)

// Streaming variant
provider.CompleteStream(ctx, req, func(token string) error {
    fmt.Print(token)
    return nil
})
```

## Model Configuration

Load model settings from `models.json` with provider configs, role-based defaults, and fallback chains that resolve against available Ollama models.

`go-llm` does not hard-code a model roster — `models.json` is the sole source of truth. Substitute any model your configured provider can load by editing `models.json`; capabilities (chat / embedding / tool-call) are detected at runtime by `fingerprint/`. See [`docs/llm/`](docs/llm/) for the reference lineup shipped by default and the full BYO guide.

```go
import "github.com/kstruzzieri/go-llm/config"

cfg, _ := config.Default() // auto-discovers models.json

// Simple lookup
model := cfg.ModelFor("chat") // e.g., "gemma4:31b"

// Resolve with fallback chain (checks which models are actually available)
resolved, _ := cfg.Resolve(ctx, client, "chat")
fmt.Printf("Using %s (fallback: %v)\n", resolved.Name, resolved.IsFallback)
```

## MCP Server

Expose all go-llm capabilities over the [Model Context Protocol](https://modelcontextprotocol.io/) for use with Claude Desktop, IDE extensions, or any MCP client.

```bash
# Build
go build -o go-llm-mcp ./cmd/go-llm-mcp/

# Stdio (Claude Desktop, IDE integration)
./go-llm-mcp --transport stdio

# HTTP/2 (local development)
./go-llm-mcp --transport http --addr 127.0.0.1:8080

# Custom Ollama URL
./go-llm-mcp --ollama-url http://gpu-server:11434
```

Claude Desktop configuration (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "go-llm": {
      "command": "/path/to/go-llm-mcp",
      "args": ["--transport", "stdio"]
    }
  }
}
```

The server exposes 19 tools (chat, generate, code completion, embeddings, RAG, model management, analysis), 4 prompt templates, 7 concrete resources, and 1 resource template. Chat, generate, completion, embedding, and analysis tools accept an optional `model` parameter; when omitted, the request is routed by `provider.Router` using a use-case-appropriate weight profile (chat / fim / embedding / reasoning / analysis / code-review / agent), with circuit-breaker-aware fallback. Routing state for diagnostics is exposed via the `route://breakers`, `route://warmth`, and `route://sticky` resources. (The actual model that served a given call is computed internally as `RouteOutcome.ActualModel` but is not currently included in tool responses; see Roadmap.)

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

## Roadmap

### Recently shipped

| Feature | Description |
|---------|-------------|
| Provider Router → MCP | `mcp/` chat/generate/embed/completion tools and analysis handlers route through `provider.Router` with use-case-aware weight profiles, circuit breakers, warmth scoring, and sticky preference. Routing state surfaced via `route://breakers`, `route://warmth`, `route://sticky` resources. |
| FIM via Router | Completion routing with FIM-family pinning, template prompt support, and empty-suffix semantics — `provider.Router` chooses the actual completion model under the hood. |

### In progress

| Feature | Description |
|---------|-------------|
| OpenAI-compatible endpoint | `compat/` package exposes local Ollama models via an OpenAI-compatible chat/completions API for clients that speak OpenAI's API but want a local backend. Concurrency limiter and model-alias resolution are in place; further hardening ongoing. |
| Persistent drift signature | Per-chunk vector-space identity in the RAG SQLite store so cross-run drift across embedding-model boundaries is detected at query time. Closes the chain-fallback channel that the in-memory drift guard left open. |

### Future

| Feature | Description |
|---------|-------------|
| Agentic RAG | Opt-in agentic retrieval layer planned on top of static RAG. Static RAG remains the default. Phase 0 gates are active. |
| In-band routing transparency | Surface `RouteOutcome` (actual model, fallbacks used, sticky decision) in MCP tool responses so callers see which model served a request rather than only the planned default. Out-of-band today via `route://*` resources. |
| Vision support | Image inputs in chat messages |
| ANN search | Approximate nearest neighbor search for large vector stores |

## Dependencies

Minimal by design:

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- `golang.org/x/sync` — concurrency primitives (bounded worker pools for indexing)
- `golang.org/x/net` — h2c HTTP/2 cleartext transport (only imported by `mcp/`)
- `github.com/modelcontextprotocol/go-sdk` — official MCP Go SDK (only imported by `mcp/`)
- `github.com/parquet-go/parquet-go` — Parquet file writer (only imported by `rag/parquet/`)

## Testing

```bash
# Unit tests (no Ollama required)
go test ./...

# With verbose output
go test ./... -v
```

### Local CI

Enable the Docker-backed pre-push hook once per clone:

```bash
git config core.hooksPath .githooks
```

Run the same full suite manually:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

`full` includes `golangci-lint run`, `go test -race ./...`, and `go test -run '^$' ./...`. The pre-push hook runs that full suite automatically before pushes. GitHub still runs the required `Lint & Test` workflow on PRs to satisfy branch protection; push-triggered Actions and macOS smoke remain disabled unless manually dispatched. See [`docs/local-ci.md`](docs/local-ci.md) for the full local CI workflow.

## License

MIT
