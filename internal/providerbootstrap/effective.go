package providerbootstrap

import (
	"fmt"
	"maps"
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// effectiveProviders is the pure output of materialization: the ONE config
// every later consumer reads — display, network plan, manifest, client
// construction, receipt — plus a validated destination identity per
// provider. Nothing here has performed I/O.
type effectiveProviders struct {
	cfg *config.Config
	// dests maps provider key -> validated destination/v1 identity, derived
	// from the SAME effective base URLs the clients will dial. Task 5's
	// network plan and Task 6's guarded clients both key off this map, so
	// the admitted destination and the dialed destination cannot diverge.
	dests map[string]provider.Destination
}

// materializeEffectiveConfig applies the nil-config default and BOTH URL
// override families onto a copied config, normalizes the defaulted
// api_format, and validates every provider's base URL as a destination
// identity. Pure: no I/O, and the caller's config is never mutated.
//
// This is the D7 fix for the pre-#477 divergence (F10): the Ollama override
// used to be applied only inside client construction, so Bundle.Config
// reported a URL the live client was not dialing — and a manifest built from
// that config would have admitted the wrong destination. Overrides now land
// here, before anything reads the config, and nothing later re-resolves a
// different URL.
//
// Destination validation intentionally tightens acceptance relative to
// config.Load, which checks only scheme and host: a base URL carrying
// userinfo, a query, or a fragment loads today but is rejected here with a
// typed error naming the provider — before any client is constructed, and
// without echoing the value, which may embed a credential.
func materializeEffectiveConfig(cfg *config.Config, ollamaOverride, ocOverrideProvider, ocOverrideURL string) (*effectiveProviders, error) {
	var effective *config.Config
	if cfg == nil {
		url := ollamaOverride
		if url == "" {
			url = defaultOllamaURL
		}
		effective = &config.Config{Providers: map[string]config.ProviderConfig{
			"ollama": {BaseURL: url, APIFormat: "ollama"},
		}}
	} else {
		if len(cfg.Providers) == 0 {
			return nil, fmt.Errorf("providerbootstrap: no providers configured")
		}
		// Always copy: overrides and api_format normalization write into
		// the provider map, and the caller's config must stay theirs.
		cp := *cfg
		cp.Providers = make(map[string]config.ProviderConfig, len(cfg.Providers))
		maps.Copy(cp.Providers, cfg.Providers)
		effective = &cp
	}

	if (ocOverrideProvider == "") != (ocOverrideURL == "") {
		return nil, fmt.Errorf("providerbootstrap: OpenAICompatURLOverrideProvider and OpenAICompatURLOverride must be set together")
	}

	// Normalize the defaulted api_format on the copy so every consumer of
	// the effective config sees the value the constructed client acts on.
	for key, pc := range effective.Providers {
		if pc.APIFormat == "" {
			pc.APIFormat = "ollama"
			effective.Providers[key] = pc
		}
	}

	if ocOverrideProvider != "" {
		pc, ok := effective.Providers[ocOverrideProvider]
		if !ok {
			return nil, fmt.Errorf("providerbootstrap: openai-compat URL override: unknown provider %q", ocOverrideProvider)
		}
		if pc.APIFormat != "openai-compat" {
			return nil, fmt.Errorf("providerbootstrap: openai-compat URL override: provider %q has api_format %q, want \"openai-compat\"", ocOverrideProvider, pc.APIFormat)
		}
		pc.BaseURL = ocOverrideURL
		effective.Providers[ocOverrideProvider] = pc
	}

	// Explicit override pins the ollama base URL (mirrors mcp WithOllamaURL):
	// only a provider NAMED "ollama" speaking the ollama format. Applied to
	// the effective config, never just the client (the F10 fix).
	if ollamaOverride != "" && cfg != nil {
		if pc, ok := effective.Providers["ollama"]; ok && pc.APIFormat == "ollama" {
			pc.BaseURL = ollamaOverride
			effective.Providers["ollama"] = pc
		}
	}

	dests := make(map[string]provider.Destination, len(effective.Providers))
	for key, pc := range effective.Providers {
		if err := config.ValidateProviderName(key); err != nil {
			return nil, fmt.Errorf("providerbootstrap: provider config: %w", err)
		}
		d, err := provider.NewDestination(key, pc.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("providerbootstrap: %w", err)
		}
		dests[key] = d
	}
	return &effectiveProviders{cfg: effective, dests: dests}, nil
}

// sortedProviderKeys returns the effective provider keys in deterministic
// order for construction and rendering.
func (e *effectiveProviders) sortedProviderKeys() []string {
	keys := make([]string, 0, len(e.cfg.Providers))
	for key := range e.cfg.Providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
