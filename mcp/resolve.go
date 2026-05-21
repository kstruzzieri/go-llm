package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

type resolvedModelTarget struct {
	Name     string
	Provider string
}

func (t resolvedModelTarget) selector() string {
	return modelSelector(t.Provider, t.Name)
}

// resolveModel resolves a model name for a given use-case.
// If explicit is non-empty, it is returned directly.
// Otherwise, the resolved map is consulted for the use-case key.
// Use-case keys match config.Defaults (e.g., "chat", "completion",
// "embedding", "analysis"), not role names.
func (s *Server) resolveModel(ctx context.Context, explicit, useCase string) (string, error) {
	target, err := s.resolveModelTarget(ctx, explicit, useCase)
	if err != nil {
		return "", err
	}
	return target.Name, nil
}

// resolveModelTarget is resolveModel's provider-aware sibling. Explicit
// qualified selectors preserve their provider component; configured defaults
// return the provider instance captured by config.ResolvedModel.
func (s *Server) resolveModelTarget(ctx context.Context, explicit, useCase string) (resolvedModelTarget, error) {
	if explicit != "" {
		if key, ok := parseModelSelector(explicit); ok {
			return resolvedModelTarget{Name: key.Model, Provider: key.Provider}, nil
		}
		return resolvedModelTarget{Name: explicit}, nil
	}

	// No config available — model must be provided explicitly.
	if s.cfg == nil {
		return resolvedModelTarget{}, fmt.Errorf("model parameter required (no models.json configured)")
	}

	resolved := s.snapshotResolved()
	needsRefresh := len(resolved) == 0
	if !needsRefresh {
		rm, ok := resolved[useCase]
		needsRefresh = !ok || rm.Name == ""
	}
	if needsRefresh {
		s.maybeRefreshResolved(ctx)
		resolved = s.snapshotResolved()
	}

	// Config exists but resolved map is empty (Ollama was unavailable).
	if len(resolved) == 0 {
		return resolvedModelTarget{}, fmt.Errorf("defaults unavailable; provide model explicitly")
	}

	rm, ok := resolved[useCase]
	if !ok {
		return resolvedModelTarget{}, fmt.Errorf("no model configured for use-case %q", useCase)
	}
	if rm.Name == "" {
		return resolvedModelTarget{}, fmt.Errorf("default for use-case %q did not resolve", useCase)
	}

	return resolvedModelTarget{Name: rm.Name, Provider: rm.Provider}, nil
}

// refreshResolved re-resolves all models against registered provider instances
// and rebuilds derived clients. Partial results are stored even on error.
func (s *Server) refreshResolved(ctx context.Context) error {
	if s.cfg == nil {
		return fmt.Errorf("mcp: refresh models: no configuration loaded")
	}
	checker := s.modelChecker()
	if checker == nil {
		return fmt.Errorf("mcp: refresh models: client unavailable")
	}

	resolved, err := s.cfg.ResolveAll(ctx, checker)

	// Store partial results and rebuild derived clients under the write lock
	// so future requests see the freshest resolved defaults. Derived clients
	// are rebuilt outside the lock because completion-provider construction may
	// perform cancelable network I/O.
	s.mu.Lock()
	if resolved != nil {
		s.resolved = resolved
	}
	s.mu.Unlock()
	s.rebuildDerivedClients(ctx)

	if err != nil {
		return fmt.Errorf("mcp: refresh models: resolve: %w", err)
	}
	return nil
}

func (s *Server) maybeRefreshResolved(ctx context.Context) {
	if s.modelChecker() == nil {
		return
	}
	_ = s.refreshResolved(ctx)
}

func (s *Server) snapshotResolved() map[string]config.ResolvedModel {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resolved := make(map[string]config.ResolvedModel, len(s.resolved))
	for k, v := range s.resolved {
		resolved[k] = v
	}
	return resolved
}

// chainFor returns the ordered config-derived selector chain for a use-case,
// suitable for RoutingRequest.PreferredChain. It returns config errors before
// handlers call Router.Route so empty chains cannot fall into global Recommend.
func (s *Server) chainFor(useCase string) ([]string, error) {
	if s.cfg == nil {
		return nil, fmt.Errorf("model parameter required (no models.json configured)")
	}
	chain, err := s.cfg.RoleFallbackChain(useCase)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("no model configured for use-case %q", useCase)
	}
	return chain, nil
}

func (s *Server) modelChecker() config.ModelChecker {
	if pReg := s.providerRegistrySnapshot(); pReg != nil {
		return providerRegistryModelChecker{registry: pReg}
	}
	if s.client != nil {
		return s.client
	}
	return nil
}

type providerRegistryModelChecker struct {
	registry *provider.Registry
}

func (c providerRegistryModelChecker) AvailableModels(ctx context.Context) ([]string, error) {
	keys, err := c.AvailableModelKeys(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(keys))
	models := make([]string, 0, len(keys))
	for _, key := range keys {
		if key.Model == "" || seen[key.Model] {
			continue
		}
		seen[key.Model] = true
		models = append(models, key.Model)
	}
	return models, nil
}

func (c providerRegistryModelChecker) AvailableModelKeys(ctx context.Context) ([]provider.ModelKey, error) {
	if c.registry == nil {
		return nil, fmt.Errorf("mcp: provider registry unavailable")
	}
	var keys []provider.ModelKey
	var firstErr error
	for _, p := range c.registry.All() {
		models, err := p.Models(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mcp: list models for provider %q: %w", p.Name(), err)
			}
			continue
		}
		for _, model := range models {
			if model.Name == "" {
				continue
			}
			key := provider.ModelKey{Provider: p.Name(), Model: model.Name}
			keys = append(keys, key)
			_ = c.registry.AddModelToIndex(model.Name, p.Name())
		}
	}
	if len(keys) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return keys, nil
}

func parseModelSelector(selector string) (provider.ModelKey, bool) {
	providerName, model, ok := strings.Cut(selector, "/")
	if !ok || providerName == "" || model == "" {
		return provider.ModelKey{}, false
	}
	return provider.ModelKey{Provider: providerName, Model: model}, true
}

func modelSelector(providerName, model string) string {
	if providerName == "" || model == "" {
		return model
	}
	return providerName + "/" + model
}
