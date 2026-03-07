# Shared Go LLM Module — Design Document

**Date:** 2026-02-25
**Consumers:** Firn IDE, Flux ML, (future: Quantum Trader Go API)

---

## Overview

A shared Go module providing Ollama integration (chat, completions, embeddings) and a lightweight RAG layer with SQLite-backed vector storage. Both Firn IDE and Flux ML are Wails apps that already use SQLite (Flux directly, Firn can add it), making this a natural shared dependency.

## Repository Structure

```
projects/
├── go-llm/                              # NEW: shared module
│   ├── go.mod                           # module github.com/kstruzzieri/go-llm
│   ├── go.sum
│   ├── README.md
│   │
│   ├── ollama/                          # Ollama API client
│   │   ├── client.go                    # HTTP client, connection management
│   │   ├── client_test.go
│   │   ├── types.go                     # Request/response types
│   │   ├── chat.go                      # Chat completions (streaming + non-streaming)
│   │   ├── chat_test.go
│   │   ├── generate.go                  # Raw text generation
│   │   ├── generate_test.go
│   │   ├── embed.go                     # Embedding generation
│   │   ├── embed_test.go
│   │   ├── models.go                    # Model listing, info, pull, delete
│   │   └── models_test.go
│   │
│   ├── rag/                             # RAG (Retrieval Augmented Generation)
│   │   ├── chunker.go                   # Text chunking strategies
│   │   ├── chunker_test.go
│   │   ├── chunker_code.go             # Code-aware chunking (by function/class)
│   │   ├── store.go                     # VectorStore interface
│   │   ├── store_test.go
│   │   ├── sqlite_store.go             # SQLite + brute-force cosine similarity
│   │   ├── sqlite_store_test.go
│   │   ├── indexer.go                   # Index files → chunks → embeddings → store
│   │   ├── indexer_test.go
│   │   ├── retriever.go                # Query → embed → search → rank → return
│   │   └── retriever_test.go
│   │
│   ├── completion/                      # IDE completion helpers
│   │   ├── inline.go                    # FIM (Fill-in-the-Middle) completion
│   │   ├── inline_test.go
│   │   ├── context.go                   # Context window management
│   │   └── context_test.go
│   │
│   └── analysis/                        # Higher-level analysis helpers
│       ├── code_review.go               # Code review prompt construction
│       ├── explain.go                   # Code explanation
│       ├── metrics.go                   # ML metrics analysis (for Flux)
│       └── trading.go                   # Trading strategy analysis (for QT)
```

## Module Path

```
module github.com/kstruzzieri/go-llm
```

Consumers reference it via local replace directives during development:

```go
// firn-ide/go.mod
require github.com/kstruzzieri/go-llm v0.0.0
replace github.com/kstruzzieri/go-llm => ../../go-llm

// flux-ml/go.mod
require github.com/kstruzzieri/go-llm v0.0.0
replace github.com/kstruzzieri/go-llm => ../../go-llm
```

When stable, push to GitHub and use versioned imports.

---

## Package Details

### `ollama/` — Ollama API Client

The core HTTP client. All other packages depend on this.

```go
package ollama

// Client talks to the Ollama REST API at localhost:11434.
type Client struct {
    baseURL    string
    httpClient *http.Client
}

// NewClient creates a client with sensible defaults.
func NewClient(opts ...Option) *Client

// Options
type Option func(*Client)
func WithBaseURL(url string) Option       // default: http://localhost:11434
func WithTimeout(d time.Duration) Option  // default: 5 min (generation can be slow)
func WithHTTPClient(c *http.Client) Option

// --- Chat ---

type ChatMessage struct {
    Role    string `json:"role"`    // system, user, assistant
    Content string `json:"content"`
}

type ChatRequest struct {
    Model    string        `json:"model"`
    Messages []ChatMessage `json:"messages"`
    Stream   bool          `json:"stream"`
    Options  *ModelOptions `json:"options,omitempty"`
}

type ModelOptions struct {
    Temperature   float64 `json:"temperature,omitempty"`
    TopP          float64 `json:"top_p,omitempty"`
    NumPredict    int     `json:"num_predict,omitempty"`    // max tokens
    NumCtx        int     `json:"num_ctx,omitempty"`        // context window
    Stop          []string `json:"stop,omitempty"`
    RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
}

type ChatResponse struct {
    Model     string      `json:"model"`
    Message   ChatMessage `json:"message"`
    Done      bool        `json:"done"`
    TotalDuration  int64  `json:"total_duration"`   // nanoseconds
    EvalCount      int    `json:"eval_count"`       // tokens generated
    EvalDuration   int64  `json:"eval_duration"`    // nanoseconds for generation
}

// Chat sends a chat completion request.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

// ChatStream sends a streaming chat request, calling fn for each token.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error

// --- Embeddings ---

type EmbedRequest struct {
    Model string `json:"model"`
    Input string `json:"input"`
}

type EmbedResponse struct {
    Embeddings [][]float64 `json:"embeddings"`
}

// Embed generates embeddings for the given text.
func (c *Client) Embed(ctx context.Context, model string, text string) ([]float64, error)

// EmbedBatch generates embeddings for multiple texts.
func (c *Client) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float64, error)

// --- Models ---

type ModelInfo struct {
    Name       string `json:"name"`
    Size       int64  `json:"size"`
    ParamSize  string `json:"parameter_size"`
    QuantLevel string `json:"quantization_level"`
}

// ListModels returns all available models.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error)

// IsAvailable checks if Ollama is running and responsive.
func (c *Client) IsAvailable(ctx context.Context) bool
```

**Key design decisions:**
- Streaming is first-class — both Firn IDE (inline completion) and Flux (live analysis) need it
- Context cancellation everywhere — user switches files mid-completion, cancel immediately
- No global state — multiple clients can coexist (different timeout configs for completion vs analysis)

### `rag/` — Retrieval Augmented Generation

```go
package rag

// --- Chunking ---

type Chunk struct {
    ID       string            // deterministic hash of content + source
    Content  string            // the text
    Source   string            // file path
    StartLine int              // line number in source
    EndLine   int
    Language string            // "go", "python", "typescript", etc.
    Metadata map[string]string // arbitrary k/v (function name, class, etc.)
}

// Chunker splits text into chunks suitable for embedding.
type Chunker interface {
    Chunk(source string, content string) ([]Chunk, error)
}

// NewCodeChunker returns a chunker that respects code boundaries.
// It splits on function/method/class boundaries rather than arbitrary line counts.
// Falls back to sliding window for non-code files.
func NewCodeChunker(opts ...ChunkerOption) Chunker

type ChunkerOption func(*codeChunker)
func WithMaxChunkSize(n int) ChunkerOption   // default: 1500 chars (~375 tokens)
func WithOverlap(n int) ChunkerOption        // default: 200 chars
func WithLanguage(lang string) ChunkerOption // auto-detect if empty

// --- Vector Store ---

// VectorStore persists and searches embeddings.
type VectorStore interface {
    // Store saves chunks with their embeddings.
    Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error

    // Search finds the top-k most similar chunks to the query embedding.
    Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error)

    // Delete removes chunks by source path (for re-indexing).
    DeleteBySource(ctx context.Context, source string) error

    // Stats returns index statistics.
    Stats(ctx context.Context) (StoreStats, error)
}

type SearchResult struct {
    Chunk      Chunk
    Score      float64 // cosine similarity, 0-1
    Distance   float64 // 1 - score
}

type StoreStats struct {
    TotalChunks    int
    TotalSources   int
    EmbeddingDim   int
    StorageBytes   int64
}

// NewSQLiteStore creates a vector store backed by SQLite.
// Uses the sqlite-vec extension if available, falls back to brute-force cosine.
//
// Why SQLite:
// - Both Firn IDE and Flux ML already use or can trivially add SQLite
// - Zero server dependency (unlike ChromaDB, Qdrant)
// - Embedded, single-file, cross-platform
// - For codebases < 100k chunks, brute-force cosine is fast enough (~50ms)
// - sqlite-vec extension adds ANN indexing if needed later
func NewSQLiteStore(dbPath string) (VectorStore, error)

// --- Indexer ---

// Indexer coordinates chunking, embedding, and storing.
type Indexer struct {
    client  *ollama.Client
    model   string          // embedding model name
    store   VectorStore
    chunker Chunker
}

func NewIndexer(client *ollama.Client, store VectorStore, opts ...IndexerOption) *Indexer

type IndexerOption func(*Indexer)
func WithEmbeddingModel(model string) IndexerOption  // default: "nomic-embed-text"
func WithChunker(c Chunker) IndexerOption

// IndexFile indexes a single file.
func (idx *Indexer) IndexFile(ctx context.Context, path string) error

// IndexDirectory indexes all supported files in a directory tree.
// Respects .gitignore patterns.
func (idx *Indexer) IndexDirectory(ctx context.Context, dir string, opts ...IndexDirOption) error

type IndexDirOption func(*indexDirConfig)
func WithExtensions(exts ...string) IndexDirOption  // default: .go, .py, .ts, .tsx, .js, .md
func WithExclude(patterns ...string) IndexDirOption // default: node_modules, .git, dist, vendor
func WithConcurrency(n int) IndexDirOption          // default: 4

// IndexStatus returns progress for a running index operation.
type IndexStatus struct {
    TotalFiles   int
    IndexedFiles int
    SkippedFiles int
    Errors       []string
    InProgress   bool
}

// --- Retriever ---

// Retriever queries the vector store and builds augmented prompts.
type Retriever struct {
    client *ollama.Client
    model  string
    store  VectorStore
}

func NewRetriever(client *ollama.Client, store VectorStore, opts ...RetrieverOption) *Retriever

// Retrieve finds the top-k most relevant chunks for a query.
func (r *Retriever) Retrieve(ctx context.Context, query string, k int) ([]SearchResult, error)

// BuildContext constructs a context string from retrieved chunks,
// formatted for LLM consumption with source attribution.
func (r *Retriever) BuildContext(results []SearchResult, maxTokens int) string
```

### `completion/` — IDE Completion Helpers

Specifically for Firn IDE's inline code completion:

```go
package completion

// FIMRequest represents a Fill-in-the-Middle completion request.
type FIMRequest struct {
    Prefix     string // code before cursor
    Suffix     string // code after cursor
    FilePath   string // for language detection
    MaxTokens  int    // default: 128
    Language   string // auto-detect from FilePath if empty
}

type FIMResponse struct {
    Completion string
    Tokens     int
    LatencyMs  int64
}

// Provider generates inline completions using Ollama.
type Provider struct {
    client *ollama.Client
    model  string
}

func NewProvider(client *ollama.Client, model string) *Provider

// Complete generates an inline completion.
// Automatically constructs the FIM prompt format for the model.
func (p *Provider) Complete(ctx context.Context, req FIMRequest) (*FIMResponse, error)

// CompleteStream generates a streaming inline completion.
func (p *Provider) CompleteStream(ctx context.Context, req FIMRequest, fn func(token string) error) error
```

### `analysis/` — Domain-Specific Analysis

Higher-level helpers built on chat + RAG:

```go
package analysis

// CodeReviewer generates code reviews using LLM + RAG context.
type CodeReviewer struct {
    client    *ollama.Client
    retriever *rag.Retriever
    model     string
}

// Review generates a code review for the given diff or file content.
func (cr *CodeReviewer) Review(ctx context.Context, code string, opts ...ReviewOption) (string, error)

// MetricsAnalyzer generates natural-language analysis of ML metrics.
// Used by Flux ML.
type MetricsAnalyzer struct {
    client *ollama.Client
    model  string
}

// AnalyzeTraining generates a summary of training progress.
func (ma *MetricsAnalyzer) AnalyzeTraining(ctx context.Context, metrics TrainingMetrics) (string, error)

// ExplainAnomaly explains a detected anomaly in plain English.
func (ma *MetricsAnalyzer) ExplainAnomaly(ctx context.Context, anomaly AnomalyInfo) (string, error)

// TrainingMetrics represents a snapshot of training state.
type TrainingMetrics struct {
    Epoch           int
    Loss            float64
    LossHistory     []float64
    RewardMean      float64
    RewardHistory   []float64
    KLDivergence    float64
    LearningRate    float64
    CustomMetrics   map[string]float64
}

type AnomalyInfo struct {
    Type        string // "reward_hack", "kl_drift", "loss_spike", etc.
    Severity    string // "warning", "critical"
    Description string
    Metrics     map[string]float64
}
```

---

## Integration Points

### Firn IDE

```go
// firn-ide/internal/llm/service.go
package llm

import (
    "github.com/kstruzzieri/go-llm/ollama"
    "github.com/kstruzzieri/go-llm/rag"
    "github.com/kstruzzieri/go-llm/completion"
)

// Service is the LLM facade exposed to the Wails frontend.
type Service struct {
    client     *ollama.Client
    completer  *completion.Provider
    indexer    *rag.Indexer
    retriever  *rag.Retriever
    store      rag.VectorStore
}

func NewService(dbPath string) (*Service, error) {
    client := ollama.NewClient()
    store, _ := rag.NewSQLiteStore(dbPath)
    indexer := rag.NewIndexer(client, store)
    retriever := rag.NewRetriever(client, store)
    completer := completion.NewProvider(client, "qwen2.5-coder:32b")

    return &Service{
        client:    client,
        completer: completer,
        indexer:   indexer,
        retriever: retriever,
        store:     store,
    }, nil
}

// --- Wails-exposed methods ---

// GetCompletion returns an inline code completion.
// Called from CodeMirror via Wails bridge.
func (s *Service) GetCompletion(prefix, suffix, filePath string) (string, error)

// Chat sends a message with optional RAG context.
// Called from the chat panel.
func (s *Service) Chat(message string, useRAG bool) (string, error)

// ChatStream sends a streaming message. Emits events to frontend.
func (s *Service) ChatStream(ctx context.Context, message string, useRAG bool)

// IndexWorkspace indexes the current workspace for RAG.
func (s *Service) IndexWorkspace(workspacePath string) error

// GetIndexStatus returns current indexing progress.
func (s *Service) GetIndexStatus() rag.IndexStatus

// ListModels returns available Ollama models.
func (s *Service) ListModels() ([]ollama.ModelInfo, error)

// IsOllamaRunning checks if Ollama is available.
func (s *Service) IsOllamaRunning() bool
```

Then in `app.go`:
```go
type App struct {
    ctx         context.Context
    // ... existing fields ...
    llm         *llm.Service   // NEW
}
```

Frontend calls via Wails bindings:
```typescript
// React component
const completion = await window.go.main.App.GetCompletion(prefix, suffix, filePath);
const models = await window.go.main.App.ListModels();
```

### Flux ML

```go
// flux-ml/internal/llm/service.go
package llm

import (
    "github.com/kstruzzieri/go-llm/ollama"
    "github.com/kstruzzieri/go-llm/analysis"
)

type Service struct {
    client   *ollama.Client
    analyzer *analysis.MetricsAnalyzer
}

func NewService() *Service {
    client := ollama.NewClient()
    return &Service{
        client:   client,
        analyzer: analysis.NewMetricsAnalyzer(client, "qwen2.5:72b"),
    }
}

// --- Wails-exposed methods ---

// AnalyzeExperiment generates an LLM analysis of experiment metrics.
func (s *Service) AnalyzeExperiment(experimentID string) (string, error)

// ExplainAlert explains a reward hack alert in plain English.
func (s *Service) ExplainAlert(alertID string) (string, error)

// CompareExperiments generates a comparative analysis.
func (s *Service) CompareExperiments(idA, idB string) (string, error)

// SuggestHyperparameters suggests adjustments based on training curves.
func (s *Service) SuggestHyperparameters(experimentID string) (string, error)
```

---

## Dependencies

Minimal external dependencies:

```go
// go.mod
module github.com/kstruzzieri/go-llm

go 1.23

require (
    modernc.org/sqlite v1.44.3   // SQLite for vector store (pure Go, no CGo)
)
```

That's it. The Ollama client is pure `net/http`. Embeddings math is `math` stdlib. SQLite is already used by Flux ML (same driver: `modernc.org/sqlite`).

---

## Implementation Priority

### Phase 1: Core (Week 1)
1. `ollama/client.go` — HTTP client with streaming
2. `ollama/chat.go` — Chat completions
3. `ollama/embed.go` — Embedding generation
4. `ollama/models.go` — Model listing
5. Tests for all of the above

### Phase 2: RAG (Week 1-2)
1. `rag/chunker.go` + `rag/chunker_code.go`
2. `rag/sqlite_store.go` — Vector store
3. `rag/indexer.go` — File → chunks → embeddings → store
4. `rag/retriever.go` — Query → search → rank
5. Tests

### Phase 3: IDE Integration (Week 2)
1. `completion/inline.go` — FIM completion
2. Firn IDE `internal/llm/service.go` — Wails facade
3. Frontend bindings + CodeMirror integration
4. Chat panel component

### Phase 4: ML Analysis (Week 2-3)
1. `analysis/metrics.go` — Training analysis
2. Flux ML `internal/llm/service.go` — Wails facade
3. Frontend integration in Experiments view

### Phase 5: Polish (Week 3)
1. `analysis/code_review.go`
2. `analysis/trading.go` (for quantum-trader)
3. Documentation
4. Push to GitHub as proper module

---

## Performance Considerations

- **Embedding batch size:** nomic-embed-text handles ~32 texts per batch efficiently. The indexer should batch.
- **Vector search:** For < 50k chunks (typical codebase), brute-force cosine similarity in SQLite is < 50ms. No ANN index needed yet.
- **Completion latency:** FIM completion should target < 500ms for inline suggestions. Use small context windows (2048 tokens) and `num_predict: 128`.
- **Memory:** The go-llm module itself uses negligible memory. Ollama manages model memory independently.
- **Cancellation:** Every method takes `context.Context`. Inline completions get cancelled frequently (every keystroke debounced to ~300ms).

---

## Testing Strategy

- Unit tests with mock HTTP server (no Ollama dependency for CI)
- Integration tests tagged `//go:build integration` that require running Ollama
- SQLite store tests use in-memory databases
- Chunker tests use fixture files in `testdata/`
