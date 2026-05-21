package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/provider"
)

type listedModelInfo struct {
	provider.ModelInfo
	Provider string `json:"provider,omitempty"`
}

func (s *Server) registerModelTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "list_models",
		Description: "List all available provider models. Also refreshes the model resolution cache.",
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
	// Refresh model resolution cache since model availability may have changed.
	if s.cfg != nil {
		_ = s.refreshResolved(ctx) // non-fatal: partial results kept
	}

	models, err := s.listModels(ctx)
	if err != nil {
		return toolError("provider", "%v", err), nil
	}

	data, err := json.Marshal(models)
	if err != nil {
		return toolError("provider", "marshal models: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) listModels(ctx context.Context) ([]listedModelInfo, error) {
	if pReg := s.providerRegistrySnapshot(); pReg != nil {
		return listProviderRegistryModels(ctx, pReg)
	}
	return s.listLegacyOllamaModels(ctx)
}

func listProviderRegistryModels(ctx context.Context, pReg *provider.Registry) ([]listedModelInfo, error) {
	var out []listedModelInfo
	var firstErr error
	for _, p := range pReg.All() {
		models, err := p.Models(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list models for provider %q: %w", p.Name(), err)
			}
			continue
		}
		for _, model := range models {
			if model.Name == "" {
				continue
			}
			out = append(out, listedModelInfo{
				ModelInfo: model,
				Provider:  p.Name(),
			})
			_ = pReg.AddModelToIndex(model.Name, p.Name())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].Name < out[j].Name
		}
		return out[i].Provider < out[j].Provider
	})
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (s *Server) listLegacyOllamaModels(ctx context.Context) ([]listedModelInfo, error) {
	models, err := s.client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]listedModelInfo, 0, len(models))
	for _, model := range models {
		if model.Name == "" {
			continue
		}
		out = append(out, listedModelInfo{
			ModelInfo: provider.ModelInfo{
				Name:          model.Name,
				Family:        model.Family,
				ParameterSize: model.ParamSize,
				QuantLevel:    model.QuantLevel,
				Template:      model.Template,
				Capabilities:  model.Capabilities,
				Digest:        model.Digest,
			},
			Provider: "ollama",
		})
	}
	return out, nil
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
