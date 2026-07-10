package providerbootstrap

import (
	"fmt"
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

type modelDefaultsEntry struct {
	role string
	opts provider.SamplingDefaults
}

func buildModelDefaults(cfg *config.Config) (map[provider.ModelKey]provider.SamplingDefaults, error) {
	out := make(map[provider.ModelKey]provider.SamplingDefaults)
	if cfg == nil {
		return out, nil
	}
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	seen := make(map[provider.ModelKey]modelDefaultsEntry)
	for _, role := range roles {
		model := cfg.Models[role]
		if model.Provider == "" || model.Name == "" || model.Options == nil {
			continue
		}
		opts := provider.SamplingDefaults{
			Temperature: model.Options.Temperature,
			TopP:        model.Options.TopP,
			TopK:        model.Options.TopK,
		}
		key := provider.ModelKey{Provider: model.Provider, Model: model.Name}
		if existing, ok := seen[key]; ok && !sameModelDefaults(existing.opts, opts) {
			return nil, fmt.Errorf(
				"providerbootstrap: conflicting sampling defaults for %s: models %q and %q; defaults are per provider/model, so use identical options or distinct provider keys for workload-specific defaults",
				key, existing.role, role,
			)
		}
		seen[key] = modelDefaultsEntry{role: role, opts: opts}
		out[key] = opts
	}
	return out, nil
}

func sameModelDefaults(a, b provider.SamplingDefaults) bool {
	return samePtr(a.Temperature, b.Temperature) && samePtr(a.TopP, b.TopP) && samePtr(a.TopK, b.TopK)
}

func samePtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

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
			continue // derived caps are floors now (buildCapabilityFloors), not overrides
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

// buildCapabilityFloors computes baseline capability floors for undeclared
// openai-compat models: their provider exposes no capability metadata, so
// the type-derived set guarantees routability. Floors OR-merge in the
// registry (lowest precedence), so unlike the old REPLACE override they
// cannot erase catalog/fingerprint/runtime capabilities (issue #219 root
// cause). Shared keys across roles union their derived sets — OR semantics
// make conflicts impossible.
func buildCapabilityFloors(cfg *config.Config) (map[provider.ModelKey][]string, error) {
	out := make(map[provider.ModelKey][]string)
	if cfg == nil {
		return out, nil
	}
	union := make(map[provider.ModelKey]map[string]bool)
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		m := cfg.Models[role]
		if m.Provider == "" || m.Name == "" || len(m.Capabilities) > 0 {
			continue // explicit declarations are overrides, not floors
		}
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
		derived := m.ResolvedCapabilities()
		if len(derived) == 0 {
			continue
		}
		if _, err := provider.ParseCapsStrict(derived); err != nil {
			return nil, fmt.Errorf("providerbootstrap: model %q derived floor for %s/%s: %w", role, m.Provider, m.Name, err)
		}
		key := provider.ModelKey{Provider: m.Provider, Model: m.Name}
		if union[key] == nil {
			union[key] = make(map[string]bool)
		}
		for _, tok := range derived {
			union[key][tok] = true
		}
	}
	for key, set := range union {
		toks := make([]string, 0, len(set))
		for tok := range set {
			toks = append(toks, tok)
		}
		sort.Strings(toks)
		out[key] = toks
	}
	return out, nil
}

// installCapabilityFloors installs the computed floors onto the registry.
// No-op when mr/cfg is nil or there are no floors.
func installCapabilityFloors(mr *provider.ModelRegistry, cfg *config.Config) error {
	if mr == nil || cfg == nil {
		return nil
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		return err
	}
	if len(floors) == 0 {
		return nil
	}
	mr.SetCapabilityFloor(func(key provider.ModelKey) []string {
		caps, ok := floors[key]
		if !ok {
			return nil
		}
		out := make([]string, len(caps))
		copy(out, caps)
		return out
	})
	return nil
}

// thinkOverrideEntry holds the per-key think override plus the role that
// declared each field, so same-key conflicts can name both roles.
type thinkOverrideEntry struct {
	modeRole string
	tagsRole string
	mode     *provider.ThinkMode
	tags     *provider.ThinkTags
}

// buildThinkOverrides computes the think_mode/think_tags overrides declared in
// models.json, keyed by provider.ModelKey. Config.Models is keyed by role, so
// multiple roles may share a provider/model key: identical re-declarations are
// fine, complementary fields combine (one role mode-only, another tags-only),
// and conflicting declarations error naming both roles — mirroring
// buildCapabilityOverrides. Pure (no registry side effect) so it is directly
// unit-testable.
func buildThinkOverrides(cfg *config.Config) (map[provider.ModelKey]thinkOverrideEntry, error) {
	out := make(map[provider.ModelKey]thinkOverrideEntry)
	if cfg == nil {
		return out, nil
	}
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
		if m.ThinkMode == "" && m.ThinkTags == nil {
			continue
		}
		key := provider.ModelKey{Provider: m.Provider, Model: m.Name}
		entry := out[key]
		if m.ThinkMode != "" {
			// config.Load already validated this, but programmatic Config
			// callers bypass Load — stay fail-loud here too.
			mode, err := provider.ParseThinkModeStrict(m.ThinkMode)
			if err != nil {
				return nil, fmt.Errorf("providerbootstrap: model %q think_mode for %s/%s: %w", role, key.Provider, key.Model, err)
			}
			switch {
			case entry.mode == nil:
				entry.mode = &mode
				entry.modeRole = role
			case *entry.mode != mode:
				return nil, fmt.Errorf(
					"providerbootstrap: conflicting think_mode for %s/%s: model %q declares %q, model %q declares %q",
					key.Provider, key.Model, entry.modeRole, entry.mode.String(), role, mode.String(),
				)
			}
		}
		if tt := m.ThinkTags; tt != nil {
			tags := provider.ThinkTags{Open: tt.Open, Close: tt.Close}
			switch {
			case entry.tags == nil:
				entry.tags = &tags
				entry.tagsRole = role
			case *entry.tags != tags:
				return nil, fmt.Errorf(
					"providerbootstrap: conflicting think_tags for %s/%s: model %q declares %q/%q, model %q declares %q/%q",
					key.Provider, key.Model, entry.tagsRole, entry.tags.Open, entry.tags.Close, role, tags.Open, tags.Close,
				)
			}
		}
		out[key] = entry
	}
	return out, nil
}

// installThinkOverrides installs the computed think overrides onto the
// registry. No-op when mr/cfg is nil or there are no overrides.
func installThinkOverrides(mr *provider.ModelRegistry, cfg *config.Config) error {
	if mr == nil || cfg == nil {
		return nil
	}
	overrides, err := buildThinkOverrides(cfg)
	if err != nil {
		return err
	}
	if len(overrides) == 0 {
		return nil
	}
	mr.SetThinkOverride(func(key provider.ModelKey) (*provider.ThinkMode, *provider.ThinkTags) {
		entry, ok := overrides[key]
		if !ok {
			return nil, nil
		}
		// Return copies so hook callers can never alias the map's memory
		// (the registry merge also copies — defense in depth).
		var mode *provider.ThinkMode
		if entry.mode != nil {
			m := *entry.mode
			mode = &m
		}
		var tags *provider.ThinkTags
		if entry.tags != nil {
			tg := *entry.tags
			tags = &tg
		}
		return mode, tags
	})
	return nil
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
