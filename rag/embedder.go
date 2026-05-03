// Package rag — Embedder seam.
//
// The Embedder interface is rag's narrow embedding dependency: batch-shaped,
// returning generic provenance instead of importing provider response types
// into rag. Router-backed implementations are supplied by callers (see
// mcp.Server.ragEmbedder); the legacy *ollama.Client path is preserved via
// the private embedderFromOllamaClient adapter used by NewIndexer / NewRetriever.
package rag

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
)

// EmbedResult is the embedding payload plus enough provenance for RAG to
// validate vector-space identity. VectorSpaceID is implementation-defined
// but should be stable for all embeddings that can be compared safely; for
// the Ollama path it is "ollama/<model>".
type EmbedResult struct {
	Embeddings    [][]float64
	Model         string
	Provider      string
	VectorSpaceID string
}

// Embedder is the narrow embedding dependency rag needs. Batch-shaped, like
// provider.Provider.Embed, but returning generic provenance instead of
// importing provider response types into rag.
//
// An empty model string is implementation-defined. The *ollama.Client compat
// shim will fail because Ollama requires a model. Router-backed
// implementations may invoke chain fallback; RAG callers MUST pass a
// non-empty model unless they intentionally own vector-space drift policy.
//
// Implementations must be safe for concurrent use; Indexer.IndexDirectory
// runs files in parallel and may issue Embed calls from multiple goroutines.
//
// Empty inputs (nil or zero-length slice) MUST short-circuit and return
// (EmbedResult{}, nil) without invoking the backend.
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) (EmbedResult, error)
}

// EmbedderFunc is a closure adapter mirroring http.HandlerFunc. Lets MCP
// (and any other consumer) supply an Embedder from a closure without
// declaring a named type.
type EmbedderFunc func(ctx context.Context, model string, inputs []string) (EmbedResult, error)

// Embed calls f(ctx, model, inputs).
func (f EmbedderFunc) Embed(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
	return f(ctx, model, inputs)
}

// embedderFromOllamaClient adapts *ollama.Client to Embedder. Private —
// used only by the legacy NewIndexer / NewRetriever compat shims. External
// consumers wiring router-aware embedding switch to NewIndexerWithEmbedder
// and supply their own Embedder.
//
// Returns nil when client is nil so the legacy nil-passthrough behaviour of
// NewIndexer / NewRetriever is preserved (a nil-deref on first embed call,
// not at construction time).
func embedderFromOllamaClient(c *ollama.Client) Embedder {
	if c == nil {
		return nil
	}
	return EmbedderFunc(func(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
		if len(inputs) == 0 {
			return EmbedResult{}, nil
		}
		embeddings, err := c.EmbedBatch(ctx, model, inputs)
		if err != nil {
			return EmbedResult{}, fmt.Errorf("rag: ollama embedder: %w", err)
		}
		return EmbedResult{
			Embeddings:    embeddings,
			Model:         model,
			Provider:      "ollama",
			VectorSpaceID: "ollama/" + model,
		}, nil
	})
}
