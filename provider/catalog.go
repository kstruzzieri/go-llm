// provider/catalog.go
package provider

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed catalog.json
var catalogJSON []byte

// catalogData is the top-level JSON structure for the embedded model catalog.
type catalogData struct {
	Families map[string]catalogFamily `json:"families"`
}

// catalogFamily is the JSON representation of a model family entry.
// Each family defines shared policy defaults (FIM stops/budget, think mode,
// context window) and a map of size-keyed variants with resource requirements.
type catalogFamily struct {
	FIM           *FIMConfig                `json:"fim"`
	ThinkMode     string                    `json:"think_mode"`
	ThinkTags     *catalogThinkTags         `json:"think_tags"`
	Caps          []string                  `json:"caps"`
	ContextWindow int                       `json:"context_window"`
	Dimensions    int                       `json:"dimensions"`
	Variants      map[string]catalogVariant `json:"variants"`
}

// catalogThinkTags is the JSON representation of thinking delimiters.
type catalogThinkTags struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

// catalogVariant is the JSON representation of a specific model size variant.
type catalogVariant struct {
	RAMRequired     float64 `json:"ram_required"`
	RAMRecommended  float64 `json:"ram_recommended"`
	VRAMRecommended float64 `json:"vram_recommended"`
	Quality         string  `json:"quality"`
	Speed           string  `json:"speed"`
}

// staticCatalog provides lookup methods over the embedded catalog data.
// It is created once at startup from the parsed catalog.json and used by
// ModelRegistry for the static layer of three-layer profile merge.
type staticCatalog struct {
	data catalogData
}

// loadCatalog parses the embedded catalog.json into a catalogData structure.
func loadCatalog() (*catalogData, error) {
	var data catalogData
	if err := json.Unmarshal(catalogJSON, &data); err != nil {
		return nil, fmt.Errorf("provider: load catalog: %w", err)
	}
	return &data, nil
}

// newStaticCatalog creates a staticCatalog from parsed catalog data.
func newStaticCatalog(data *catalogData) *staticCatalog {
	return &staticCatalog{data: *data}
}

// lookup returns a ModelProfile for an exact family + param size match.
// Returns nil if the family or variant is not found in the catalog.
func (sc *staticCatalog) lookup(family, paramSize string) *ModelProfile {
	family = strings.ToLower(family)
	paramSize = strings.ToLower(paramSize)

	fam, ok := sc.data.Families[family]
	if !ok {
		return nil
	}

	variant, ok := fam.Variants[paramSize]
	if !ok {
		return nil
	}

	return sc.buildProfile(family, &fam, &variant)
}

// lookupFamily returns a ModelProfile with family-level metadata (FIM policy,
// think mode, context window) but without variant-specific resource data.
// Returns nil if the family is not found in the catalog.
func (sc *staticCatalog) lookupFamily(family string) *ModelProfile {
	family = strings.ToLower(family)

	fam, ok := sc.data.Families[family]
	if !ok {
		return nil
	}

	return sc.buildProfile(family, &fam, nil)
}

// families returns all family names present in the catalog.
func (sc *staticCatalog) families() []string {
	names := make([]string, 0, len(sc.data.Families))
	for name := range sc.data.Families {
		names = append(names, name)
	}
	return names
}

// buildProfile constructs a ModelProfile from catalog data. If variant is nil,
// only family-level data is populated and resource fields remain zero-valued.
func (sc *staticCatalog) buildProfile(family string, fam *catalogFamily, variant *catalogVariant) *ModelProfile {
	p := &ModelProfile{
		Family:        family,
		ThinkMode:     parseThinkMode(fam.ThinkMode),
		Caps:          parseCaps(fam.Caps),
		ContextWindow: fam.ContextWindow,
		Dimensions:    fam.Dimensions,
		Source:        SourceStatic,
	}

	if fam.FIM != nil {
		p.FIM = fam.FIM
	}

	if fam.ThinkTags != nil {
		p.ThinkTags = &ThinkTags{
			Open:  fam.ThinkTags.Open,
			Close: fam.ThinkTags.Close,
		}
	}

	if variant != nil {
		p.Resources = ResourceProfile{
			RAMRequired:     variant.RAMRequired,
			RAMRecommended:  variant.RAMRecommended,
			VRAMRecommended: variant.VRAMRecommended,
		}
		p.Quality = parseTier(variant.Quality)
		p.Speed = parseTier(variant.Speed)
	}

	return p
}

// parseThinkMode converts a string think_mode value from JSON to ThinkMode.
func parseThinkMode(s string) ThinkMode {
	switch strings.ToLower(s) {
	case "none":
		return ThinkNone
	case "always":
		return ThinkAlways
	case "toggle":
		return ThinkToggle
	case "auto":
		return ThinkAuto
	default:
		return ThinkNone
	}
}

// parseTier converts a string tier name to a Tier value.
// Supports both quality names (basic, good, great, best) and speed aliases
// (fast, medium, slow) so that catalog JSON can use descriptive labels.
func parseTier(s string) Tier {
	switch strings.ToLower(s) {
	case "basic":
		return TierBasic
	case "good":
		return TierGood
	case "great":
		return TierGreat
	case "best":
		return TierBest
	// Speed tier aliases: fast=great throughput, medium=good, slow=basic.
	case "fast":
		return TierGreat
	case "medium":
		return TierGood
	case "slow":
		return TierBasic
	default:
		return TierBasic
	}
}

// CanonicalCapabilityNames is the user-facing capability vocabulary: each
// token maps to exactly one Capability bit. Aliases like "completion" that
// expand to multiple bits are NOT in this set — they remain valid only in
// catalog.json and upstream metadata sources (handled by parseCaps).
//
// Exported so config validation in adjacent packages can use this list as
// the single source of truth for "valid user-config capability tokens"
// rather than maintaining a parallel set that could drift.
var CanonicalCapabilityNames = []string{
	"chat",
	"generate",
	"stream",
	"embed",
	"tool_call",
	"thinking",
	"insert",
}

// canonicalCapability maps a single canonical token to its single-bit
// contribution. Used by ParseCapsStrict for user-config validation where
// each token must mean exactly one bit and aliases are forbidden so the
// "explicit Capabilities REPLACE derived" contract holds without surprise
// expansion.
//
// Catalog aliases that expand multiple bits (e.g. "insert" -> generate+
// stream+insert) are handled by aliasedCapability, used only by parseCaps
// for catalog.json and runtime /api/show output.
func canonicalCapability(token string) (Capability, bool) {
	switch strings.ToLower(token) {
	case "chat":
		return CapChat, true
	case "generate":
		return CapGenerate, true
	case "stream":
		return CapStream, true
	case "embed":
		return CapEmbed, true
	case "tool_call":
		return CapToolCall, true
	case "thinking":
		return CapThinking, true
	case "insert":
		return CapInsert, true
	}
	return 0, false
}

// aliasedCapability maps catalog/runtime tokens (including aliases that
// expand multiple bits) to their bitmask contribution. NOT used for
// user-config — see canonicalCapability and ParseCapsStrict for that path.
//
//	"completion" -> CapChat | CapGenerate | CapStream
//	"insert"     -> CapGenerate | CapStream | CapInsert (full FIM path)
//	"tools"      -> CapToolCall
//	"embedding"  -> CapEmbed
//	"thinking"   -> CapThinking
//
// All canonical names are also accepted so catalog.json may use either
// shorthand or canonical form interchangeably.
func aliasedCapability(token string) (Capability, bool) {
	switch strings.ToLower(token) {
	// Canonical single-bit names (also valid in catalog/runtime input).
	case "chat":
		return CapChat, true
	case "generate":
		return CapGenerate, true
	case "stream":
		return CapStream, true
	case "embed":
		return CapEmbed, true
	case "tool_call":
		return CapToolCall, true
	case "thinking":
		return CapThinking, true
	// Catalog aliases — multi-bit expansion is intentional here because
	// these are author-controlled shorthand strings in catalog.json,
	// fingerprint records, and Ollama /api/show capability arrays.
	case "insert":
		return CapGenerate | CapStream | CapInsert, true
	case "completion":
		return CapChat | CapGenerate | CapStream, true
	case "tools":
		return CapToolCall, true
	case "embedding":
		return CapEmbed, true
	}
	return 0, false
}

// parseCaps converts a string slice of capability names to a bitmask,
// silently skipping unknown tokens. Suitable for upstream metadata sources
// (catalog.json, fingerprint cache, provider /api/show) where graceful
// degradation across schema versions is the right behavior. Catalog
// aliases that expand multiple bits are honored here.
//
// User config MUST use ParseCapsStrict instead: aliases are forbidden in
// user-facing config so the "explicit Capabilities REPLACE derived"
// contract holds without surprise expansion.
func parseCaps(caps []string) Capability {
	var c Capability
	for _, s := range caps {
		if bit, ok := aliasedCapability(s); ok {
			c |= bit
		}
	}
	return c
}

// aliasSuggestions maps known catalog/runtime aliases that ParseCapsStrict
// rejects to a "did you mean" replacement in canonical user-config form.
// Used to make the rejection error actionable when the user typed a
// recognizable shorthand (the most common mistake) rather than dumping
// the full canonical vocabulary and leaving them to guess.
//
// Tokens not in this map fall back to the canonical-names list in the
// error message. "insert" is NOT here because it is itself canonical —
// catalog parseCaps expands it to generate+stream+insert, but strict
// parsing accepts it as the single CapInsert bit, which is the right
// answer for user config.
var aliasSuggestions = map[string]string{
	"tools":      `"tool_call"`,
	"embedding":  `"embed"`,
	"completion": `"chat", "generate", and/or "stream" (was a multi-bit alias; pick the bits you actually want)`,
}

// ParseCapsStrict converts a slice of canonical-only capability tokens to
// a bitmask. Each token must be a name from CanonicalCapabilityNames; any
// non-canonical token is rejected — including aliases that would expand
// to multiple bits ("completion" -> chat+generate+stream, the catalog
// shorthand "insert" -> generate+stream+insert) AND single-bit aliases
// that merely shadow a canonical name ("tools" is rejected because the
// canonical spelling is "tool_call"). Intended for validating
// user-authored capability lists in models.json where any non-canonical
// token would risk silent bit-set divergence from what the user wrote.
//
// Rejection errors include a "did you mean" hint when the rejected token
// is a known alias (e.g. "tools" -> suggest "tool_call"); otherwise they
// fall back to listing the canonical vocabulary.
func ParseCapsStrict(caps []string) (Capability, error) {
	var c Capability
	for _, s := range caps {
		bit, ok := canonicalCapability(s)
		if !ok {
			if hint, known := aliasSuggestions[strings.ToLower(s)]; known {
				return 0, fmt.Errorf("provider: unknown or non-canonical capability %q (did you mean %s?)", s, hint)
			}
			return 0, fmt.Errorf("provider: unknown or non-canonical capability %q (canonical names: %v)", s, CanonicalCapabilityNames)
		}
		c |= bit
	}
	return c, nil
}
