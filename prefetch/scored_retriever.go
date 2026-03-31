package prefetch

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/rag"
)

// ScoredRetriever implements Retriever without caching by delegating directly
// to an Ollama embedding client and a RAG vector store. It always returns
// CacheHit: false.
//
// When the store implements rag.MultiSignalSearcher, ScoredRetriever uses
// SearchMulti for richer signal-scored results. Otherwise, it falls back to
// plain Search and wraps each result as a ScoredResult with a semantic-only
// signal.
type ScoredRetriever struct {
	client     *ollama.Client
	store      rag.VectorStore
	embedModel string
}

// NewScoredRetriever creates a ScoredRetriever that embeds queries via the
// given Ollama client and model, then searches the provided vector store.
func NewScoredRetriever(client *ollama.Client, store rag.VectorStore, embedModel string) *ScoredRetriever {
	return &ScoredRetriever{
		client:     client,
		store:      store,
		embedModel: embedModel,
	}
}

// Retrieve embeds the query, searches the store, and returns scored results.
// The opts.SkipCache flag is ignored since ScoredRetriever has no cache.
func (r *ScoredRetriever) Retrieve(ctx context.Context, query string, k int,
	qCtx rag.QueryContext, opts RetrieveOptions) (*RetrieveResult, error) {

	embedding, err := r.client.Embed(ctx, r.embedModel, query)
	if err != nil {
		return nil, fmt.Errorf("prefetch: embed query: %w", err)
	}

	// Prefer multi-signal search when available.
	if ms, ok := r.store.(rag.MultiSignalSearcher); ok {
		results, err := ms.SearchMulti(ctx, embedding, query, k, qCtx)
		if err != nil {
			return nil, fmt.Errorf("prefetch: multi-signal search: %w", err)
		}
		return &RetrieveResult{
			Chunks:   results,
			CacheHit: false,
		}, nil
	}

	// Fall back to plain vector search.
	results, err := r.store.Search(ctx, embedding, k)
	if err != nil {
		return nil, fmt.Errorf("prefetch: search: %w", err)
	}

	scored := make([]rag.ScoredResult, len(results))
	for i, sr := range results {
		scored[i] = rag.ScoredResult{
			SearchResult: sr,
			Signals:      map[string]float64{"semantic": sr.Score},
		}
	}

	return &RetrieveResult{
		Chunks:   scored,
		CacheHit: false,
	}, nil
}
