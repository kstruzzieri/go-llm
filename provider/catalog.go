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

// capabilityFor maps a single capability token to its bitmask contribution.
// It accepts both the canonical lowercase names matching Capability.String()
// output ("chat", "generate", "stream", "embed", "tool_call", "thinking",
// "insert") and the higher-level catalog aliases used by catalog.json:
//
//	"completion" -> CapChat | CapGenerate | CapStream
//	"insert"     -> CapGenerate | CapStream | CapInsert
//	"tools"      -> CapToolCall
//	"embedding"  -> CapEmbed
//	"thinking"   -> CapThinking
//
// Tokens are lowercased before matching. The boolean return reports whether
// the token was recognized; callers decide whether unknowns are an error
// (user config) or a silent skip (upstream metadata sources).
func capabilityFor(token string) (Capability, bool) {
	switch strings.ToLower(token) {
	// Canonical single-bit names.
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
		// "insert" intentionally implies generate+stream+insert because
		// FIM consumers expect the full path; standalone CapInsert without
		// generate/stream is not a useful state in this codebase.
		return CapGenerate | CapStream | CapInsert, true
	// Catalog aliases retained for compatibility with catalog.json and
	// upstream caps strings emitted by fingerprint / Ollama /api/show.
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
// degradation across schema versions is the right behavior. User config
// should use ParseCapsStrict instead so typos surface at load time.
func parseCaps(caps []string) Capability {
	var c Capability
	for _, s := range caps {
		if bit, ok := capabilityFor(s); ok {
			c |= bit
		}
	}
	return c
}

// ParseCapsStrict is the strict variant of parseCaps: it returns an error on
// the first unknown token. Intended for validating user-authored capability
// lists in models.json where a silent skip would hide configuration mistakes.
func ParseCapsStrict(caps []string) (Capability, error) {
	var c Capability
	for _, s := range caps {
		bit, ok := capabilityFor(s)
		if !ok {
			return 0, fmt.Errorf("provider: unknown capability %q", s)
		}
		c |= bit
	}
	return c, nil
}
