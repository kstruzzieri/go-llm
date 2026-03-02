# Getting Started with go-llm

A guide to setting up and using go-llm with your local Ollama models.

## Prerequisites

1. **Go 1.23+** installed
2. **Ollama** running locally — [install](https://ollama.com/download)
3. Required models pulled (see [Model Configuration](#model-configuration))

## Install

```bash
go get github.com/kstruzzieri/go-llm
```

## Pull Required Models

```bash
# General purpose (reasoning, vision, conversation)
ollama pull qwen3.5:27b

# Fast MoE model (agents, tool use, high throughput)
ollama pull qwen3.5:35b-a3b

# Coding (code generation, review, FIM completion)
ollama pull qwen3-coder-next

# Lightweight (simple/fast tasks)
ollama pull qwen3:8b

# Embeddings (RAG vector search)
ollama pull qwen3-embedding:8b
```

## Model Configuration

go-llm uses `models.json` at the project root to define available models and their roles. Users can modify this file to swap models without changing code.

```json
{
  "models": {
    "general": { "name": "qwen3.5:27b" },
    "fast":    { "name": "qwen3.5:35b-a3b" },
    "coding":  { "name": "qwen3-coder-next:latest" },
    "embedding": { "name": "qwen3-embedding:8b" }
  },
  "defaults": {
    "chat": "general",
    "completion": "coding",
    "embedding": "embedding",
    "agent": "fast"
  }
}
```

### Choosing a Model

| Role | Model | When to Use |
|------|-------|-------------|
| `general` | qwen3.5:27b (17GB) | Complex reasoning, image analysis, long conversations |
| `fast` | qwen3.5:35b-a3b (23GB) | Agent loops, tool calling, rapid iteration. MoE = only 3B params active per token |
| `coding` | qwen3-coder-next (51GB) | Code generation, review, FIM completions |
| `lightweight` | qwen3:8b (5GB) | Quick answers, classification, low-stakes tasks |
| `embedding` | qwen3-embedding:8b (5GB) | Vector embeddings for RAG search |

### Swapping Models

To use different models (e.g., Llama, Mistral, DeepSeek), edit `models.json`:

```json
{
  "models": {
    "general": { "name": "llama3.3:70b" },
    "coding":  { "name": "deepseek-coder-v3:33b" }
  }
}
```

No code changes needed — just pull the models and update the config.

## Quick Start

### Chat

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
        Model: "qwen3.5:27b",
        Messages: []ollama.ChatMessage{
            {Role: "user", Content: "Explain MoE architectures in neural networks"},
        },
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(resp.Message.Content)
}
```

### Streaming Chat

```go
err := client.ChatStream(ctx, ollama.ChatRequest{
    Model:    "qwen3.5:35b-a3b",  // fast model for interactive use
    Messages: []ollama.ChatMessage{{Role: "user", Content: "Hello"}},
}, func(resp ollama.ChatResponse) error {
    fmt.Print(resp.Message.Content)
    return nil
})
```

### Code Generation

```go
resp, err := client.Generate(ctx, ollama.GenerateRequest{
    Model:  "qwen3-coder-next",
    Prompt: "Write a Go function that implements binary search on a sorted slice",
    Options: &ollama.ModelOptions{
        Temperature: 0.2,  // low temp for precise code
        NumPredict:  2048,
    },
})
```

### Embeddings

```go
embedding, err := client.Embed(ctx, "qwen3-embedding:8b", "search query text")
// Returns []float64 vector for similarity search
```

### RAG — Index and Search a Codebase

```go
import (
    "github.com/kstruzzieri/go-llm/ollama"
    "github.com/kstruzzieri/go-llm/rag"
)

client := ollama.NewClient()

// Create vector store
store, _ := rag.NewSQLiteStore("./vectors.db")
defer store.Close()

// Index a project directory
indexer := rag.NewIndexer(client, store,
    rag.WithEmbeddingModel("qwen3-embedding:8b"),
)
indexer.IndexDirectory(ctx, "./my-project")

// Search for relevant code
retriever := rag.NewRetriever(client, store,
    rag.WithRetrieverModel("qwen3-embedding:8b"),
)
results, _ := retriever.Retrieve(ctx, "how does authentication work", 5)

// Build context for LLM
context := retriever.BuildContext(results, 4096)

// Ask the LLM with retrieved context
resp, _ := client.Chat(ctx, ollama.ChatRequest{
    Model: "qwen3.5:27b",
    Messages: []ollama.ChatMessage{
        {Role: "system", Content: "Answer using the provided code context.\n\n" + context},
        {Role: "user", Content: "How does authentication work in this project?"},
    },
})
```

## Architecture

```
go-llm/
├── ollama/        # Ollama REST API client
│   ├── client.go  # HTTP client with functional options
│   ├── chat.go    # Chat completions (streaming + sync)
│   ├── generate.go# Text generation (streaming + sync)
│   ├── embed.go   # Embedding generation (single + batch)
│   ├── models.go  # Model management (list, show, pull)
│   └── types.go   # Request/response types
├── rag/           # Retrieval Augmented Generation
│   ├── store.go       # VectorStore interface
│   ├── sqlite_store.go# SQLite implementation (cosine similarity)
│   ├── chunker.go     # Text chunking interface + sliding window
│   ├── chunker_code.go# Code-aware chunking (Go, Python, TS, etc.)
│   ├── indexer.go     # File/directory indexing coordinator
│   └── retriever.go   # Query embedding + context building
├── completion/    # IDE inline completions (planned)
├── analysis/      # Domain analysis helpers (planned)
├── models.json    # Model configuration (user-editable)
└── docs/          # Documentation
```

## Hardware Requirements

| Setup | RAM | Recommended Models |
|-------|-----|-------------------|
| Minimal (8GB) | 8GB | qwen3:8b only |
| Standard (16-32GB) | 16-32GB | qwen3.5:27b + qwen3:8b + embedding |
| Full (64GB+) | 64GB+ | All models |
| Power (128GB+) | 128GB+ | All models, can run 2+ simultaneously |

## Consumers

- **Arc IDE** — custom Wails IDE with code completion and chat
- **Flux ML** — Wails ML development environment
- **Quantum Trader** — Go+Python trading platform with ML analysis
