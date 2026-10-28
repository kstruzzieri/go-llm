package providerbootstrap

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

// TestBundleSecretMatcherFromEffectiveConfig (#424): the bundle carries the
// matcher of the config it materialized. Every value is a synthetic fixture
// resolved through an explicit lookup fake; no real environment is read.
func TestBundleSecretMatcherFromEffectiveConfig(t *testing.T) {
	t.Run("template component survives URL overrides", func(t *testing.T) {
		const component = "golem-t-component-0001"
		doc, err := config.ParseDocument([]byte(`{
  "providers": {"lc": {"base_url": "http://127.0.0.1:1", "api_format": "openai-compat", "api_key": "pre-${GOLEM_T_KEY}"}},
  "models": {"chat": {"name": "m", "provider": "lc", "type": "dense"}}
}`), config.Origin{Source: config.OriginExplicit}, config.DocumentOptions{LookupEnv: func(name string) (string, bool) {
			if name == "GOLEM_T_KEY" {
				return component, true
			}
			return "", false
		}})
		if err != nil {
			t.Fatalf("ParseDocument: %v", err)
		}
		b, err := New(context.Background(), Options{
			Config:                          doc.Config(),
			OpenAICompatURLOverrideProvider: "lc",
			OpenAICompatURLOverride:         "http://127.0.0.1:2",
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = b.Close() }()
		if got := b.Config.Providers["lc"].BaseURL; got != "http://127.0.0.1:2" {
			t.Fatalf("effective base URL = %q, want the override", got)
		}
		if b.SecretMatcher == nil {
			t.Fatal("Bundle.SecretMatcher is nil")
		}
		// The component alone is the discriminating row: it is a member only
		// through the resolved sidecar, which a copy that drops it would lose
		// while the full effective key still matched.
		for _, tc := range []struct {
			text string
			want bool
		}{
			{component, true},
			{"pre-" + component, true},
			{"${GOLEM_T_KEY}", false},
			{"GOLEM_T_KEY", false},
			{"pre-", false},
			{"golem-t-component-0002", false},
		} {
			if got := b.SecretMatcher(tc.text); got != tc.want {
				t.Errorf("SecretMatcher(%q) = %v, want %v", tc.text, got, tc.want)
			}
		}
	})
	t.Run("nil config is an enabled empty set", func(t *testing.T) {
		// The default provider points at a refusing port, so the refresh
		// fails into a warning instead of reaching a real server.
		b, err := New(context.Background(), Options{OllamaURLOverride: "http://127.0.0.1:1"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = b.Close() }()
		if b.SecretMatcher == nil {
			t.Fatal("Bundle.SecretMatcher is nil for Config: nil, want an always-false matcher")
		}
		for _, text := range []string{"", "ollama", "http://127.0.0.1:1"} {
			if b.SecretMatcher(text) {
				t.Errorf("SecretMatcher(%q) = true, want false", text)
			}
		}
	})
}
