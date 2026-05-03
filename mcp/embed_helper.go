package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
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
			return nil, routedEmbedError{category: embedToolConfig, err: err}
		}
		rr.PreferredChain = chain
	}
	plan, err := router.Route(ctx, rr)
	if err != nil {
		return nil, routedEmbedError{category: embedToolRouter, err: err}
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

// ragEmbedder returns a rag.Embedder that routes through provider.Router via
// s.routedEmbed at the supplied priority. Indexing should pass
// provider.PriorityBackground; query retrieval should pass
// provider.PriorityNormal so latency-sensitive user-facing paths take
// precedence under contention.
//
// The closure refuses empty model strings to prevent vector-space drift:
// RAG indexing and query embedding must always use the configured embedding
// model, not chain fallback. If a real use case for empty-model + RAG
// emerges, replace the inline error with a typed sentinel so callers can
// errors.Is and consciously override.
func (s *Server) ragEmbedder(priority provider.Priority) rag.Embedder {
	return rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		if model == "" {
			return rag.EmbedResult{}, fmt.Errorf("rag: embedder requires explicit model to prevent vector-space drift across embedding-model boundaries")
		}
		if len(inputs) == 0 {
			// Short-circuit before routing. Provider/VectorSpaceID are
			// undetermined without a routing decision; preserve the requested
			// model so callers see the model they asked for.
			return rag.EmbedResult{Model: model}, nil
		}
		resp, err := s.routedEmbed(ctx, model, inputs, priority)
		if err != nil {
			return rag.EmbedResult{}, err
		}
		result := rag.EmbedResult{
			Embeddings: resp.Embeddings,
			Model:      resp.Model,
			Provider:   resp.Provider,
		}
		if resp.RouteOutcome != nil {
			am := resp.RouteOutcome.ActualModel
			if am.Provider != "" && am.Model != "" {
				result.VectorSpaceID = am.String()
			}
		}
		if result.VectorSpaceID == "" && result.Provider != "" && result.Model != "" {
			result.VectorSpaceID = result.Provider + "/" + result.Model
		}
		return result, nil
	})
}
