# Getting Started with go-llm

A guide to setting up and using go-llm with local or hosted models. **llama.cpp is
the recommended primary backend** (best local performance, via its
OpenAI-compatible server); Ollama is fully supported as an alternative. See
[Local model backends](../README.md#local-model-backends) for the full backend
reference.

## Prerequisites

1. **Go 1.25+** installed
2. A model backend — one of:
   - **llama.cpp** (recommended) — `llama-server` per model, exposing the OpenAI-compatible API
   - **Ollama** (alternative) — running locally ([install](https://ollama.com/download))
   - **A hosted OpenAI-compatible API** — no local model needed; see
     [Running Golem against a hosted API](#running-golem-against-a-hosted-api)
3. Models available to your backend (see [Model Configuration](#model-configuration))

## Install

```bash
go get github.com/kstruzzieri/go-llm
```

## Serve the models

These models match the current reference lineup checked into `models.json`.

**llama.cpp via llama-swap (recommended).** A `llama-server` process pins one
model in memory, so [llama-swap](https://github.com/mostlygeek/llama-swap) — a
tiny OpenAI-compatible proxy — fronts the whole lineup on one port and starts
the right `llama-server` on demand from the requested model name (load-on-demand,
like Ollama, with llama.cpp performance). Write one entry per model in
`llama-swap.yaml`:

```yaml
models:
  "gemma4:31b":              { cmd: llama-server -m /models/gemma4-31b.gguf --port ${PORT} -c 8192 -ngl 99 --jinja }
  "qwen3.6:35b-a3b":         { cmd: llama-server -m /models/qwen3.6-35b-a3b.gguf --port ${PORT} -c 8192 -ngl 99 --jinja }
  "qwen3-coder-next:latest": { cmd: llama-server -m /models/qwen3-coder-next.gguf --port ${PORT} -c 8192 -ngl 99 --jinja }
  "qwen3.5:9b-mtp":          { cmd: llama-server -m /models/qwen3.5-9b-mtp.gguf --port ${PORT} -c 8192 -ngl 99 --jinja }
  "qwen3-embedding:8b":      { cmd: llama-server -m /models/qwen3-embedding-8b.gguf --port ${PORT} -c 8192 -ngl 99 --embeddings }
```

Run it: `llama-swap --config llama-swap.yaml --listen 127.0.0.1:8080`. The
shipped `models.json` already points a single `llamacpp` `openai-compat` provider
at `http://127.0.0.1:8080`, with every model `name` matching a `llama-swap` key.
See [Model Configuration](#model-configuration).

**llama.cpp without a proxy (pinned servers).** Skip llama-swap and run one
`llama-server` per model on its own port when you want specific models hot at all
times; then declare one `openai-compat` provider per port. The Router's circuit
breakers and fallback chains route around any server that isn't up, so you need
not run the whole fleet at once (and on a 128GB box you can't co-resident it).

**Ollama (alternative).** If you run Ollama instead, pull the lineup:

```bash
ollama pull gemma4:31b            # general / agent / judge / analysis
ollama pull qwen3.6:35b-a3b       # fast fallback / lower-latency chat
ollama pull qwen3-coder-next:latest  # coding / review / FIM
ollama pull qwen3:8b              # lightweight fallback for Ollama-only installs
ollama pull qwen3-embedding:8b    # embeddings (RAG)
```

If you customize `models.json`, serve the models you configured instead.
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

To target a **llama.cpp** backend, declare it as an `openai-compat` provider and
route through `provider.Router` (which dispatches to the right backend client per
the model's `provider`), rather than constructing `ollama.NewClient` directly:

```json
{
  "providers": {
    "llamacpp": { "base_url": "http://127.0.0.1:8080", "timeout": "5m", "api_format": "openai-compat" },
    "ollama":   { "base_url": "http://localhost:11434", "timeout": "5m" }
  },
  "models": {
    "general": { "name": "gemma4:31b", "provider": "llamacpp", "type": "dense" }
  }
}
```

The `provider` key on each model selects its backend (here the `llamacpp`
provider points at the llama-swap proxy on `:8080`); `api_format` defaults to
`ollama` when omitted. See [Local model backends](../README.md#local-model-backends).

Each model can also declare static sampling defaults:

```json
"general": {
  "name": "gemma4:31b",
  "provider": "llamacpp",
  "type": "dense",
  "options": { "temperature": 0, "top_p": 0.9, "top_k": 40 }
}
```

`temperature` must be non-negative, `top_p` must be greater than 0 and at most
1, and `top_k` must be non-negative. Request values fill first, so an explicit
request value—including zero—overrides the configured default. Defaults are
per provider/model, not per role: roles sharing one provider/model key must use
identical options. For workload-specific defaults, use request overrides or
distinct provider keys. `top_k` is supported by llama.cpp and Ollama but is not
part of the strict OpenAI request schema; omit it for hosted endpoints that
reject unknown fields.

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
| `judge` | `gemma4:31b` (~20GB) | Local LLM-as-judge scoring for captured trace replays |
| `fast` | `qwen3.6:35b-a3b` (~28GB) | Lower-latency chat and strong fallback path |
| `coding` | `qwen3-coder-next:latest` (~46GB) | Code generation, review, FIM completions |
| `lightweight` | `qwen3.5:9b-mtp` (~6GB) | Quick answers, classification, low-stakes tasks |
| `embedding` | `qwen3-embedding:8b` (~5GB) | Vector embeddings for RAG search |

The `analysis` use-case currently maps to the `general` role, so it also
resolves to `gemma4:31b` unless you customize the defaults. The `judge`
role is present for `cmd/llm-bench -scorer llm-judge`, but it is not in
`defaults` because consumer apps should not expose it as a normal runtime
use-case.

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

## Running Golem against a hosted API

Golem does not require a local LLM. Any hosted OpenAI-compatible endpoint works:
declare a provider with `api_format: "openai-compat"` and an `api_key`, point the
`agent` role at it, and run `golem` as usual. For the general (non-Golem)
hosted-provider reference — compatibility table, mixing providers, fallbacks —
see [Use a hosted API](../README.md#use-a-hosted-api-bring-your-own-key) in the
README; this section is the Golem-specific path.

A minimal hosted `models.json` (this exact config is load-verified against
`config.Load`):

```json
{
  "providers": {
    "hosted": {
      "base_url": "https://api.openai.com",
      "api_format": "openai-compat",
      "api_key": "${HOSTED_API_KEY}"
    }
  },
  "models": {
    "agent": {
      "name": "gpt-4o",
      "provider": "hosted",
      "type": "dense",
      "context_window": 128000,
      "capabilities": ["chat", "stream", "tool_call"]
    }
  },
  "defaults": {
    "agent": "agent"
  }
}
```

Three things in this config are easy to get wrong:

- `base_url` is the server **root** — do not include `/v1`; the client appends
  the `/v1/...` endpoint paths itself.
- `api_format` must be set explicitly. When omitted it defaults to `ollama`,
  and a hosted endpoint will not speak that protocol.
- `capabilities` must declare `tool_call` explicitly, alongside `chat` and
  `stream`. `tool_call` is never derived from the model `type`; without it the
  agent loop rejects the model.

### Secrets

Set `api_key` to a `${ENV_VAR}` reference and export the variable before
running golem:

```bash
export HOSTED_API_KEY=sk-...
golem -config models.json
```

Expansion happens when the config file loads and fails fast if the variable is
unset or empty — it never falls back to the literal `${...}` string, and the
error names the provider and variable, never the secret value. Never commit a
literal key to a config file. Expansion applies only to file-loaded configs;
a `config.Config` built programmatically keeps `api_key` as written.

### Remote-relevant flags

- `-no-probe` — skips the openai-compat port discovery scan. The scan is
  loopback-only (localhost ports 8080-8090); a remote `base_url` is never
  scanned regardless, so this flag just silences local discovery when you are
  not running a local backend.
- `-no-cap-probe` — disables the active tool-capability probe. For a model
  with no declared or catalog-known `tool_call` capability, golem sends up to
  two real chat-completion requests to the endpoint to test tool support —
  against a hosted API those are billable calls. Verdicts are cached in
  `fingerprints.db` under the golem data directory, so a positive verdict is
  probed once per backend/model rather than per session (negative and
  inconclusive verdicts can expire and re-probe). Declaring `capabilities` explicitly (as in
  the example above) makes the probe unnecessary: an explicit declaration is
  authoritative and no probe is sent.
- `-base-url` — overrides the primary openai-compat agent provider's base URL.
  Server root, without `/v1`, used exactly as given; also disables discovery.
- `golem models -probe-all` / `golem models -reprobe` — subcommand flags for
  explicit capability-probe refresh: `-probe-all` actively probes every
  non-explicit entry, `-reprobe` deletes the cached verdicts first and probes
  fresh. Use these when checking provenance with `golem models`; they are not
  top-level `golem` flags.

### Capabilities the agent loop needs

Golem's agent loop requires `chat`, `stream`, and `tool_call` on the agent
model (and on any fallback it may route to). Explicit `capabilities` REPLACE
the type-derived defaults, which also makes them the carve-down mechanism: the
example's `["chat", "stream", "tool_call"]` omits `generate`, so a hosted
server without `/v1/completions` is fine — the Router never sends it
unsupported requests.

RAG is optional. To index and use `retrieve`, add a hosted embedding model
(`"type": "embedding"`) and set `defaults.embedding` to it. Without one, golem
runs with file tools only.

### Verify the setup

```bash
golem models                 # confirm resolution and tool_call provenance
golem -p "say hi"            # one-shot smoke against the hosted endpoint
```

`golem models` shows which model each role resolves to and where its
`tool_call` verdict came from (`explicit`, `catalog`, `runtime`, or `probe`).
If it looks right, the one-shot run is the end-to-end proof.

Provider `timeout` defaults to `5m` when omitted. Set `"timeout"` on the
provider entry if a hosted model needs longer.

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
| Minimal (8GB) | 8GB | `qwen3.5:9b-mtp` only |
| Standard (16-32GB) | 16-32GB | `qwen3.5:9b-mtp` + `qwen3-embedding:8b` |
| Large (32-64GB) | 32-64GB | One primary model at a time (`gemma4:31b` or `qwen3.6:35b-a3b`), adding smaller models only if memory allows |
| Power (128GB+) | 128GB+ | Full reference lineup from `models.json` |

## Consumers

- **Firn IDE** — custom Wails IDE with code completion and chat
- **Flux ML** — Wails ML development environment
- **Quantum Trader** — Go+Python trading platform with ML analysis
