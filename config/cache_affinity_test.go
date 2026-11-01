package config

import (
	"fmt"
	"testing"
)

// loadCacheAffinity loads a one-provider config whose provider "p" has the
// given api_format (omitted when empty) and cache_affinity.
func loadCacheAffinity(t *testing.T, format, v string) (*Config, error) {
	t.Helper()
	formatField := ""
	if format != "" {
		formatField = fmt.Sprintf(`"api_format": %q, `, format)
	}
	return Load(writeTempJSON(t, fmt.Sprintf(`{
		"providers": {"p": {"base_url": "https://api.openai.com", %s"cache_affinity": %q}},
		"models": {"m": {"name": "x", "provider": "p", "type": "dense"}},
		"defaults": {}
	}`, formatField, v)))
}

// TestCacheAffinityUnknownValueRejected: cache_affinity is a closed set, so a
// typo fails the load instead of silently sending nothing.
func TestCacheAffinityUnknownValueRejected(t *testing.T) {
	for _, v := range []string{"", "none", "openai", "openrouter", "opencode"} {
		t.Run("accepts "+v, func(t *testing.T) {
			cfg, err := loadCacheAffinity(t, "openai-compat", v)
			if err != nil {
				t.Fatalf("cache_affinity %q: %v", v, err)
			}
			if got := cfg.Providers["p"].CacheAffinity; got != v {
				t.Errorf("CacheAffinity = %q, want %q kept as authored", got, v)
			}
		})
	}
	for _, v := range []string{"OpenAI", "anthropic", " openai"} {
		t.Run("rejects "+v, func(t *testing.T) {
			_, err := loadCacheAffinity(t, "openai-compat", v)
			if want := fmt.Sprintf("config: provider %q: unknown cache_affinity %q", "p", v); err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			assertDiag(t, err, CodeInvalidArgument, SubjectProvider, "p")
		})
	}
}

// TestCacheAffinityRequiresOpenAICompat: only the openai-compat client sends a
// cache key, so a cache_affinity on any other provider would be silently
// ignored; it fails the load the way slot_discovery does. Empty stays valid.
func TestCacheAffinityRequiresOpenAICompat(t *testing.T) {
	for _, tc := range []struct{ format, value string }{
		{"ollama", "none"},
		{"ollama", "opencode"},
		{"", "openai"}, // omitted api_format means ollama
	} {
		t.Run(tc.format+"/"+tc.value, func(t *testing.T) {
			_, err := loadCacheAffinity(t, tc.format, tc.value)
			if want := `config: provider "p": cache_affinity requires api_format "openai-compat", got "ollama"`; err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			assertDiag(t, err, CodeInvalidArgument, SubjectProvider, "p")
		})
	}
	if _, err := loadCacheAffinity(t, "ollama", ""); err != nil {
		t.Errorf("ollama provider without cache_affinity: %v", err)
	}
}
