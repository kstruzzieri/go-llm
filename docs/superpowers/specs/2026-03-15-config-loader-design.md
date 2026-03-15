# Config Loader Design Spec

**Issue:** #12 — feat: config loader for models.json
**Date:** 2026-03-15
**Status:** Draft

## Problem

Model names are hardcoded throughout go-llm (`"nomic-embed-text"` in rag/, model strings passed as constructor args in analysis/ and completion/). This makes model changes require code edits and recompilation. Consumers (Firn IDE, Flux ML, Quantum Trader) all need a centralized way to configure which models are used for which tasks.

## Solution

A new `config/` package that loads `models.json`, provides model name resolution by role/use-case, and supports runtime availability checking with fallback chains.

## Design Principles

- **Leaf package** — `config/` has zero imports of internal packages (`ollama`, `rag`, etc.)
- **Pure data** — config holds parsed data and lookup logic, never constructs clients
- **Provider-agnostic** — schema supports multiple LLM backends (Ollama, llama.cpp, vLLM) via a `providers` map
- **Backward compatible** — existing package APIs unchanged; config is consumed at the call site

## Package Structure

```
config/
├── config.go       # Config struct, Load(), Default(), ModelFor(), MustModelFor()
├── resolve.go      # Resolve(), ModelChecker interface, ResolvedModel
└── config_test.go  # Table-driven tests
```

## models.json Schema

```json
{
  "providers": {
    "ollama": {
      "base_url": "http://localhost:11434",
      "timeout": "5m"
    }
  },
  "models": {
    "general": {
      "name": "qwen3.5:27b",
      "provider": "ollama",
      "description": "General purpose — reasoning, conversation, multimodal (vision)",
      "type": "dense",
      "parameters": "27B",
      "context_window": 256000,
      "fallbacks": ["qwen3:8b"]
    },
    "fast": {
      "name": "qwen3.5:35b-a3b",
      "provider": "ollama",
      "description": "MoE model — fast inference, agent/tool use, multimodal",
      "type": "moe",
      "parameters": "35B total / 3B active",
      "context_window": 256000,
      "fallbacks": ["qwen3:8b"]
    },
    "coding": {
      "name": "qwen3-coder-next:latest",
      "provider": "ollama",
      "description": "Dedicated coding model — code generation, review, completion",
      "type": "dense",
      "parameters": "~32B",
      "context_window": 131072,
      "fallbacks": ["qwen3.5:27b", "qwen3:8b"]
    },
    "lightweight": {
      "name": "qwen3:8b",
      "provider": "ollama",
      "description": "Small and fast — simple tasks, low resource usage",
      "type": "dense",
      "parameters": "8B",
      "context_window": 32768
    },
    "embedding": {
      "name": "qwen3-embedding:8b",
      "provider": "ollama",
      "description": "Embedding model for RAG vector search",
      "type": "embedding",
      "parameters": "8B",
      "dimensions": 4096
    }
  },
  "defaults": {
    "chat": "general",
    "completion": "coding",
    "embedding": "embedding",
    "agent": "fast",
    "analysis": "general"
  }
}
```

### Schema Changes from Current models.json

- `"ollama"` key → `"providers": { "ollama": { ... } }` (supports multiple backends)
- Each model gains optional `"provider"` field (defaults to `"ollama"`)
- Each model gains optional `"fallbacks"` array (ordered fallback model names)

## Core Types

```go
// ProviderConfig holds connection settings for an LLM backend.
type ProviderConfig struct {
    BaseURL   string        // e.g. "http://localhost:11434"
    Timeout   time.Duration // request timeout
    APIKey    string        // unused for Ollama, ready for API providers
    APIFormat string        // "ollama", "openai" — for health-check routing
}

// ModelConfig describes a single model with optional fallback chain.
type ModelConfig struct {
    Name          string
    Provider      string   // which provider key serves this model (default: "ollama")
    Description   string
    Type          string   // "dense", "moe", "embedding"
    Parameters    string
    ContextWindow int
    Dimensions    int      // embedding models only
    Fallbacks     []string // ordered fallback model names
}

// Config is the top-level configuration loaded from models.json.
type Config struct {
    Providers map[string]ProviderConfig // "ollama", "llama_cpp", etc.
    Models    map[string]ModelConfig    // keyed by role: "general", "coding"
    Defaults  map[string]string         // use-case → role: "chat" → "general"
}
```

## Public API

### config.go — Loading & Lookup

```go
// Load reads and validates a models.json file from the given path.
func Load(path string) (*Config, error)

// Default discovers and loads models.json from standard locations:
//   1. ./models.json (working directory)
//   2. ~/.config/go-llm/models.json
// Returns error if not found in any location.
func Default() (*Config, error)

// ModelFor resolves a use-case to a model name through the defaults chain.
// Returns "" if the use-case or its target role is not found.
//   cfg.ModelFor("chat") → defaults["chat"]="general" → models["general"].Name → "qwen3.5:27b"
func (c *Config) ModelFor(useCase string) string

// MustModelFor is like ModelFor but panics if the use-case cannot be resolved.
// Use at startup for programmer-error detection.
func (c *Config) MustModelFor(useCase string) string

// ModelConfig returns the full model configuration for a role.
// Returns nil if the role is not found.
func (c *Config) RoleConfig(role string) *ModelConfig

// ProviderFor returns the provider config for a given role's model.
// Falls back to the "ollama" provider if the model has no explicit provider.
// Returns nil if the provider is not found.
func (c *Config) ProviderFor(role string) *ProviderConfig
```

### resolve.go — Runtime Availability & Fallbacks

```go
// ModelChecker abstracts checking model availability against a backend.
type ModelChecker interface {
    AvailableModels(ctx context.Context) ([]string, error)
}

// ResolvedModel is the result of resolving a role to an available model.
type ResolvedModel struct {
    Name       string // the model that was selected
    Role       string // the role that was requested
    IsFallback bool   // true if the primary model wasn't available
}

// Resolve checks model availability and walks the fallback chain if needed.
// Returns the first available model for the given use-case.
func (c *Config) Resolve(ctx context.Context, checker ModelChecker, useCase string) (ResolvedModel, error)
```

### Resolution Flow

`Resolve("chat")`:
1. Look up `defaults["chat"]` → `"general"`
2. Get `models["general"].Name` → `"qwen3.5:27b"`
3. Check if `"qwen3.5:27b"` is in `checker.AvailableModels()`
4. If available → `ResolvedModel{Name: "qwen3.5:27b", Role: "general", IsFallback: false}`
5. If not → walk `models["general"].Fallbacks`, return first available with `IsFallback: true`
6. If none available → return error

## Validation

`Load` validates on parse — fail fast with clear messages:

- JSON is well-formed
- At least one provider exists
- Every default references an existing role in models
- Every model has a non-empty Name
- Every model's Provider (if set) references an existing provider key
- Fallback names are non-empty strings (existence checked at Resolve time, not Load time)
- Timeout parses as a valid duration

Errors follow project convention: `fmt.Errorf("config: <context>: %w", err)`

## Integration with Existing Packages

**No changes to existing constructors.** Config is consumed at the call site by consumers:

```go
// Before (hardcoded):
indexer := rag.NewIndexer(client, store)  // defaults to "nomic-embed-text"
reviewer := analysis.NewCodeReviewer(client, retriever, "qwen2.5:72b")

// After (config-driven):
cfg := config.MustLoad("models.json")
client := ollama.NewClient(cfg.ProviderFor("ollama").BaseURL)

indexer := rag.NewIndexer(client, store, rag.WithEmbeddingModel(cfg.MustModelFor("embedding")))
reviewer := analysis.NewCodeReviewer(client, retriever, cfg.MustModelFor("analysis"))
completer := completion.NewProvider(client, cfg.MustModelFor("completion"))
```

### What This Means

- Zero changes to `rag/`, `completion/`, `analysis/` packages
- Config is purely additive — no breaking changes anywhere
- Hardcoded defaults in `NewIndexer`/`NewRetriever` remain as safe fallbacks
- Consumers (Firn, Flux) get one `config.Load` call replacing all hardcoded strings

### ModelChecker Adapter

`ollama.Client.ListModels()` returns `[]ollama.ModelInfo`. To satisfy `config.ModelChecker`, consumers write a thin adapter or we add a convenience method to ollama/:

```go
// In ollama/client.go — satisfies config.ModelChecker
func (c *Client) AvailableModels(ctx context.Context) ([]string, error) {
    models, err := c.ListModels(ctx)
    if err != nil {
        return nil, err
    }
    names := make([]string, len(models))
    for i, m := range models {
        names[i] = m.Name
    }
    return names, nil
}
```

This is the one touch point outside `config/` — a single method added to `ollama.Client`.

## Testing Strategy

- **Table-driven tests** for `Load`, `ModelFor`, `MustModelFor`, `ProviderFor`
- **Validation tests** — malformed JSON, missing providers, dangling defaults, empty model names
- **Resolve tests** — mock `ModelChecker` returning controlled model lists; test primary hit, fallback hit, all-unavailable error
- **Default() tests** — test discovery order with temp directories
- **Test fixtures** in `config/testdata/` — valid and invalid models.json variants

## File Discovery for Default()

1. `./models.json` (working directory)
2. `~/.config/go-llm/models.json`
3. Return `config: models.json not found in ./models.json or ~/.config/go-llm/models.json`

## Future Considerations (Not In Scope)

- Hot reload / file watching — unnecessary for desktop apps that restart
- Auto-pull missing models — Ollama's responsibility
- OpenAI-compatible `ModelChecker` implementation — add when a consumer needs it
- Provider-specific client constructors — consumers wire this themselves
