// config/capabilities.go
//
// The capability policy of one provider/model selector (#736): what models.json
// says about its capabilities, read exactly as bootstrap's override and floor
// builders read it, for hosts that explain capabilities without a registry.
package config

import (
	"fmt"
	"slices"
	"sort"

	"github.com/kstruzzieri/go-llm/provider"
)

// ModelCapabilityPolicy is what the configuration says about one
// provider/model selector's capabilities.
type ModelCapabilityPolicy struct {
	Authored  bool                // some role names this provider/model
	Explicit  []string            // the explicit list; nil when no role declares one
	Derived   []string            // sorted union of ResolvedCapabilities of matching roles without an explicit list, any api_format
	TypeNames []string            // sorted, deduplicated Type of those roles
	TypeCaps  provider.Capability // Derived parsed strictly when the provider's api_format is openai-compat; else 0
}

// CapabilityPolicy returns the capability policy of the selector providerName
// and model. A role matches when its provider and name are both nonempty and
// equal the arguments exactly: there is no Ollama alias fallback, so a role
// naming qwen3 says nothing about qwen3:latest. Roles are walked in sorted
// order. Every matching role's explicit list must parse strictly, and two lists
// conflict only when their parsed masks differ; either failure is an error, as
// it is in bootstrap. Explicit is the first nonempty list; roles without one
// contribute Derived and TypeNames, and TypeCaps when the provider speaks
// openai-compat (the floor bootstrap installs). Every slice is a copy. A nil
// Config has no policy.
func (c *Config) CapabilityPolicy(providerName, model string) (ModelCapabilityPolicy, error) {
	var p ModelCapabilityPolicy
	if c == nil {
		return p, nil
	}
	roles := make([]string, 0, len(c.Models))
	for role := range c.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	explicitRole, explicitCaps := "", provider.Capability(0)
	for _, role := range roles {
		m := c.Models[role]
		if m.Provider == "" || m.Name == "" || m.Provider != providerName || m.Name != model {
			continue
		}
		p.Authored = true
		if len(m.Capabilities) == 0 {
			p.Derived = append(p.Derived, m.ResolvedCapabilities()...)
			if m.Type != "" {
				p.TypeNames = append(p.TypeNames, m.Type)
			}
			continue
		}
		caps, err := provider.ParseCapsStrict(m.Capabilities)
		if err != nil {
			return ModelCapabilityPolicy{}, fmt.Errorf("config: model %q capability override for %s/%s: %w", role, providerName, model, err)
		}
		if p.Explicit == nil {
			p.Explicit, explicitRole, explicitCaps = slices.Clone(m.Capabilities), role, caps
			continue
		}
		if caps != explicitCaps {
			return ModelCapabilityPolicy{}, fmt.Errorf("config: conflicting capability overrides for %s/%s: models %q and %q", providerName, model, explicitRole, role)
		}
	}
	slices.Sort(p.Derived)
	p.Derived = slices.Compact(p.Derived)
	slices.Sort(p.TypeNames)
	p.TypeNames = slices.Compact(p.TypeNames)
	if c.Providers[providerName].APIFormat == "openai-compat" {
		caps, err := provider.ParseCapsStrict(p.Derived)
		if err != nil {
			return ModelCapabilityPolicy{}, fmt.Errorf("config: derived capabilities for %s/%s: %w", providerName, model, err)
		}
		p.TypeCaps = caps
	}
	return p, nil
}
