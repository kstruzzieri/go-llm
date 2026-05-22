package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
				"provider": map[string]any{
					"type":        "string",
					"description": "Provider instance name when the model name is ambiguous",
				},
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
		Name     string `json:"name"`
		Provider string `json:"provider,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Name == "" {
		return toolError("validation", "name must not be empty"), nil
	}

	info, err := s.showModel(ctx, args.Name, args.Provider)
	if err != nil {
		return toolError("provider", "%v", err), nil
	}

	data, err := json.Marshal(info)
	if err != nil {
		return toolError("provider", "marshal model info: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) showModel(ctx context.Context, name, providerName string) (any, error) {
	key, ok, err := s.modelKeyForShowModel(ctx, name, providerName)
	if err != nil {
		return nil, err
	}
	if ok {
		return s.lookupProviderModelInfo(ctx, key)
	}

	info, err := s.client.ShowModel(ctx, name)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (s *Server) modelKeyForShowModel(ctx context.Context, name, providerName string) (provider.ModelKey, bool, error) {
	if providerName != "" {
		return provider.ModelKey{Provider: providerName, Model: name}, true, nil
	}
	if key, ok := s.parseKnownModelSelector(name); ok {
		return key, true, nil
	}

	inferredProvider, err := s.inferProviderForExplicitModel(ctx, name)
	if err != nil {
		return provider.ModelKey{}, false, err
	}
	if inferredProvider != "" {
		return provider.ModelKey{Provider: inferredProvider, Model: name}, true, nil
	}
	return provider.ModelKey{}, false, nil
}

func (s *Server) lookupProviderModelInfo(ctx context.Context, key provider.ModelKey) (listedModelInfo, error) {
	s.mu.RLock()
	modelRegistry := s.modelRegistry
	s.mu.RUnlock()
	if modelRegistry != nil {
		profile, err := modelRegistry.Lookup(ctx, key)
		if err == nil {
			return listedModelInfo{
				ModelInfo: modelInfoFromProfile(profile),
				Provider:  key.Provider,
			}, nil
		}
	}

	pReg := s.providerRegistrySnapshot()
	if pReg == nil {
		return listedModelInfo{}, fmt.Errorf("provider registry unavailable")
	}
	p, ok := pReg.Get(key.Provider)
	if !ok {
		return listedModelInfo{}, fmt.Errorf("provider %q not found", key.Provider)
	}

	models, err := p.Models(ctx)
	if err != nil {
		return listedModelInfo{}, err
	}
	for _, model := range models {
		if model.Name == key.Model {
			return listedModelInfo{
				ModelInfo: model,
				Provider:  key.Provider,
			}, nil
		}
	}
	return listedModelInfo{}, fmt.Errorf("model %q not found on provider %q", key.Model, key.Provider)
}

func modelInfoFromProfile(profile *provider.ModelProfile) provider.ModelInfo {
	if profile == nil {
		return provider.ModelInfo{}
	}
	return provider.ModelInfo{
		Name:          profile.Key.Model,
		Family:        profile.Family,
		ParameterSize: profile.Resources.ParameterSize,
		QuantLevel:    profile.Resources.QuantLevel,
		Template:      profile.Template,
		Capabilities:  capabilityNames(profile.Caps),
		ContextWindow: profile.ContextWindow,
		Digest:        profile.Digest,
	}
}

func capabilityNames(caps provider.Capability) []string {
	if caps == 0 {
		return nil
	}
	return strings.Split(caps.String(), "|")
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
