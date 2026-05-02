package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/provider"
)

func (s *Server) registerResources() {
	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "go-llm://health",
		Name:        "health",
		Description: "Server health status (Ollama connectivity and RAG store status)",
		MIMEType:    "application/json",
	}, s.handleHealthResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "go-llm://models",
		Name:        "models",
		Description: "List of available Ollama models with metadata",
		MIMEType:    "application/json",
	}, s.handleModelsResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "go-llm://rag/stats",
		Name:        "rag-stats",
		Description: "RAG vector store statistics",
		MIMEType:    "application/json",
	}, s.handleRAGStatsResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "go-llm://config",
		Name:        "config",
		Description: "Current resolved model configuration",
		MIMEType:    "application/json",
	}, s.handleConfigResource)

	s.mcpServer.AddResourceTemplate(&gomcp.ResourceTemplate{
		URITemplate: "go-llm://models/{name}",
		Name:        "model-detail",
		Description: "Details for a specific Ollama model",
		MIMEType:    "application/json",
	}, s.handleModelDetailResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "route://breakers",
		Name:        "Provider circuit breakers",
		Description: "Live circuit breaker state for every provider with a breaker.",
		MIMEType:    "application/json",
	}, s.handleRouteBreakersResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "route://warmth",
		Name:        "Warm models",
		Description: "Models currently loaded in provider memory (warmth snapshot).",
		MIMEType:    "application/json",
	}, s.handleRouteWarmthResource)

	s.mcpServer.AddResource(&gomcp.Resource{
		URI:         "route://sticky",
		Name:        "Sticky routes",
		Description: "Active sticky routing entries (empty for chain-routed requests).",
		MIMEType:    "application/json",
	}, s.handleRouteStickyResource)
}

func resourceResult(uri, mimeType, text string) *gomcp.ReadResourceResult {
	return &gomcp.ReadResourceResult{
		Contents: []*gomcp.ResourceContents{
			{URI: uri, MIMEType: mimeType, Text: text},
		},
	}
}

// marshalResource marshals v to indented JSON for a resource response.
func marshalResource(uri string, v any) (*gomcp.ReadResourceResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal resource %s: %w", uri, err)
	}
	return resourceResult(uri, "application/json", string(data)), nil
}

func (s *Server) handleHealthResource(ctx context.Context, req *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	ollamaOK := s.client.IsAvailable(ctx)

	health := map[string]any{
		"ollama": map[string]any{
			"url":       s.ollamaURL,
			"available": ollamaOK,
		},
		"rag": map[string]any{
			"enabled": !s.ragDisabled,
		},
	}

	if !s.ragDisabled {
		s.mu.RLock()
		store := s.store
		s.mu.RUnlock()

		if store != nil {
			stats, err := store.Stats(ctx)
			if err == nil {
				health["rag"].(map[string]any)["total_chunks"] = stats.TotalChunks
				health["rag"].(map[string]any)["total_sources"] = stats.TotalSources
			}
		}
	}

	return marshalResource("go-llm://health", health)
}

func (s *Server) handleModelsResource(ctx context.Context, req *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	models, err := s.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return marshalResource("go-llm://models", models)
}

func (s *Server) handleRAGStatsResource(ctx context.Context, req *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	if s.ragDisabled {
		return resourceResult("go-llm://rag/stats", "application/json", `{"enabled":false}`), nil
	}

	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()

	if store == nil {
		return resourceResult("go-llm://rag/stats", "application/json", `{"enabled":true,"status":"store unavailable"}`), nil
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return marshalResource("go-llm://rag/stats", stats)
}

func (s *Server) handleConfigResource(_ context.Context, req *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	s.mu.RLock()
	resolved := s.resolved
	s.mu.RUnlock()

	result := map[string]any{
		"has_config": s.cfg != nil,
		"resolved":   resolved,
	}
	return marshalResource("go-llm://config", result)
}

func (s *Server) handleModelDetailResource(ctx context.Context, req *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	name := strings.TrimPrefix(req.Params.URI, "go-llm://models/")
	if name == "" || name == req.Params.URI {
		return nil, gomcp.ResourceNotFoundError(req.Params.URI)
	}

	info, err := s.client.ShowModel(ctx, name)
	if err != nil {
		return nil, err
	}
	return marshalResource(req.Params.URI, info)
}

// breakerEntry pairs a provider name with its breaker state for JSON output.
type breakerEntry struct {
	Provider string               `json:"provider"`
	Info     provider.BreakerInfo `json:"info"`
}

func (s *Server) handleRouteBreakersResource(_ context.Context, _ *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	entries := []breakerEntry{}
	router := s.routerSnapshot()
	providerRegistry := s.providerRegistrySnapshot()
	if router != nil && providerRegistry != nil {
		for _, name := range providerRegistry.Names() {
			if info, ok := router.BreakerInfo(name); ok {
				entries = append(entries, breakerEntry{Provider: name, Info: info})
			}
		}
	}
	return marshalResource("route://breakers", entries)
}

func (s *Server) handleRouteWarmthResource(_ context.Context, _ *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	snap := s.WarmthSnapshot()
	if snap == nil {
		snap = []provider.WarmModel{}
	}
	return marshalResource("route://warmth", snap)
}

func (s *Server) handleRouteStickyResource(_ context.Context, _ *gomcp.ReadResourceRequest) (*gomcp.ReadResourceResult, error) {
	snap := s.StickyRoutes()
	if snap == nil {
		snap = map[string]provider.StickyRouteInfo{}
	}
	return marshalResource("route://sticky", snap)
}
