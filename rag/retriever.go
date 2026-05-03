package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Retriever queries the vector store and builds augmented prompts.
type Retriever struct {
	embedder Embedder
	model    string
	store    VectorStore
}

// RetrieverOption configures a Retriever.
type RetrieverOption func(*Retriever)

// WithRetrieverModel sets the embedding model for query embedding (default: "nomic-embed-text").
func WithRetrieverModel(model string) RetrieverOption {
	return func(r *Retriever) {
		r.model = model
	}
}

// buildRetriever is the single private path constructing a Retriever; both
// the legacy ollama-backed shim NewRetriever and the new
// NewRetrieverWithEmbedder route through it.
func buildRetriever(emb Embedder, store VectorStore, opts ...RetrieverOption) *Retriever {
	r := &Retriever{
		embedder: emb,
		model:    "nomic-embed-text",
		store:    store,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewRetriever is the *ollama.Client-backed compat shim. Existing consumers
// continue to use this constructor unchanged; new code should prefer
// NewRetrieverWithEmbedder.
//
// Nil-passthrough is preserved: passing a nil client yields a Retriever
// whose embedder is nil and which will nil-deref on first Retrieve call,
// matching the pre-Embedder behaviour.
func NewRetriever(client *ollama.Client, store VectorStore, opts ...RetrieverOption) *Retriever {
	return buildRetriever(embedderFromOllamaClient(client), store, opts...)
}

// NewRetrieverWithEmbedder is the router-aware constructor. See
// NewIndexerWithEmbedder for the design rationale.
//
// Returns an error if embedder is nil.
func NewRetrieverWithEmbedder(embedder Embedder, store VectorStore, opts ...RetrieverOption) (*Retriever, error) {
	if embedder == nil {
		return nil, fmt.Errorf("rag: embedder is required")
	}
	return buildRetriever(embedder, store, opts...), nil
}

// Retrieve finds the top-k most relevant chunks for a query.
func (r *Retriever) Retrieve(ctx context.Context, query string, k int) ([]SearchResult, error) {
	res, err := r.embedder.Embed(ctx, r.model, []string{query})
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}
	if len(res.Embeddings) != 1 {
		return nil, fmt.Errorf("rag: embed query: expected 1 embedding, got %d", len(res.Embeddings))
	}
	embedding := res.Embeddings[0]

	results, err := r.store.Search(ctx, embedding, k)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w", err)
	}

	return results, nil
}

// BuildContext constructs a context string from retrieved chunks,
// formatted for LLM consumption with source attribution.
// maxTokens provides a rough character limit (4 chars per token).
func (r *Retriever) BuildContext(results []SearchResult, maxTokens int) string {
	if len(results) == 0 {
		return ""
	}

	maxChars := maxTokens * 4 // rough approximation
	var b strings.Builder
	b.WriteString("Relevant code context:\n\n")

	for _, res := range results {
		entry := fmt.Sprintf("--- %s (lines %d-%d, similarity: %.2f) ---\n%s\n\n",
			res.Chunk.Source, res.Chunk.StartLine, res.Chunk.EndLine,
			res.Score, res.Chunk.Content)

		if maxChars > 0 && b.Len()+len(entry) > maxChars {
			break
		}
		b.WriteString(entry)
	}

	return b.String()
}
