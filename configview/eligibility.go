package configview

import (
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// capBitByName maps canonical capability tokens to their bits. The
// TestCapBitByNameCoversCanonical tripwire pins it to
// provider.CanonicalCapabilityNames.
var capBitByName = map[string]provider.Capability{
	"chat":      provider.CapChat,
	"generate":  provider.CapGenerate,
	"insert":    provider.CapInsert,
	"embed":     provider.CapEmbed,
	"stream":    provider.CapStream,
	"tool_call": provider.CapToolCall,
	"thinking":  provider.CapThinking,
}

// eligibilityFor evaluates required bits against known capabilities. A set
// Caps bit counts as known regardless of KnownMask. Any definitive missing
// bit → ineligible; else any unknown bit → unknown; else eligible.
// req == 0 (no consumer-declared shape) → unknown, reason no_requirements.
func eligibilityFor(req, caps, known provider.Capability) (Eligibility, []string) {
	if req == 0 {
		return EligibilityUnknown, []string{"no_requirements"}
	}
	known |= caps
	var reasons []string
	result := EligibilityEligible
	for _, name := range req.Names() {
		bit, ok := capBitByName[name]
		if !ok {
			continue
		}
		switch {
		case caps.Has(bit):
		case known.Has(bit):
			reasons = append(reasons, "missing_capability:"+name)
			result = EligibilityIneligible
		default:
			reasons = append(reasons, "capability_unknown:"+name)
			if result != EligibilityIneligible {
				result = EligibilityUnknown
			}
		}
	}
	sort.Strings(reasons)
	if result == EligibilityEligible {
		reasons = nil
	}
	return result, reasons
}

// capsFor resolves a selector's (caps, knownMask). An inventory entry wins —
// the registry is the merged authority (catalog + fingerprint + runtime +
// overrides). Config-only selectors split on authority: EXPLICIT
// Capabilities entries REPLACE the derived set (config semantics), so their
// omissions are DEFINITIVE — the known mask covers every canonical bit.
// Type-derived caps are present-only knowledge: derivation can never assert
// tool_call absence (the #219 trap), so absent bits stay unknown.
func capsFor(sel string, cfg *config.Config, inv map[string]InventoryModel) (provider.Capability, provider.Capability) {
	if m, ok := inv[sel]; ok {
		return m.Caps, m.KnownMask
	}
	mc, ok := configModelBySelector(cfg, sel)
	if !ok {
		return 0, 0
	}
	caps, err := provider.ParseCapsStrict(mc.ResolvedCapabilities())
	if err != nil {
		// Unparseable declared caps: claim nothing, know nothing.
		return 0, 0
	}
	if len(mc.Capabilities) > 0 {
		return caps, allCanonicalBits()
	}
	return caps, caps
}

// configModelBySelector finds a config model entry by provider/name selector.
// Role iteration is sorted so a multi-role selector resolves deterministically.
func configModelBySelector(cfg *config.Config, sel string) (config.ModelConfig, bool) {
	for _, role := range sortedKeys(cfg.Models) {
		m := cfg.Models[role]
		if m.Provider+"/"+m.Name == sel {
			return m, true
		}
	}
	return config.ModelConfig{}, false
}

func allCanonicalBits() provider.Capability {
	var all provider.Capability
	for _, bit := range capBitByName {
		all |= bit
	}
	return all
}
