package analysis

import (
	"context"

	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// ChatFunc is the chat-shaped dependency analysis tools need. The useCase
// string flows to the Router (in router-aware callers) for per-tool weight
// profile selection (e.g. "code-review", "analysis"). For *ollama.Client-
// backed compat shims, useCase is ignored.
type ChatFunc func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error)

// ContextRetriever is the narrow retriever dependency code review needs.
// Satisfied directly by *rag.Retriever — no adapter required.
type ContextRetriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error)
	BuildContext(results []rag.SearchResult, maxTokens int) string
}
