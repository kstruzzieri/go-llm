# go-llm as a Go library

Deep-dive reference for embedding go-llm's packages in your own application:
chat and streaming, tool calling, embeddings, RAG indexing and retrieval, the
Ollama client, Fill-in-the-Middle completion, model configuration, Parquet
export, and analysis helpers. For installation and backend setup, start from
the [README](../README.md) and [Getting Started](GETTING_STARTED.md).

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
