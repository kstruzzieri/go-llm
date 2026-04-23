# Getting Started with go-llm

A guide to setting up and using go-llm with your local Ollama models.

## Prerequisites

1. **Go 1.25+** installed
2. **Ollama** running locally — [install](https://ollama.com/download)
3. Required models pulled (see [Model Configuration](#model-configuration))

## Install

```bash
go get github.com/kstruzzieri/go-llm
```

## Pull Required Models

These pulls match the current reference lineup checked into `models.json`:

```bash
# General / agent / analysis (reasoning, multimodal chat, tool use)
ollama pull gemma4:31b

# Fast fallback / lower-latency chat
ollama pull qwen3.6:35b-a3b

# Coding (code generation, review, FIM completion)
ollama pull qwen3-coder-next:latest

# Lightweight (simple/fast tasks)
ollama pull qwen3:8b

# Embeddings (RAG vector search)
ollama pull qwen3-embedding:8b
```

If you customize `models.json`, pull the models you configured instead.
See [llm/recommendation.md](llm/recommendation.md) for the current
reference lineup and BYO-model guidance.

## Model Configuration

The `models.json` file at the project root configures which models are used for each task. Load it with the `config` package:

```go
import "github.com/kstruzzieri/go-llm/config"

cfg := config.MustLoad("models.json")

// Resolve model names by use-case
chatModel := cfg.MustModelFor("chat")            // "gemma4:31b"
codeModel := cfg.MustModelFor("completion")      // "qwen3-coder-next:latest"
embedModel := cfg.MustModelFor("embedding")      // "qwen3-embedding:8b"

// Get provider connection info
providerCfg := cfg.Provider("ollama")
client := ollama.NewClient(ollama.WithBaseURL(providerCfg.BaseURL))
```

Models are organized by **role** (general, coding, embedding, etc.) with a **defaults** map linking use-cases to roles. Each model can specify **fallbacks** — if the preferred model isn't available in Ollama, the config resolver will try alternatives automatically:

```go
// Check availability and fall back if needed
resolved, err := cfg.Resolve(ctx, client, "chat")
if resolved.IsFallback {
    log.Printf("using fallback model %s instead of primary", resolved.Name)
}
```

Consumer applications (Firn IDE, Flux ML) can also use `config.Default()` to discover `models.json` automatically.

### Choosing a Model

| Role | Model | When to Use |
|------|-------|-------------|
| `general` | `gemma4:31b` (~20GB) | Complex reasoning, multimodal chat, long conversations |
| `agent` | `gemma4:31b` (~20GB) | Tool use, agent loops, function calling |
| `fast` | `qwen3.6:35b-a3b` (~28GB) | Lower-latency chat and strong fallback path |
| `coding` | `qwen3-coder-next:latest` (~46GB) | Code generation, review, FIM completions |
| `lightweight` | `qwen3:8b` (~6GB) | Quick answers, classification, low-stakes tasks |
| `embedding` | `qwen3-embedding:8b` (~5GB) | Vector embeddings for RAG search |

The `analysis` use-case currently maps to the `general` role in
`models.json`, so it also resolves to `gemma4:31b` unless you customize
the defaults.

### Swapping Models

To use different models, edit `models.json`:

```json
"general": {
  "name": "llama3.3:70b",
  "type": "dense",
  ...
}
```

Pull the new model (`ollama pull llama3.3:70b`) and restart your application.

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

### Streaming Chat

```go
err := client.ChatStream(ctx, ollama.ChatRequest{
    Model:    "qwen3.6:35b-a3b",  // fast model for interactive use
    Messages: []ollama.ChatMessage{{Role: "user", Content: "Hello"}},
}, func(resp ollama.ChatResponse) error {
    fmt.Print(resp.Message.Content)
    return nil
})
```

### Code Generation

```go
resp, err := client.Generate(ctx, ollama.GenerateRequest{
    Model:  "qwen3-coder-next:latest",
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
    Model: "gemma4:31b",
    Messages: []ollama.ChatMessage{
        {Role: "system", Content: "Answer using the provided code context.\n\n" + context},
        {Role: "user", Content: "How does authentication work in this project?"},
    },
})
```

### MCP Server

The MCP server exposes all go-llm capabilities over the [Model Context Protocol](https://modelcontextprotocol.io/) for use with Claude Desktop, IDE extensions, or any MCP client.

#### Standalone Binary

```bash
# Build
go build -o go-llm-mcp ./cmd/go-llm-mcp/

# Stdio transport (for Claude Desktop / IDE integration)
./go-llm-mcp --transport stdio

# HTTP/2 transport (local development)
./go-llm-mcp --transport http --addr 127.0.0.1:8080

# With custom Ollama URL and config
./go-llm-mcp --ollama-url http://gpu-server:11434 --config /path/to/models.json

# Disable RAG tools
./go-llm-mcp --no-rag

# HTTP/2 with TLS (remote deployment)
./go-llm-mcp --transport http --addr 0.0.0.0:443 --tls-cert cert.pem --tls-key key.pem
```

#### Claude Desktop Configuration

Add to `claude_desktop_config.json`:

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

#### Embedded in a Go Application

```go
import "github.com/kstruzzieri/go-llm/mcp"

srv, err := mcp.NewServer(ctx,
    mcp.WithOllamaURL("http://localhost:11434"),
    mcp.WithConfig("models.json"),
    mcp.WithRAGPath("./vectors.db"),
)
if err != nil {
    log.Fatal(err)
}
defer srv.Shutdown(ctx)

// Stdio (connects to parent process)
srv.ListenStdio(ctx)

// Or HTTP/2 (network)
srv.ListenHTTP(ctx, "127.0.0.1:8080")
```

The server provides 19 tools (chat, generate, code completion, embeddings, RAG, model management, analysis), 4 prompt templates, and 5 resources. Chat, generate, completion, embedding, and analysis tools accept an optional `model` parameter; when omitted, the configured default for that use-case is used.

## Architecture

```
go-llm/
├── ollama/          # Ollama REST API client (chat, generate, embeddings, models)
├── config/          # Model configuration loader (models.json, resolve, fallback)
├── provider/        # Intelligent model routing (Router, circuit breakers, warmth, scoring)
├── rag/             # RAG: chunking, SQLite vector store, indexing, retrieval
├── completion/      # IDE inline completion (Fill-in-the-Middle)
├── analysis/        # Domain-specific analysis (code review, ML metrics, trading)
├── mcp/             # MCP server: tools, prompts, resources over stdio/HTTP/2
├── conversation/    # Persistent conversation storage with SQLite
├── feedback/        # Implicit user behavioral signal collection
├── fingerprint/     # Model profiling (latency benchmarks, capability detection)
├── prefetch/        # Predictive cache-warming engine for RAG retrieval
├── cmd/go-llm-mcp/  # Standalone MCP server binary
├── models.json      # Model configuration (loaded by config package)
└── testdata/        # Test fixtures
```

## Hardware Requirements

| Setup | RAM | Recommended Models |
|-------|-----|-------------------|
| Minimal (8GB) | 8GB | `qwen3:8b` only |
| Standard (16-32GB) | 16-32GB | `qwen3:8b` + `qwen3-embedding:8b` |
| Large (32-64GB) | 32-64GB | One primary model at a time (`gemma4:31b` or `qwen3.6:35b-a3b`), adding smaller models only if memory allows |
| Power (128GB+) | 128GB+ | Full reference lineup from `models.json` |

## Consumers

- **Firn IDE** — custom Wails IDE with code completion and chat
- **Flux ML** — Wails ML development environment
- **Quantum Trader** — Go+Python trading platform with ML analysis
