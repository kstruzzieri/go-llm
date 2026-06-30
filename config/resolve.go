package config

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// ModelChecker abstracts checking model availability against a backend.
type ModelChecker interface {
	AvailableModels(ctx context.Context) ([]string, error)
}

// ProviderModelChecker is an optional provider-aware extension for callers
// that can report model availability per configured provider instance.
type ProviderModelChecker interface {
	AvailableModelKeys(ctx context.Context) ([]provider.ModelKey, error)
}

type modelAvailability struct {
	names map[string]bool
	keys  map[provider.ModelKey]bool
}

func availabilityFromNames(names []string) modelAvailability {
	return modelAvailability{names: toSet(names)}
}

func availabilityFromKeys(keys []provider.ModelKey) modelAvailability {
	available := modelAvailability{
		names: make(map[string]bool, len(keys)),
		keys:  make(map[provider.ModelKey]bool, len(keys)),
	}
	for _, key := range keys {
		if key.Provider == "" || key.Model == "" {
			continue
		}
		available.names[key.Model] = true
		available.keys[key] = true
	}
	return available
}

func (a modelAvailability) has(providerName, model string) bool {
	if a.keys != nil {
		return a.keys[provider.ModelKey{Provider: providerName, Model: model}]
	}
	return a.names[model]
}

// ResolvedModel is the result of resolving a role to an available model.
// Provider tracks the provider instance that owns the actually-resolved model,
// including the fallback case — if role "coding" on provider "local-a" falls
// back to role "fast" on provider "local-b", Provider is "local-b". The
// originally-requested provider/role belongs in chain metadata or route
// outcome planning, not here.
type ResolvedModel struct {
	Name       string // the model name that was selected
	Role       string // the role of the resolved model (may differ from requested role if fallback)
	Provider   string // the provider instance owning the resolved model
	IsFallback bool   // true if the primary model wasn't available
}

// Resolve checks availability and walks the fallback chain for a single use-case.
// It calls checker.AvailableModels once, builds a lookup set, then tries the
// primary model followed by each fallback until one is found in the set. Optional
// auxiliary use-cases first fall back to existing configured defaults when their
// own Defaults slot is absent.
func (c *Config) Resolve(ctx context.Context, checker ModelChecker, useCase string) (ResolvedModel, error) {
	if checker == nil {
		return ResolvedModel{}, fmt.Errorf("config: model checker is required")
	}
	role, ok := c.RoleForUseCase(useCase)
	if !ok {
		return ResolvedModel{}, fmt.Errorf("config: unknown use-case %q", useCase)
	}

	available, err := availableModels(ctx, checker)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("config: checking available models: %w", err)
	}

	return c.resolveRole(role, available, nil)
}

// ResolveAll resolves every entry in Defaults with a single AvailableModels call.
// It returns partial results alongside an error if some use-cases could not resolve.
func (c *Config) ResolveAll(ctx context.Context, checker ModelChecker) (map[string]ResolvedModel, error) {
	if checker == nil {
		return nil, fmt.Errorf("config: model checker is required")
	}
	available, err := availableModels(ctx, checker)
	if err != nil {
		return nil, fmt.Errorf("config: checking available models: %w", err)
	}

	results := make(map[string]ResolvedModel, len(c.Defaults))
	var errs []string

	// Sort for deterministic error messages.
	useCases := make([]string, 0, len(c.Defaults))
	for useCase := range c.Defaults {
		useCases = append(useCases, useCase)
	}
	sort.Strings(useCases)

	for _, useCase := range useCases {
		role := c.Defaults[useCase]
		resolved, err := c.resolveRole(role, available, nil)
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

func availableModels(ctx context.Context, checker ModelChecker) (modelAvailability, error) {
	if keyed, ok := checker.(ProviderModelChecker); ok {
		keys, err := keyed.AvailableModelKeys(ctx)
		if err != nil {
			return modelAvailability{}, err
		}
		return availabilityFromKeys(keys), nil
	}
	models, err := checker.AvailableModels(ctx)
	if err != nil {
		return modelAvailability{}, err
	}
	return availabilityFromNames(models), nil
}

// resolveRole tries the primary model for the given role, then walks its
// Fallbacks slice, returning the first model found in the available set.
// The onStack set tracks the current DFS path to detect circular fallbacks
// while allowing diamond-shaped shared fallback nodes to be visited from
// multiple parents (defensive — Load validates against cycles, but this
// protects programmatic use).
func (c *Config) resolveRole(role string, available modelAvailability, onStack map[string]bool) (ResolvedModel, error) {
	if onStack == nil {
		onStack = make(map[string]bool)
	}
	if onStack[role] {
		return ResolvedModel{}, fmt.Errorf("config: circular fallback at role %q", role)
	}
	onStack[role] = true
	defer delete(onStack, role)

	m, ok := c.Models[role]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("config: unknown role %q", role)
	}

	// Load+applyDefaults guarantees a non-empty Provider for every model.
	// Reaching this with an empty Provider means the caller constructed
	// Config programmatically and skipped Load — silently inserting
	// "ollama" here would lie to downstream consumers about which provider
	// instance owns the model. Surface it as a real configuration error
	// pointing the caller at the exported APIs they should use instead.
	if m.Provider == "" {
		return ResolvedModel{}, fmt.Errorf("config: role %q has empty provider; use config.Load to materialize defaults or set ModelConfig.Provider explicitly", role)
	}

	// Try primary model.
	if available.has(m.Provider, m.Name) {
		return ResolvedModel{Name: m.Name, Role: role, Provider: m.Provider, IsFallback: false}, nil
	}

	// Walk fallback chain: try each fallback role (and transitively its own fallbacks).
	var fallbackErrs []string
	for _, fbRole := range m.Fallbacks {
		resolved, err := c.resolveRole(fbRole, available, onStack)
		if err == nil {
			resolved.IsFallback = true
			return resolved, nil
		}
		fallbackErrs = append(fallbackErrs, fmt.Sprintf("%s: %v", fbRole, err))
	}

	if len(fallbackErrs) > 0 {
		return ResolvedModel{}, fmt.Errorf("config: no available model for role %q (fallback errors: %s)",
			role, strings.Join(fallbackErrs, "; "))
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

// RoleFallbackChain returns the ordered chain of model selectors for a use-case
// role: the primary model first, then each fallback in declared order, then
// transitively each fallback's fallbacks. Optional auxiliary use-cases first
// fall back to existing configured defaults when absent. Selectors are always provider-
// qualified ("provider/model") because Load defaults Model.Provider to
// "ollama" when unset.
//
// The chain is first-seen unique: a model encountered via multiple paths in a
// diamond fallback graph appears only once. Cycles in the fallback graph
// surface as an error. Availability is NOT filtered — that is the Router's
// job via breakers, warmth, and gates.
func (c *Config) RoleFallbackChain(useCase string) ([]string, error) {
	role, ok := c.RoleForUseCase(useCase)
	if !ok {
		return nil, fmt.Errorf("config: unknown use-case %q", useCase)
	}
	var chain []string
	seen := make(map[string]bool)    // dedupe on selector string
	onStack := make(map[string]bool) // cycle detection on role
	if err := c.walkRole(role, &chain, seen, onStack); err != nil {
		return nil, err
	}
	return chain, nil
}

func (c *Config) walkRole(role string, chain *[]string, seen, onStack map[string]bool) error {
	if onStack[role] {
		return fmt.Errorf("config: circular fallback at role %q", role)
	}
	onStack[role] = true
	defer delete(onStack, role)

	m, ok := c.Models[role]
	if !ok {
		return fmt.Errorf("config: unknown role %q", role)
	}
	// Provider is guaranteed non-empty by Load (defaults to "ollama" when
	// unset). Always emit the qualified form.
	provider := m.Provider
	if provider == "" {
		provider = "ollama"
	}
	selector := provider + "/" + m.Name
	if !seen[selector] {
		seen[selector] = true
		*chain = append(*chain, selector)
	}
	for _, fb := range m.Fallbacks {
		if err := c.walkRole(fb, chain, seen, onStack); err != nil {
			return err
		}
	}
	return nil
}
