package configview

import (
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// eligibilityFor delegates to the single shared evaluator
// provider.EvaluateCaps — evaluation semantics (tri-state, bounded sorted
// reasons, non-canonical-bit handling) have exactly one implementation.
func eligibilityFor(req, caps, known provider.Capability) (Eligibility, []string) {
	v, reasons := provider.EvaluateCaps(req, caps, known)
	return Eligibility(v), reasons
}

// capsFor resolves a selector's (caps, knownMask). EXPLICIT Capabilities
// entries REPLACE every discovered set (config semantics), so their omissions
// are DEFINITIVE. Type-derived caps are present-only knowledge: derivation can
// never assert tool_call absence (the #219 trap), so absent bits stay unknown.
func capsFor(sel string, cfg *config.Config, inv map[string]InventoryModel) (provider.Capability, provider.Capability) {
	for _, role := range sortedKeys(cfg.Models) {
		mc := cfg.Models[role]
		if mc.Provider+"/"+mc.Name != sel || len(mc.Capabilities) == 0 {
			continue
		}
		caps, err := provider.ParseCapsStrict(mc.Capabilities)
		if err != nil {
			return 0, 0
		}
		return caps, allCanonicalBits()
	}
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

// configQualityCtxCeiling returns the first nonzero quality_ctx_ceiling across
// every role on sel, in sorted role order. Zero means unset and Load rejects
// different nonzero values on one selector, so any role that sets it speaks
// for the whole selector, as provider bootstrap installs it.
func configQualityCtxCeiling(cfg *config.Config, sel string) int {
	for _, role := range sortedKeys(cfg.Models) {
		m := cfg.Models[role]
		if m.Provider+"/"+m.Name == sel && m.QualityCtxCeiling != 0 {
			return m.QualityCtxCeiling
		}
	}
	return 0
}

func allCanonicalBits() provider.Capability {
	return provider.CanonicalCaps()
}
