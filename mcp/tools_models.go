package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
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
		Description: "Download/install a model using a pull-capable provider.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Model name to pull (e.g. qwen3:8b). May also be provider/model when the provider is known."},
				"provider": map[string]any{
					"type":        "string",
					"description": "Provider instance name when multiple pull-capable providers exist",
				},
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
		models, err := pReg.RefreshModelsAndList(ctx, p.Name())
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
	pReg := s.providerRegistrySnapshot()
	if pReg == nil {
		return listedModelInfo{}, fmt.Errorf("provider registry unavailable")
	}

	models, err := pReg.RefreshModelsAndList(ctx, key.Provider)
	if err != nil {
		return listedModelInfo{}, err
	}
	for i := range models {
		if models[i].Name != key.Model {
			continue
		}

		s.mu.RLock()
		modelRegistry := s.modelRegistry
		s.mu.RUnlock()
		if modelRegistry != nil {
			profile, err := modelRegistry.Refresh(ctx, key)
			if err == nil {
				return listedModelInfo{
					ModelInfo: modelInfoFromProfile(profile),
					Provider:  key.Provider,
				}, nil
			}
		}

		return listedModelInfo{
			ModelInfo: models[i],
			Provider:  key.Provider,
		}, nil
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

// pullerForModel returns the ModelPuller that should service a pull for name,
// or nil when the owning provider does not support pull. providerName, when
// non-empty, is authoritative.
func pullerForModel(reg *provider.Registry, name, providerName string) (provider.ModelPuller, error) {
	if reg == nil {
		return nil, nil
	}
	if providerName != "" {
		p, ok := reg.Get(providerName)
		if !ok {
			return nil, fmt.Errorf("provider %q not found", providerName)
		}
		mp, ok := p.(provider.ModelPuller)
		if !ok {
			return nil, nil
		}
		return mp, nil
	}

	candidates, err := reg.ProvidersForModel(name)
	if err == nil && len(candidates) > 0 {
		for _, p := range candidates {
			if mp, ok := p.(provider.ModelPuller); ok {
				return mp, nil
			}
		}
		return nil, nil
	}

	var pullers []provider.ModelPuller
	var names []string
	for _, name := range reg.Names() {
		p, _ := reg.Get(name)
		if mp, ok := p.(provider.ModelPuller); ok {
			pullers = append(pullers, mp)
			names = append(names, name)
		}
	}
	switch len(pullers) {
	case 0:
		return nil, nil
	case 1:
		return pullers[0], nil
	default:
		return nil, fmt.Errorf("model %q is not advertised by any provider and multiple pull-capable providers exist (%s); pass provider", name, strings.Join(names, ", "))
	}
}

func (s *Server) handlePullModel(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
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

	puller, modelName, err := s.modelPuller(args.Name, args.Provider)
	if err != nil {
		return toolError("validation", "%v", err), nil
	}
	if puller == nil {
		return toolError("unsupported",
			"pull is not supported for model %q: this backend's models are file-managed (e.g. GGUF served by llama-server). Install the model file and add it to models.json instead",
			modelName), nil
	}

	if err := puller.PullModel(ctx, modelName, nil); err != nil {
		return toolError("provider", "%v", err), nil
	}

	// Refresh model resolution cache after successful pull.
	if s.cfg != nil {
		_ = s.refreshResolved(ctx) // non-fatal: partial results kept
	}
	// Self-heal the provider registry's model index so the freshly pulled
	// model becomes routable immediately.
	s.refreshProviderModelIndexes(ctx)

	return toolResult(fmt.Sprintf("model %q pulled successfully", modelName)), nil
}

// modelPuller resolves the puller and normalized model name, preserving the
// legacy (no-registry) Ollama path.
func (s *Server) modelPuller(name, providerName string) (provider.ModelPuller, string, error) {
	if pReg := s.providerRegistrySnapshot(); pReg != nil {
		modelName := name
		if providerName == "" {
			if key, ok := s.parseKnownModelSelector(name); ok {
				providerName = key.Provider
				modelName = key.Model
			}
		}
		puller, err := pullerForModel(pReg, modelName, providerName)
		return puller, modelName, err
	}
	if s.client != nil {
		return ollamaClientPuller{c: s.client}, name, nil
	}
	return nil, name, nil
}

// ollamaClientPuller adapts the legacy direct *ollama.Client to ModelPuller
// for servers constructed without a provider registry.
type ollamaClientPuller struct{ c *ollama.Client }

func (a ollamaClientPuller) PullModel(ctx context.Context, name string, fn func(status string, completed, total int64)) error {
	return a.c.PullModel(ctx, name, fn)
}
