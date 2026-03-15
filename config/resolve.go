package config

import (
	"context"
	"fmt"
	"strings"
)

// ModelChecker abstracts checking model availability against a backend.
type ModelChecker interface {
	AvailableModels(ctx context.Context) ([]string, error)
}

// ResolvedModel is the result of resolving a role to an available model.
type ResolvedModel struct {
	Name       string
	Role       string
	IsFallback bool
}

// Resolve checks availability and walks the fallback chain for a single use-case.
// It calls checker.AvailableModels once, builds a lookup set, then tries the
// primary model followed by each fallback until one is found in the set.
func (c *Config) Resolve(ctx context.Context, checker ModelChecker, useCase string) (ResolvedModel, error) {
	role, ok := c.Defaults[useCase]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("config: unknown use-case %q", useCase)
	}

	models, err := checker.AvailableModels(ctx)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("config: checking available models: %w", err)
	}

	available := toSet(models)
	return c.resolveRole(role, available)
}

// ResolveAll resolves every entry in Defaults with a single AvailableModels call.
// It returns partial results alongside an error if some use-cases could not resolve.
func (c *Config) ResolveAll(ctx context.Context, checker ModelChecker) (map[string]ResolvedModel, error) {
	models, err := checker.AvailableModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: checking available models: %w", err)
	}

	available := toSet(models)
	results := make(map[string]ResolvedModel, len(c.Defaults))
	var errs []string

	for useCase, role := range c.Defaults {
		resolved, err := c.resolveRole(role, available)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", useCase, err))
			continue
		}
		results[useCase] = resolved
	}

	if len(errs) > 0 {
		return results, fmt.Errorf("config: unresolved use-cases: %s", strings.Join(errs, "; "))
	}
	return results, nil
}

// resolveRole tries the primary model for the given role, then walks its
// Fallbacks slice, returning the first model found in the available set.
func (c *Config) resolveRole(role string, available map[string]bool) (ResolvedModel, error) {
	m, ok := c.Models[role]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("config: unknown role %q", role)
	}

	// Try primary model.
	if available[m.Name] {
		return ResolvedModel{Name: m.Name, Role: role, IsFallback: false}, nil
	}

	// Walk fallback chain: try each fallback role (and transitively its own fallbacks).
	for _, fbRole := range m.Fallbacks {
		resolved, err := c.resolveRole(fbRole, available)
		if err == nil {
			resolved.IsFallback = true
			return resolved, nil
		}
	}

	return ResolvedModel{}, fmt.Errorf("config: no available model for role %q", role)
}

// toSet converts a string slice to a map for O(1) lookups.
func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}
