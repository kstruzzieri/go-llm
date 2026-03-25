package mcp

import (
	"context"
	"fmt"
)

// resolveModel resolves a model name for a given use-case.
// If explicit is non-empty, it is returned directly.
// Otherwise, the resolved map is consulted for the use-case key.
// Use-case keys match config.Defaults (e.g., "chat", "completion",
// "embedding", "analysis"), not role names.
func (s *Server) resolveModel(explicit, useCase string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}

	// No config available — model must be provided explicitly.
	if s.cfg == nil {
		return "", fmt.Errorf("mcp: model parameter required (no models.json configured)")
	}

	s.mu.RLock()
	resolved := s.resolved
	s.mu.RUnlock()

	// Config exists but resolved map is empty (Ollama was unavailable).
	if len(resolved) == 0 {
		return "", fmt.Errorf("mcp: defaults unavailable; provide model explicitly")
	}

	rm, ok := resolved[useCase]
	if !ok {
		return "", fmt.Errorf("mcp: no model configured for use-case %q", useCase)
	}
	if rm.Name == "" {
		return "", fmt.Errorf("mcp: default for use-case %q did not resolve", useCase)
	}

	return rm.Name, nil
}

// refreshResolved re-resolves all models against the running Ollama instance
// and rebuilds derived clients. Partial results are stored even on error.
func (s *Server) refreshResolved(ctx context.Context) error {
	if s.cfg == nil {
		return fmt.Errorf("mcp: no configuration loaded")
	}

	resolved, err := s.cfg.ResolveAll(ctx, s.client)

	// Store partial results even when some use-cases fail to resolve.
	if resolved != nil {
		s.mu.Lock()
		s.resolved = resolved
		s.mu.Unlock()
	}

	s.rebuildDerivedClients()

	if err != nil {
		return fmt.Errorf("mcp: refresh models: %w", err)
	}
	return nil
}
