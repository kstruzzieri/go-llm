package mcp

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
)

// resolveModel resolves a model name for a given use-case.
// If explicit is non-empty, it is returned directly.
// Otherwise, the resolved map is consulted for the use-case key.
// Use-case keys match config.Defaults (e.g., "chat", "completion",
// "embedding", "analysis"), not role names.
func (s *Server) resolveModel(ctx context.Context, explicit, useCase string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	// No config available — model must be provided explicitly.
	if s.cfg == nil {
		return "", fmt.Errorf("model parameter required (no models.json configured)")
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
		return "", fmt.Errorf("defaults unavailable; provide model explicitly")
	}

	rm, ok := resolved[useCase]
	if !ok {
		return "", fmt.Errorf("no model configured for use-case %q", useCase)
	}
	if rm.Name == "" {
		return "", fmt.Errorf("default for use-case %q did not resolve", useCase)
	}

	return rm.Name, nil
}

// refreshResolved re-resolves all models against the running Ollama instance
// and rebuilds derived clients. Partial results are stored even on error.
func (s *Server) refreshResolved(ctx context.Context) error {
	if s.cfg == nil {
		return fmt.Errorf("mcp: refresh models: no configuration loaded")
	}
	if s.client == nil {
		return fmt.Errorf("mcp: refresh models: client unavailable")
	}

	resolved, err := s.cfg.ResolveAll(ctx, s.client)

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
	if s.client == nil {
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
