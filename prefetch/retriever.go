// Package prefetch provides a predictive cache-warming engine for RAG retrieval.
// It monitors user activity (active file, open files, recent edits) via a
// StateProvider and proactively retrieves contextually relevant chunks into
// an in-memory warm cache. This reduces latency for subsequent user-initiated
// retrieval requests.
package prefetch

import (
	"context"

	"github.com/kstruzzieri/go-llm/rag"
)

// RetrieveOptions controls retrieval behavior.
type RetrieveOptions struct {
	// SkipCache bypasses the warm cache, forcing a cold retrieval.
	SkipCache bool
}

// RetrieveResult holds the outcome of a retrieval request.
type RetrieveResult struct {
	// Chunks contains the scored results ordered by relevance.
	Chunks []rag.ScoredResult
	// CacheHit indicates whether the result was served from the warm cache.
	CacheHit bool
}

// Retriever is the interface for retrieval with optional cache awareness.
// Both ScoredRetriever (no cache) and Engine (with cache) implement it.
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int,
		qCtx rag.QueryContext, opts RetrieveOptions) (*RetrieveResult, error)
}
