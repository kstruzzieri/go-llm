package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

// embedToolCategory enumerates the error-category strings used by the embed
// handlers. Centralised so callers can reason about categorisation without
// string matching.
type embedToolCategory string

const (
	embedToolConfig embedToolCategory = "config"
	embedToolRouter embedToolCategory = "router"
	embedToolOllama embedToolCategory = "ollama"
)

// routedEmbedError wraps a routing error with a category. Callers extract
// the category via routedEmbedCategory; unwrapping yields the underlying
// error for normal error inspection.
type routedEmbedError struct {
	category embedToolCategory
	err      error
}

func (e routedEmbedError) Error() string { return e.err.Error() }
func (e routedEmbedError) Unwrap() error { return e.err }

// routedEmbedCategory extracts the embedToolCategory from any error returned
// by routedEmbed. Errors not wrapping routedEmbedError are categorised as
// "ollama" (the most conservative — assume backend execution failed).
func routedEmbedCategory(err error) embedToolCategory {
	var re routedEmbedError
	if errors.As(err, &re) {
		return re.category
	}
	return embedToolOllama
}

// routedEmbed is the single source of truth for embed routing in MCP.
// All callers — handleEmbed, handleEmbedBatch, and s.ragEmbedder(priority) —
// funnel through it. Returns the full *provider.EmbedResponse so callers can
// preserve route metadata (e.g., RouteOutcome.ActualModel for vector-space
// provenance).
//
// Empty inputs short-circuit and return an empty EmbedResponse without
// invoking the router.
//
// model="" enables config-fallback-chain selection via s.chainFor("embedding").
// RAG callers MUST NOT pass an empty model — see s.ragEmbedder for the
// drift-prevention guard that enforces this for RAG indexing/retrieval.
func (s *Server) routedEmbed(ctx context.Context, model string, inputs []string, priority provider.Priority) (*provider.EmbedResponse, error) {
	if len(inputs) == 0 {
		return &provider.EmbedResponse{}, nil
	}
	router := s.routerSnapshot()
	if router == nil {
		return nil, routedEmbedError{category: embedToolConfig, err: fmt.Errorf("router unavailable")}
	}
	rr := provider.RoutingRequest{
		Model:          model,
		UseCase:        "embedding",
		RequiredCaps:   provider.CapEmbed,
		Input:          inputs,
		ExpectedOutput: provider.DefaultExpectedOutput("embedding"),
		Priority:       priority,
	}
	if rr.Model == "" {
		chain, err := s.chainFor("embedding")
		if err != nil {
			return nil, routedEmbedError{category: embedToolConfig, err: fmt.Errorf("chain: %w", err)}
		}
		rr.PreferredChain = chain
	}
	plan, err := router.Route(ctx, rr)
	if err != nil {
		return nil, routedEmbedError{category: embedToolRouter, err: fmt.Errorf("route: %w", err)}
	}
	resp, err := plan.ExecuteEmbed(ctx)
	if err != nil {
		return nil, routedEmbedError{category: embedToolOllama, err: err}
	}
	if len(resp.Embeddings) != len(inputs) {
		return nil, routedEmbedError{
			category: embedToolOllama,
			err:      fmt.Errorf("embedding count mismatch: got %d for %d inputs", len(resp.Embeddings), len(inputs)),
		}
	}
	return resp, nil
}
