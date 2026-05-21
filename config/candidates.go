package config

import (
	"context"
	"fmt"
)

// CandidateModel represents an available model discovered during fallback
// chain traversal. Unlike ResolvedModel (which returns the first match),
// CandidateModel is one entry in a fully enumerated list of all available
// models for a use-case, ordered by config preference.
type CandidateModel struct {
	Provider   string // the provider instance owning the model
	Name       string // the model name (e.g. "qwen3.5:27b")
	Role       string // the config role this model belongs to (e.g. "general")
	IsFallback bool   // true if reached via fallback chain, not the primary role
}

// ResolveCandidates returns all available models for a use-case, ordered by
// config preference (primary first, then fallback chain traversal order).
// Unlike Resolve (which returns the first available model), this enumerates
// every reachable model that is currently available, giving orchestration
// layers the full candidate set for enrichment and scoring.
//
// Returns an empty slice (not an error) when no candidates are available.
// Errors are reserved for operational failures, unknown use-cases, and invalid
// role references encountered during traversal.
func (c *Config) ResolveCandidates(ctx context.Context, checker ModelChecker, useCase string) ([]CandidateModel, error) {
	if checker == nil {
		return nil, fmt.Errorf("config: model checker is required")
	}
	role, ok := c.Defaults[useCase]
	if !ok {
		return nil, fmt.Errorf("config: unknown use-case %q", useCase)
	}

	available, err := availableModels(ctx, checker)
	if err != nil {
		return nil, fmt.Errorf("config: checking available models: %w", err)
	}

	visited := make(map[string]bool)
	candidates := make([]CandidateModel, 0)
	if err := c.collectCandidates(role, available, visited, false, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

// collectCandidates walks the fallback chain from the given role, appending
// every available model to candidates. It continues walking after finding
// matches (unlike resolveRole which short-circuits). The visited set prevents
// both circular fallbacks and duplicate entries in diamond-shaped graphs
// (e.g. a→b→d, a→c→d — d appears once, from whichever branch reaches it first).
func (c *Config) collectCandidates(role string, available modelAvailability, visited map[string]bool, isFallback bool, candidates *[]CandidateModel) error {
	if visited[role] {
		return nil // already visited (cycle or diamond) — skip
	}

	m, ok := c.Models[role]
	if !ok {
		return fmt.Errorf("config: unknown role %q", role)
	}
	visited[role] = true

	if m.Provider == "" {
		return fmt.Errorf("config: role %q has empty provider; use config.Load to materialize defaults or set ModelConfig.Provider explicitly", role)
	}

	if available.has(m.Provider, m.Name) {
		*candidates = append(*candidates, CandidateModel{
			Provider:   m.Provider,
			Name:       m.Name,
			Role:       role,
			IsFallback: isFallback,
		})
	}

	for _, fbRole := range m.Fallbacks {
		if err := c.collectCandidates(fbRole, available, visited, true, candidates); err != nil {
			return err
		}
	}
	return nil
}
