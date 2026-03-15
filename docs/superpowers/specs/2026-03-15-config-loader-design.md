# Config Loader Design Spec

**Issue:** #12 — feat: config loader for models.json
**Date:** 2026-03-15
**Status:** Reviewed

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
├── resolve.go      # Resolve(), ResolveAll(), ModelChecker interface, ResolvedModel
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
      "fallbacks": ["lightweight"]
    },
    "fast": {
      "name": "qwen3.5:35b-a3b",
      "provider": "ollama",
      "description": "MoE model — fast inference, agent/tool use, multimodal",
      "type": "moe",
      "parameters": "35B total / 3B active",
      "context_window": 256000,
      "fallbacks": ["lightweight"]
    },
    "coding": {
      "name": "qwen3-coder-next:latest",
      "provider": "ollama",
      "description": "Dedicated coding model — code generation, review, completion",
      "type": "dense",
      "parameters": "~32B",
      "context_window": 131072,
      "fallbacks": ["general", "lightweight"]
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
- Each model gains optional `"fallbacks"` array (ordered fallback **role keys**, not model names)

### Fallback Design

Fallbacks reference **role keys** (e.g., `"general"`, `"lightweight"`), not raw model names. This preserves the role-based indirection layer: a fallback model's full config (provider, context window, dimensions) is always accessible through the same `RoleConfig()` path. Circular fallback chains are detected and rejected at `Load` time.

## Core Types

```go
// Duration is a time.Duration that unmarshals from JSON strings like "5m", "30s".
type Duration struct {
    time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error { ... }
func (d Duration) MarshalJSON() ([]byte, error) { ... }

// ProviderConfig holds connection settings for an LLM backend.
type ProviderConfig struct {
    BaseURL   string   `json:"base_url"`
    Timeout   Duration `json:"timeout"`
    APIKey    string   `json:"api_key,omitempty"`
    APIFormat string   `json:"api_format,omitempty"` // "ollama", "openai" — for health-check routing
}

// ModelConfig describes a single model with optional fallback chain.
type ModelConfig struct {
    Name          string   `json:"name"`
    Provider      string   `json:"provider,omitempty"`  // default: "ollama"
    Description   string   `json:"description,omitempty"`
    Type          string   `json:"type,omitempty"`      // "dense", "moe", "embedding"
    Parameters    string   `json:"parameters,omitempty"`
    ContextWindow int      `json:"context_window,omitempty"`
    Dimensions    int      `json:"dimensions,omitempty"` // embedding models only
    Fallbacks     []string `json:"fallbacks,omitempty"`  // ordered fallback role keys
}

// Config is the top-level configuration loaded from models.json.
type Config struct {
    Providers map[string]ProviderConfig `json:"providers"`
    Models    map[string]ModelConfig    `json:"models"`    // keyed by role
    Defaults  map[string]string         `json:"defaults"`  // use-case → role
}
```

## Public API

### config.go — Loading & Lookup

```go
// Load reads and validates a models.json file from the given path.
func Load(path string) (*Config, error)

// MustLoad is like Load but panics on error.
// Use at application startup for fail-fast behavior.
func MustLoad(path string) *Config

// Default discovers and loads models.json from standard locations:
//   1. $GO_LLM_CONFIG (if set)
//   2. ./models.json (working directory)
//   3. ~/.config/go-llm/models.json
// Returns error if not found in any location.
func Default() (*Config, error)

// ModelFor resolves a use-case to a model name through the defaults chain.
// Returns "" if the use-case or its target role is not found.
//   cfg.ModelFor("chat") → defaults["chat"]="general" → models["general"].Name → "qwen3.5:27b"
func (c *Config) ModelFor(useCase string) string

// MustModelFor is like ModelFor but panics if the use-case cannot be resolved.
// Use at startup for programmer-error detection.
func (c *Config) MustModelFor(useCase string) string

// RoleConfig returns the full model configuration for a role.
// Returns nil if the role is not found.
func (c *Config) RoleConfig(role string) *ModelConfig

// Provider returns the provider config for a given provider key.
// Returns nil if the key is not found.
func (c *Config) Provider(key string) *ProviderConfig

// ProviderFor returns the provider config for a given role's model.
// Looks up the model's Provider field (defaults to "ollama") and returns
// the corresponding ProviderConfig. Returns nil if not found.
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
    Role       string // the role that was resolved to (may differ from primary if fallback)
    IsFallback bool   // true if the primary model wasn't available
}

// Resolve checks model availability and walks the fallback chain if needed.
// Returns the first available model for the given use-case.
// Calls checker.AvailableModels once and checks against the result.
func (c *Config) Resolve(ctx context.Context, checker ModelChecker, useCase string) (ResolvedModel, error)

// ResolveAll resolves all default use-cases in a single AvailableModels call.
// Returns a map from use-case to resolved model. Intended for app startup.
// Returns error listing all use-cases that could not be resolved.
func (c *Config) ResolveAll(ctx context.Context, checker ModelChecker) (map[string]ResolvedModel, error)
```

### Resolution Flow

`Resolve("chat")`:
1. Look up `defaults["chat"]` → `"general"`
2. Get `models["general"].Name` → `"qwen3.5:27b"`
3. Call `checker.AvailableModels()`, build `map[string]bool` for O(1) lookups
4. Check if `"qwen3.5:27b"` is available
5. If available → `ResolvedModel{Name: "qwen3.5:27b", Role: "general", IsFallback: false}`
6. If not → walk `models["general"].Fallbacks` (these are role keys), resolve each role's model name, return first available with `IsFallback: true`
7. If none available → return error listing the use-case and all attempted models

`ResolveAll()`:
1. Call `checker.AvailableModels()` once, build availability set
2. Iterate all entries in `Defaults`, resolve each using the same availability set
3. Return map of results; collect errors for any unresolvable use-cases

## Validation

`Load` validates on parse — fail fast with clear messages:

- JSON is well-formed
- At least one provider exists
- Every default references an existing role in models
- Every model has a non-empty Name
- Every model's Provider (if set) references an existing provider key
- Every fallback references an existing role key in models
- No circular fallback chains (detected via visited-set traversal)
- Timeout parses as a valid duration string

Errors follow project convention: `fmt.Errorf("config: <context>: %w", err)`

## Integration with Existing Packages

**No changes to existing constructors.** Config is consumed at the call site by consumers:

```go
// Before (hardcoded):
indexer := rag.NewIndexer(client, store)  // defaults to "nomic-embed-text"
reviewer := analysis.NewCodeReviewer(client, retriever, "qwen2.5:72b")

// After (config-driven):
cfg := config.MustLoad("models.json")
providerCfg := cfg.Provider("ollama")
client := ollama.NewClient(ollama.WithBaseURL(providerCfg.BaseURL))

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

`ollama.Client.ListModels()` returns `[]ollama.ModelInfo`. To satisfy `config.ModelChecker`, we add a convenience method to ollama/:

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

- **Table-driven tests** for `Load`, `ModelFor`, `MustModelFor`, `Provider`, `ProviderFor`
- **Validation tests** — malformed JSON, missing providers, dangling defaults, empty model names, circular fallbacks
- **Resolve tests** — mock `ModelChecker` returning controlled model lists; test primary hit, fallback hit, all-unavailable error
- **ResolveAll tests** — partial availability, all available, none available
- **Default() tests** — test discovery order with temp directories and `GO_LLM_CONFIG` env var
- **Duration tests** — valid strings ("5m", "30s"), invalid strings, zero values
- **Test fixtures** in `config/testdata/` — valid and invalid models.json variants

## File Discovery for Default()

1. `$GO_LLM_CONFIG` (environment variable override, if set)
2. `./models.json` (working directory)
3. `~/.config/go-llm/models.json`
4. Return `config: models.json not found (checked $GO_LLM_CONFIG, ./models.json, ~/.config/go-llm/models.json)`

## Future Considerations (Not In Scope)

- Hot reload / file watching — unnecessary for desktop apps that restart
- Auto-pull missing models — Ollama's responsibility
- OpenAI-compatible `ModelChecker` implementation — add when a consumer needs it
- Provider-specific client constructors — consumers wire this themselves
- Environment variable interpolation for API keys — add when cloud providers are needed
