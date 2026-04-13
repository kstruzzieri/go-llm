# CLAUDE.md

## Project Overview

`go-llm` is a shared Go module providing Ollama LLM integration (chat, completions, embeddings) and a lightweight RAG layer with SQLite-backed vector storage.

**Consumers:** Firn IDE (custom Wails IDE), Flux ML (Wails ML dev environment), Quantum Trader (Go+Python trading platform)

## Architecture

```
go-llm/
├── ollama/          # Ollama REST API client (chat, generate, embeddings, models)
├── config/          # Model configuration loader (models.json, resolve, fallback)
├── provider/        # Intelligent model routing (Router, circuit breakers, warmth, scoring)
├── rag/             # RAG: chunking, SQLite vector store, indexing, retrieval
├── completion/      # IDE inline completion (Fill-in-the-Middle)
├── analysis/        # Domain-specific analysis helpers (code review, ML metrics, trading)
├── mcp/             # MCP server: tools, prompts, resources over stdio/HTTP/2
├── conversation/    # Persistent conversation storage with SQLite
├── feedback/        # Implicit user behavioral signal collection
├── fingerprint/     # Model profiling (latency benchmarks, capability detection)
├── prefetch/        # Predictive cache-warming engine for RAG retrieval
├── cmd/go-llm-mcp/  # Standalone MCP server binary
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
- `github.com/modelcontextprotocol/go-sdk` — official MCP Go SDK (only imported by `mcp/`)
- `github.com/parquet-go/parquet-go` — Parquet file writer (only imported by `rag/parquet/`)

Everything else uses stdlib (`net/http`, `encoding/json`, `math`, `context`, etc.)

## Design Principles

1. **Every public method takes `context.Context`** — cancellation is critical (IDE completions get cancelled constantly)
2. **Streaming is first-class** — both chat and completions support streaming via callback functions
3. **No global state** — multiple Client instances can coexist with different configs
4. **Interfaces for extensibility** — `VectorStore` interface allows swapping SQLite for other backends
5. **Errors are informative** — wrap with context, include HTTP status codes from Ollama
6. **Tests use mock HTTP servers** — no Ollama dependency for unit tests; integration tests behind build tag

## Ollama API Reference

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
  "model": "qwen3.5:27b",
  "messages": [{"role": "user", "content": "hello"}],
  "stream": true,
  "options": {
    "temperature": 0.7,
    "num_predict": 2048,
    "num_ctx": 32768
  }
}
```

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
