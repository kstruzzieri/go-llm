// provider/model_key.go
package provider

import (
	"regexp"
	"strings"
)

// ParsedModel holds the decomposed components of an Ollama model name.
// Model names follow the pattern: family[-variant]:paramSize[-variant][-quant]
//
// This is a best-effort parser for catalog matching, not a strict validator.
// Unknown or ambiguous names are parsed conservatively: unrecognized parts
// are left in the Variant field so callers always get a usable Family.
type ParsedModel struct {
	Raw       string // "qwen3.5:35b-a3b-q4_K_M"
	Family    string // "qwen3.5"
	Variant   string // "a3b" (MoE active params), "coder", "instruct", etc.
	ParamSize string // "35b", "8x7b"
	Quant     string // "q4_k_m" (lowercased)
	Tag       string // "latest" if unspecified
}

// knownVariantSuffixes are variant identifiers that appear as suffixes on the
// family portion of the model name (before the colon), e.g. "qwen2.5-coder".
// These are checked after compound family matching, so "deepseek-coder-v2"
// will not be incorrectly split at "-coder".
var knownVariantSuffixes = []string{
	"-coder",
	"-code",
	"-chat",
	"-instruct",
	"-vision",
	"-math",
}

// knownCompoundFamilies are model family prefixes that include hyphens as part
// of their identity and should not be split at hyphens. Order matters: longer
// prefixes must appear before shorter ones so that "deepseek-coder-v2" matches
// before "deepseek-coder".
var knownCompoundFamilies = []string{
	"deepseek-coder-v2",
	"deepseek-coder",
	"deepseek-r1",
	"deepseek-v2",
	"nomic-embed-text",
	"all-minilm",
	"mxbai-embed-large",
	"qwen3-coder-next",
	"solar-pro",
}

// paramSizeRe matches parameter size patterns: "8b", "70b", "3.8b", "0.6b", "8x7b".
var paramSizeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?(?:x\d+)?b)$`)

// quantRe matches quantization level patterns: "q4_K_M", "q8_0", "fp16", "f16", "f32".
var quantRe = regexp.MustCompile(`^(?i)(q\d+[a-z0-9_]*|fp16|f16|f32|fp32)$`)

// activeParamRe matches MoE active parameter patterns like "a3b", "a3.5b".
var activeParamRe = regexp.MustCompile(`^a\d+(?:\.\d+)?b$`)

// tagVariantRe matches known variant keywords that appear in the tag portion
// of the model name, e.g. "instruct", "chat", "code".
var tagVariantRe = regexp.MustCompile(`^(?i)(instruct|chat|code|coder|vision|math|text)$`)

// ParseModelName decomposes an Ollama model name string into its constituent
// parts: family, variant, parameter size, and quantization level.
//
// Examples:
//
//	"qwen3:8b"                  -> Family: "qwen3", ParamSize: "8b"
//	"qwen3.5:35b-a3b-q4_K_M"   -> Family: "qwen3.5", Variant: "a3b", ParamSize: "35b", Quant: "q4_k_m"
//	"qwen2.5-coder:7b"         -> Family: "qwen2.5", Variant: "coder", ParamSize: "7b"
//	"codellama:latest"          -> Family: "codellama", Tag: "latest"
//	"nomic-embed-text"          -> Family: "nomic-embed-text", Tag: "latest"
func ParseModelName(name string) ParsedModel {
	p := ParsedModel{Raw: name}

	if name == "" {
		return p
	}

	// Split on colon: family[:tag]
	family, tag := splitModelTag(name)
	p.Tag = tag

	// Extract variant from family part (e.g., "qwen2.5-coder" -> family="qwen2.5", variant="coder")
	family, familyVariant := extractFamilyVariant(family)
	p.Family = family

	// Parse the tag into components
	if tag != "latest" && tag != "" {
		parts := strings.Split(tag, "-")
		var paramSize string
		var quant string
		var activeParam string
		var otherParts []string

		for _, part := range parts {
			lowerPart := strings.ToLower(part)
			switch {
			case paramSize == "" && paramSizeRe.MatchString(lowerPart):
				paramSize = lowerPart
			case quantRe.MatchString(part):
				quant = strings.ToLower(part)
			case activeParamRe.MatchString(lowerPart):
				activeParam = lowerPart
			default:
				otherParts = append(otherParts, part)
			}
		}

		p.ParamSize = paramSize
		p.Quant = quant

		// Determine variant priority:
		// 1. MoE active param from tag (e.g., "a3b") -- most specific
		// 2. Family-level variant (e.g., "coder" from "qwen2.5-coder")
		// 3. Remaining tag parts that look like variant keywords (e.g., "instruct")
		// 4. All remaining unclassified tag parts joined together
		if activeParam != "" {
			p.Variant = activeParam
		} else if familyVariant != "" {
			p.Variant = familyVariant
		} else if len(otherParts) > 0 {
			// Filter out version-like suffixes (e.g., "v0.3") from variant parts
			// by keeping them attached. Join all remaining parts as the variant.
			p.Variant = strings.ToLower(strings.Join(otherParts, "-"))
		}
	} else if familyVariant != "" {
		p.Variant = familyVariant
	}

	return p
}

// NormalizedFamily returns the lowercase family name suitable for catalog
// lookup. For models with variant suffixes in the family (e.g., "qwen2.5-coder"),
// this returns just the base family ("qwen2.5").
func (p ParsedModel) NormalizedFamily() string {
	return strings.ToLower(p.Family)
}

// splitModelTag splits a model name into base and tag at the colon.
// If no colon is present, tag defaults to "latest".
func splitModelTag(name string) (base, tag string) {
	idx := strings.LastIndex(name, ":")
	if idx < 0 {
		return name, "latest"
	}
	return name[:idx], name[idx+1:]
}

// extractFamilyVariant splits the family portion at known variant suffixes.
// Returns the base family and variant (empty if none found).
//
// Known compound families (e.g., "deepseek-coder-v2") are returned intact
// without splitting, since the hyphens are part of the family identity.
func extractFamilyVariant(family string) (base, variant string) {
	lower := strings.ToLower(family)

	// Check if the family is a known compound family (don't split these).
	for _, compound := range knownCompoundFamilies {
		if lower == compound || strings.HasPrefix(lower, compound+"-") {
			return family, ""
		}
	}

	// Check for known variant suffixes.
	for _, suffix := range knownVariantSuffixes {
		if strings.HasSuffix(lower, suffix) {
			base = family[:len(family)-len(suffix)]
			variant = strings.TrimPrefix(suffix, "-")
			return base, variant
		}
	}

	return family, ""
}
