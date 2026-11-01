package config

import (
	"fmt"
	"testing"
)

// TestCacheAffinityUnknownValueRejected: cache_affinity is a closed set, so a
// typo fails the load instead of silently sending nothing.
func TestCacheAffinityUnknownValueRejected(t *testing.T) {
	load := func(t *testing.T, v string) (*Config, error) {
		t.Helper()
		return Load(writeTempJSON(t, fmt.Sprintf(`{
			"providers": {"p": {"base_url": "https://api.openai.com", "api_format": "openai-compat", "cache_affinity": %q}},
			"models": {"m": {"name": "x", "provider": "p", "type": "dense"}},
			"defaults": {}
		}`, v)))
	}
	for _, v := range []string{"", "none", "openai", "openrouter", "opencode"} {
		t.Run("accepts "+v, func(t *testing.T) {
			cfg, err := load(t, v)
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
			_, err := load(t, v)
			if want := fmt.Sprintf("config: provider %q: unknown cache_affinity %q", "p", v); err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			assertDiag(t, err, CodeInvalidArgument, SubjectProvider, "p")
		})
	}
}
