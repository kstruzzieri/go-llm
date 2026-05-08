package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry is a thread-safe collection of named providers.
// It maintains an index mapping model names to the providers that advertise them,
// enabling model-based provider lookup via Resolve and ProvidersForModel.
type Registry struct {
	mu         sync.RWMutex
	providers  map[string]Provider
	modelIndex map[string][]string // model name -> provider names advertising it
}

// NewRegistry creates an empty provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers:  make(map[string]Provider),
		modelIndex: make(map[string][]string),
	}
}

// Register adds a provider to the registry. Returns an error if the provider is
// nil, has an empty name, or a provider with the same name is already registered.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return fmt.Errorf("provider: register: provider must not be nil")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("provider: register: provider name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("provider: register: provider %q is already registered", name)
	}
	r.providers[name] = p
	return nil
}

// Unregister removes a provider from the registry and clears its model index
// entries. Returns an error if the provider is not found.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[name]; !exists {
		return fmt.Errorf("provider: unregister: provider %q not found", name)
	}

	delete(r.providers, name)

	// Remove this provider from all model index entries.
	for model, providerNames := range r.modelIndex {
		filtered := providerNames[:0]
		for _, pn := range providerNames {
			if pn != name {
				filtered = append(filtered, pn)
			}
		}
		if len(filtered) == 0 {
			delete(r.modelIndex, model)
		} else {
			r.modelIndex[model] = filtered
		}
	}
	return nil
}

// Get retrieves a provider by name. Returns the provider and true if found,
// or nil and false if not registered.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// All returns all registered providers. The returned slice is a copy and safe
// to iterate without holding a lock.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}

// Names returns the registered provider names in deterministic (sorted) order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolve finds a provider by the ModelKey's Provider field. Returns an error
// if the named provider is not registered.
func (r *Registry) Resolve(key ModelKey) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[key.Provider]
	if !ok {
		return nil, fmt.Errorf("provider: resolve: provider %q not found", key.Provider)
	}
	return p, nil
}

// ProvidersForModel returns all providers that advertise the given model name.
// The model index is populated by RefreshModels. Returns an error if no
// providers have the model.
func (r *Registry) ProvidersForModel(model string) ([]Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerNames, ok := r.modelIndex[model]
	if !ok || len(providerNames) == 0 {
		return nil, fmt.Errorf("provider: no providers found for model %q", model)
	}

	result := make([]Provider, 0, len(providerNames))
	for _, name := range providerNames {
		if p, exists := r.providers[name]; exists {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("provider: no providers found for model %q", model)
	}
	return result, nil
}

// AddModelToIndex registers that providerName advertises model in the routing
// index used by ProvidersForModel. Idempotent: re-adding an existing
// (model, providerName) pair is a no-op rather than producing duplicate
// entries. Returns an error if either argument is empty or providerName is
// not a registered provider.
//
// Intended for callers that have proof-of-existence for a single model from
// a side channel (e.g. an /api/show response that succeeded while
// /api/tags-based RefreshModels failed) and need to seed the routing index
// without bulk discovery. The bulk path is RefreshModels; this is the
// single-model fallback for partial-outage scenarios.
func (r *Registry) AddModelToIndex(model, providerName string) error {
	if model == "" {
		return fmt.Errorf("provider: add model to index: model name must not be empty")
	}
	if providerName == "" {
		return fmt.Errorf("provider: add model to index: provider name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[providerName]; !exists {
		return fmt.Errorf("provider: add model to index: provider %q not found", providerName)
	}

	for _, existing := range r.modelIndex[model] {
		if existing == providerName {
			return nil
		}
	}
	r.modelIndex[model] = append(r.modelIndex[model], providerName)
	return nil
}

// RefreshModels queries the named provider for its available models and updates
// the registry's model index. This must be called after registration to populate
// the model index for ProvidersForModel lookups.
func (r *Registry) RefreshModels(ctx context.Context, providerName string) error {
	r.mu.RLock()
	p, ok := r.providers[providerName]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("provider: refresh models: provider %q not found", providerName)
	}

	models, err := p.Models(ctx)
	if err != nil {
		return fmt.Errorf("provider: refresh models for %q: %w", providerName, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove old entries for this provider.
	for model, providerNames := range r.modelIndex {
		filtered := providerNames[:0]
		for _, pn := range providerNames {
			if pn != providerName {
				filtered = append(filtered, pn)
			}
		}
		if len(filtered) == 0 {
			delete(r.modelIndex, model)
		} else {
			r.modelIndex[model] = filtered
		}
	}

	// Add new entries.
	for _, m := range models {
		r.modelIndex[m.Name] = append(r.modelIndex[m.Name], providerName)
	}

	return nil
}
