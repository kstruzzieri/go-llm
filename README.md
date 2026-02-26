# go-llm

A shared Go module providing Ollama LLM integration and a lightweight RAG (Retrieval Augmented Generation) layer with SQLite-backed vector storage.

## Packages

| Package | Description |
|---------|-------------|
| `ollama/` | HTTP client for the Ollama REST API — chat, text generation, embeddings, model management. Streaming support via callbacks. |
| `rag/` | Code-aware text chunking, SQLite vector store with cosine similarity search, file/directory indexer, and context-building retriever. |
| `completion/` | IDE inline completion helpers (Fill-in-the-Middle). *(planned)* |
| `analysis/` | Domain-specific analysis — code review, ML metrics, trading strategy analysis. *(planned)* |

## Requirements

- Go 1.23+
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
        Model: "qwen2.5:72b",
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
    Model:    "qwen2.5:72b",
    Messages: []ollama.ChatMessage{{Role: "user", Content: "Hello"}},
}, func(resp ollama.ChatResponse) error {
    fmt.Print(resp.Message.Content)
    return nil
})
```

### Generate embeddings

```go
embedding, err := client.Embed(ctx, "nomic-embed-text", "mean reversion strategy")
// embedding is []float64 with 768 dimensions
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

indexer := rag.NewIndexer(client, store)
indexer.IndexDirectory(ctx, "/path/to/project")
```

### Query with RAG context

```go
retriever := rag.NewRetriever(client, store)
results, _ := retriever.Retrieve(ctx, "how does the pairs trading strategy work?", 5)
context := retriever.BuildContext(results, 4096)

// Feed context into a chat completion
resp, _ := client.Chat(ctx, ollama.ChatRequest{
    Model: "qwen2.5:72b",
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

- Atomic re-indexing: existing data is preserved if embedding fails
- Respects `.gitignore` (root-level, basic patterns)
- Configurable file extensions and exclusion patterns

```go
indexer.IndexDirectory(ctx, dir,
    rag.WithExtensions(".go", ".py", ".ts", ".md"),
    rag.WithExclude("node_modules", ".git", "vendor"),
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
info, _ := client.ShowModel(ctx, "qwen2.5:72b")
client.PullModel(ctx, "nomic-embed-text", func(status string, completed, total int64) {
    fmt.Printf("%s: %d/%d\n", status, completed, total)
})
```

## Dependencies

Minimal by design:

- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- Everything else is Go stdlib

## Testing

```bash
# Unit tests (no Ollama required)
go test ./...

# With verbose output
go test ./... -v
```

## License

MIT
