// Package-local file implementing Section 6 of the provider-intelligence
// design: four-layer adaptive FIM context budgeting.
//
// Layer 1: family base split (prefix percentage)
// Layer 2: language adjustment (additive modifier)
// Layer 3: position-aware rebalancing when one side is shorter than budget
// Layer 4: learned splits — not implemented here; a FIMBenchmarkSource
//
//	interface is reserved for a later phase.
package compat

import "strings"

// BudgetResult reports how many tokens of prefix and suffix to send.
type BudgetResult struct {
	Prefix int
	Suffix int
}

// familyBase maps normalized family to base prefix percentage.
var familyBase = map[string]int{
	"qwen3":     75,
	"qwen2.5":   75,
	"qwen3.5":   75,
	"codellama": 65,
	"deepseek":  70,
	"starcoder": 70,
	"phi-4":     75,
}

// languageDelta maps language to additive modifier.
var languageDelta = map[string]int{
	"python":     -10,
	"yaml":       -10,
	"go":         +5,
	"rust":       +5,
	"typescript": -5,
	"sql":        -5,
}

// fimFamily normalizes a family string to the familyBase key.
func fimFamily(family string) string {
	f := strings.ToLower(strings.TrimSpace(family))
	if f == "" {
		return "unknown"
	}
	for key := range familyBase {
		if strings.HasPrefix(f, key) {
			return key
		}
	}
	return "unknown"
}

// baseSplit returns the family base prefix pct, defaulting to 70.
func baseSplit(family string) int {
	if pct, ok := familyBase[family]; ok {
		return pct
	}
	return 70
}

// languageAdjust returns the additive modifier for a language, 0 if unknown.
func languageAdjust(lang string) int {
	return languageDelta[strings.ToLower(strings.TrimSpace(lang))]
}

// clampPct keeps the prefix percentage in [40, 90] so neither side drops below 10%.
func clampPct(pct int) int {
	if pct < 40 {
		return 40
	}
	if pct > 90 {
		return 90
	}
	return pct
}

// adaptiveSplit allocates the maxTokens budget between prefix and suffix
// based on the configured prefix percentage, then rebalances when one side's
// actual content is shorter than its budget share.
func adaptiveSplit(prefixLen, suffixLen, prefixPct, maxTokens int) BudgetResult {
	prefixBudget := (maxTokens * prefixPct) / 100
	suffixBudget := maxTokens - prefixBudget

	// Layer 3: surplus transfer.
	result := BudgetResult{}
	if prefixLen <= prefixBudget {
		surplus := prefixBudget - prefixLen
		result.Prefix = prefixLen
		suffixBudget += surplus
	} else {
		result.Prefix = prefixBudget
	}
	if suffixLen <= suffixBudget {
		result.Suffix = suffixLen
	} else {
		result.Suffix = suffixBudget
	}
	return result
}

// budgetForProfile is the single entry point used by the completions handler.
// It combines Layer 1 (family) and Layer 2 (language) into a prefix pct, then
// delegates to adaptiveSplit. PrefixBudgetPct from the profile's FIMConfig
// overrides the family default when non-zero.
//
// Compose order: when overridePrefixPct > 0, it REPLACES the family default
// BEFORE the language adjustment is applied. The final percentage is then
// passed through clampPct so it stays in [40, 90].
func budgetForProfile(family, language string, overridePrefixPct int, prefixLen, suffixLen, maxTokens int) BudgetResult {
	pct := baseSplit(fimFamily(family))
	if overridePrefixPct > 0 {
		pct = overridePrefixPct
	}
	pct = clampPct(pct + languageAdjust(language))
	return adaptiveSplit(prefixLen, suffixLen, pct, maxTokens)
}
