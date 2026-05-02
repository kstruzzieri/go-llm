package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/provider"
)

const maxBatchSize = 100

// embedArgs are the parameters for the embed tool.
type embedArgs struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
}

// embedBatchArgs are the parameters for the embed_batch tool.
type embedBatchArgs struct {
	Texts []string `json:"texts"`
	Model string   `json:"model,omitempty"`
}

func (s *Server) registerEmbedTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "embed",
		Description: "Generate an embedding vector for a single text.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":  map[string]any{"type": "string", "description": "Text to embed"},
				"model": map[string]any{"type": "string", "description": "Embedding model (uses configured default if omitted)"},
			},
			"required": []string{"text"},
		},
	}, s.handleEmbed)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "embed_batch",
		Description: fmt.Sprintf("Generate embeddings for multiple texts (max %d).", maxBatchSize),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"texts": map[string]any{
					"type":        "array",
					"description": fmt.Sprintf("Texts to embed (max %d)", maxBatchSize),
					"items":       map[string]any{"type": "string"},
				},
				"model": map[string]any{"type": "string", "description": "Embedding model (uses configured default if omitted)"},
			},
			"required": []string{"texts"},
		},
	}, s.handleEmbedBatch)
}

func (s *Server) handleEmbed(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args embedArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Text == "" {
		return toolError("validation", "text must not be empty"), nil
	}
	router := s.routerSnapshot()
	if router == nil {
		return toolError("config", "router unavailable"), nil
	}

	rr := provider.RoutingRequest{
		Model:          args.Model,
		UseCase:        "embedding",
		RequiredCaps:   provider.CapEmbed,
		Input:          []string{args.Text},
		ExpectedOutput: provider.DefaultExpectedOutput("embedding"),
		Priority:       provider.PriorityNormal,
	}
	if rr.Model == "" {
		chain, err := s.chainFor("embedding")
		if err != nil {
			return toolError("config", "%v", err), nil
		}
		rr.PreferredChain = chain
	}

	plan, err := router.Route(ctx, rr)
	if err != nil {
		return toolError("router", "%v", err), nil
	}
	resp, err := plan.ExecuteEmbed(ctx)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}
	if len(resp.Embeddings) == 0 {
		return toolError("ollama", "no embedding returned"), nil
	}
	data, mErr := json.Marshal(resp.Embeddings[0])
	if mErr != nil {
		return toolError("ollama", "marshal embedding: %v", mErr), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handleEmbedBatch(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args embedBatchArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if len(args.Texts) == 0 {
		return toolError("validation", "texts must not be empty"), nil
	}
	if len(args.Texts) > maxBatchSize {
		return toolError("validation", "texts exceeds maximum batch size of %d (got %d)", maxBatchSize, len(args.Texts)), nil
	}
	router := s.routerSnapshot()
	if router == nil {
		return toolError("config", "router unavailable"), nil
	}

	rr := provider.RoutingRequest{
		Model:          args.Model,
		UseCase:        "embedding",
		RequiredCaps:   provider.CapEmbed,
		Input:          args.Texts,
		ExpectedOutput: provider.DefaultExpectedOutput("embedding"),
		Priority:       provider.PriorityNormal,
	}
	if rr.Model == "" {
		chain, err := s.chainFor("embedding")
		if err != nil {
			return toolError("config", "%v", err), nil
		}
		rr.PreferredChain = chain
	}

	plan, err := router.Route(ctx, rr)
	if err != nil {
		return toolError("router", "%v", err), nil
	}
	resp, err := plan.ExecuteEmbed(ctx)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}
	data, mErr := json.Marshal(resp.Embeddings)
	if mErr != nil {
		return toolError("ollama", "marshal embeddings: %v", mErr), nil
	}
	return toolResult(string(data)), nil
}
