package compat

import (
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// resolveModel translates a wire model name into a provider.ModelKey. The
// returned ModelKey has an empty Provider when the input (or its alias target)
// is unqualified — the router resolves it across all providers advertising
// that model.
//
// Alias lookup is intentionally single-level: if aliases["gpt-4"] = "gpt-4-turbo"
// and aliases["gpt-4-turbo"] = "qwen3-coder-next", the result is "gpt-4-turbo",
// not "qwen3-coder-next". This avoids accidental cycles and makes config
// readable at a glance; callers who want chaining should flatten at config
// time. Lookups are case-sensitive — OpenAI wire names are lowercase in
// practice.
func resolveModel(wire string, aliases map[string]string) (provider.ModelKey, error) {
	wire = strings.TrimSpace(wire)
	if wire == "" {
		return provider.ModelKey{}, fmt.Errorf("compat: model field is required")
	}
	name := wire
	if alias, ok := aliases[wire]; ok {
		name = alias
	}
	key := parseKey(name)
	// parseKey can return an empty Model for inputs like "/" or "ollama/" —
	// catch these here so the router sees a clean 400 instead of an opaque
	// "model not found" on a zero-width name.
	if key.Model == "" {
		return provider.ModelKey{}, fmt.Errorf("compat: malformed model %q", wire)
	}
	return key, nil
}

// parseKey splits "provider/model" into a ModelKey. No slash => unqualified.
func parseKey(s string) provider.ModelKey {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return provider.ModelKey{Provider: s[:i], Model: s[i+1:]}
	}
	return provider.ModelKey{Model: s}
}

// RecommendedAliases returns a conservative preset suitable for IDE tools
// that hardcode OpenAI model names. Callers pass the result to WithAliases;
// the Server default is an empty map. Returned map is a fresh copy so
// callers may mutate it without affecting future calls.
//
// Mappings reflect the go-llm role intent rather than identical capabilities:
//   - "gpt-4" / "gpt-4-turbo"           -> coding role (qwen3-coder-next)
//   - "gpt-4o"                          -> agent role (gemma4:31b)
//   - "gpt-4o-mini" / "gpt-3.5-turbo"   -> fast role (qwen3.6:35b-a3b)
//   - "text-embedding-3-large"          -> embedding role (qwen3-embedding:8b)
//
// These mappings are intentional best-effort and not a stability contract.
// The models.json lineup may shift (see CHANGELOG.md); alias presets migrate
// alongside it.
func RecommendedAliases() map[string]string {
	return map[string]string{
		"gpt-4":                  "qwen3-coder-next:latest",
		"gpt-4-turbo":            "qwen3-coder-next:latest",
		"gpt-4o":                 "gemma4:31b",
		"gpt-4o-mini":            "qwen3.6:35b-a3b",
		"gpt-3.5-turbo":          "qwen3.6:35b-a3b",
		"text-embedding-3-large": "qwen3-embedding:8b",
		"text-embedding-3-small": "qwen3-embedding:8b",
		"text-embedding-ada-002": "qwen3-embedding:8b",
	}
}
