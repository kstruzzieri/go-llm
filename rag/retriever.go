package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Retriever queries the vector store and builds augmented prompts.
type Retriever struct {
	client *ollama.Client
	model  string
	store  VectorStore
}

// RetrieverOption configures a Retriever.
type RetrieverOption func(*Retriever)

// WithRetrieverModel sets the embedding model for query embedding (default: "nomic-embed-text").
func WithRetrieverModel(model string) RetrieverOption {
	return func(r *Retriever) {
		r.model = model
	}
}

// NewRetriever creates a retriever that queries the vector store.
func NewRetriever(client *ollama.Client, store VectorStore, opts ...RetrieverOption) *Retriever {
	r := &Retriever{
		client: client,
		model:  "nomic-embed-text",
		store:  store,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Retrieve finds the top-k most relevant chunks for a query.
func (r *Retriever) Retrieve(ctx context.Context, query string, k int) ([]SearchResult, error) {
	embedding, err := r.client.Embed(ctx, r.model, query)
	if err != nil {
		return nil, fmt.Errorf("rag: embed query: %w", err)
	}

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
