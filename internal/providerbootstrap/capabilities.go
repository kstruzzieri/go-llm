package providerbootstrap

import (
	"fmt"
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// capabilitiesForKey returns the configured capabilities for a model key:
// explicit Capabilities when set, else ResolvedCapabilities. Nil when unknown.
func capabilitiesForKey(cfg *config.Config, key provider.ModelKey) []string {
	if cfg == nil {
		return nil
	}
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		m := cfg.Models[role]
		if m.Provider != key.Provider || m.Name != key.Model {
			continue
		}
		if len(m.Capabilities) > 0 {
			out := make([]string, len(m.Capabilities))
			copy(out, m.Capabilities)
			return out
		}
		return m.ResolvedCapabilities()
	}
	return nil
}

// capabilityOverrideEntry holds the per-key data used for conflict detection
// inside buildCapabilityOverrides.
type capabilityOverrideEntry struct {
	role   string
	caps   []string
	parsed provider.Capability
}

// buildCapabilityOverrides computes the capability overrides declared in
// models.json, keyed by provider.ModelKey. It errors on conflicting
// declarations for the same key. Returns an empty (non-nil) map when cfg is
// nil or declares none. Pure (no registry side effect) so it is directly
// unit-testable.
func buildCapabilityOverrides(cfg *config.Config) (map[provider.ModelKey][]string, error) {
	out := make(map[provider.ModelKey][]string)
	if cfg == nil {
		return out, nil
	}

	overrides := make(map[provider.ModelKey]capabilityOverrideEntry)
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		m := cfg.Models[role]
		if m.Provider == "" || m.Name == "" {
			continue
		}
		caps := m.Capabilities
		if len(caps) == 0 {
			pCfg, ok := cfg.Providers[m.Provider]
			if !ok {
				continue
			}
			apiFormat := pCfg.APIFormat
			if apiFormat == "" {
				apiFormat = "ollama"
			}
			if apiFormat != "openai-compat" {
				continue
			}
			caps = m.ResolvedCapabilities()
		}
		if len(caps) == 0 {
			continue
		}
		parsedCaps, err := provider.ParseCapsStrict(caps)
		if err != nil {
			return nil, fmt.Errorf("providerbootstrap: model %q capability override for %s/%s: %w", role, m.Provider, m.Name, err)
		}

		key := provider.ModelKey{Provider: m.Provider, Model: m.Name}
		if existing, ok := overrides[key]; ok {
			if existing.parsed != parsedCaps {
				return nil, fmt.Errorf(
					"providerbootstrap: conflicting capability overrides for %s/%s: model %q declares %v, model %q declares %v",
					key.Provider,
					key.Model,
					existing.role,
					existing.caps,
					role,
					caps,
				)
			}
			continue
		}

		copied := make([]string, len(caps))
		copy(copied, caps)
		overrides[key] = capabilityOverrideEntry{
			role:   role,
			caps:   copied,
			parsed: parsedCaps,
		}
	}

	for key, entry := range overrides {
		out[key] = entry.caps
	}
	return out, nil
}

// installCapabilityOverrides installs the computed overrides onto the registry.
// No-op when mr/cfg is nil or there are no overrides.
func installCapabilityOverrides(mr *provider.ModelRegistry, cfg *config.Config) error {
	if mr == nil || cfg == nil {
		return nil
	}
	overrides, err := buildCapabilityOverrides(cfg)
	if err != nil {
		return err
	}
	if len(overrides) == 0 {
		return nil
	}
	mr.SetCapabilityOverride(func(key provider.ModelKey) []string {
		caps, ok := overrides[key]
		if !ok {
			return nil
		}
		out := make([]string, len(caps))
		copy(out, caps)
		return out
	})
	return nil
}
