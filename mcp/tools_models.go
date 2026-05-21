package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerModelTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "list_models",
		Description: "List all available Ollama models. Also refreshes the model resolution cache.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, s.handleListModels)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "show_model",
		Description: "Get detailed information about a specific model.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Model name (e.g. qwen3:8b)"},
			},
			"required": []string{"name"},
		},
	}, s.handleShowModel)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "pull_model",
		Description: "Download a model from the Ollama registry.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Model name to pull (e.g. qwen3:8b)"},
			},
			"required": []string{"name"},
		},
	}, s.handlePullModel)
}

func (s *Server) handleListModels(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	models, err := s.client.ListModels(ctx)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	// Refresh model resolution cache since model availability may have changed.
	if s.cfg != nil {
		_ = s.refreshResolved(ctx) // non-fatal: partial results kept
	}
	// Self-heal the providerRegistry's model index so the Recommend safety-net
	// tail works after providers recover from boot-time unreachability.
	s.refreshProviderModelIndexes(ctx)

	data, err := json.Marshal(models)
	if err != nil {
		return toolError("ollama", "marshal models: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handleShowModel(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Name == "" {
		return toolError("validation", "name must not be empty"), nil
	}

	info, err := s.client.ShowModel(ctx, args.Name)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	data, err := json.Marshal(info)
	if err != nil {
		return toolError("ollama", "marshal model info: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handlePullModel(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Name == "" {
		return toolError("validation", "name must not be empty"), nil
	}

	err := s.client.PullModel(ctx, args.Name, nil)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	// Refresh model resolution cache after successful pull.
	if s.cfg != nil {
		_ = s.refreshResolved(ctx) // non-fatal: partial results kept
	}
	// Self-heal the providerRegistry's model index so the freshly pulled
	// model becomes routable immediately.
	s.refreshProviderModelIndexes(ctx)

	return toolResult(fmt.Sprintf("model %q pulled successfully", args.Name)), nil
}
