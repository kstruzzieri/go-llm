package prefetch

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/rag"
)

// ScoredRetriever implements Retriever without caching by delegating to a
// rag.Retriever's scored surface. It always returns CacheHit: false.
//
// When the store implements rag.MultiSignalSearcher, the underlying
// RetrieveScored uses SearchMulti for richer signal-scored results. Otherwise it
// falls back to plain Search wrapped as semantic-only ScoredResults. Delegating
// to rag.Retriever also gives ScoredRetriever vector-space validation for free.
type ScoredRetriever struct {
	retriever *rag.Retriever
}

// NewScoredRetriever creates a ScoredRetriever that embeds queries via the given
// Ollama client and model, then searches the provided vector store.
func NewScoredRetriever(client *ollama.Client, store rag.VectorStore, embedModel string) *ScoredRetriever {
	return &ScoredRetriever{
		retriever: rag.NewRetriever(client, store, rag.WithRetrieverModel(embedModel)),
	}
}

// Retrieve embeds the query, searches the store, and returns scored results.
// The opts.SkipCache flag is ignored since ScoredRetriever has no cache.
func (r *ScoredRetriever) Retrieve(ctx context.Context, query string, k int,
	qCtx rag.QueryContext, opts RetrieveOptions) (*RetrieveResult, error) {

	scored, err := r.retriever.RetrieveScored(ctx, query, k, qCtx)
	if err != nil {
		return nil, fmt.Errorf("prefetch: scored retrieve: %w", err)
	}
	return &RetrieveResult{
		Chunks:   scored,
		CacheHit: false,
	}, nil
}
