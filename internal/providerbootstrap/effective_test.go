package providerbootstrap

import (
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// I9/M9: every URL override lands in the ONE effective config that display,
// policy, manifest, client construction, and receipt all read. The pre-#477
// code applied the Ollama override only to the constructed client, so
// Bundle.Config reported a URL the client was not dialing (F10) — the T7
// divergence this test exists to keep dead.
func TestMaterializeOllamaOverrideLandsInEffectiveConfig(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://configured.example:11434", APIFormat: "ollama"},
	}}
	eff, err := materializeEffectiveConfig(cfg, "http://override.example:9999", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.cfg.Providers["ollama"].BaseURL; got != "http://override.example:9999" {
		t.Errorf("effective base URL = %q, want the override the client dials", got)
	}
	d, ok := eff.dests["ollama"]
	if !ok {
		t.Fatal("no destination descriptor for ollama")
	}
	if got := d.BaseURL(); got != "http://override.example:9999" {
		t.Errorf("destination = %q, want derived from the override", got)
	}
	// Purity: the caller's config is untouched.
	if got := cfg.Providers["ollama"].BaseURL; got != "http://configured.example:11434" {
		t.Errorf("caller's config mutated: %q", got)
	}
}

// The spec's named case: nil config plus a REMOTE Ollama override. The
// synthetic default is loopback, but an override can point anywhere — the
// destination must classify remote so admission later gates it, not wave it
// through under the "default ollama is local" assumption.
func TestMaterializeNilConfigRemoteOllamaOverride(t *testing.T) {
	eff, err := materializeEffectiveConfig(nil, "http://lan-box.example:11434", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.cfg.Providers["ollama"].BaseURL; got != "http://lan-box.example:11434" {
		t.Errorf("synthetic effective base URL = %q, want the override", got)
	}
	d := eff.dests["ollama"]
	if d.IsLocal() {
		t.Error("remote override classified local; admission would silently skip it")
	}
}

func TestMaterializeNilConfigDefaultIsLoopback(t *testing.T) {
	eff, err := materializeEffectiveConfig(nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := eff.dests["ollama"]
	if !ok {
		t.Fatal("no destination for the synthetic default provider")
	}
	if !d.IsLocal() {
		t.Errorf("default %q must classify local", d.BaseURL())
	}
}

// I10: a base URL the destination identity rejects — query, userinfo,
// fragment, bad scheme — fails at materialization, before any client exists,
// with the typed error and no echo of the offending value (M20).
func TestMaterializeRejectsUnsafeBaseURLs(t *testing.T) {
	const secret = "SECRET-CANARY-77413"
	tests := map[string]string{
		"query":    "https://api.example.com/v1?token=" + secret,
		"userinfo": "https://user:" + secret + "@api.example.com/v1",
		"fragment": "https://api.example.com/v1#" + secret,
		"scheme":   "ftp://api.example.com/" + secret,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Providers: map[string]config.ProviderConfig{
				"opencode": {BaseURL: raw, APIFormat: "openai-compat"},
			}}
			_, err := materializeEffectiveConfig(cfg, "", "", "")
			if err == nil {
				t.Fatal("unsafe base URL accepted")
			}
			if !errors.Is(err, provider.ErrDestinationInvalid) {
				t.Errorf("error %v does not match ErrDestinationInvalid", err)
			}
			if !strings.Contains(err.Error(), "opencode") {
				t.Errorf("error does not name the provider: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error echoed the secret: %v", err)
			}
		})
	}
}

// An override-supplied URL goes through the same destination validation as a
// config-borne one: the -base-url flag and WithOllamaURL are user input too,
// and a credential-bearing value there must fail just as loudly (and just as
// quietly in the diagnostic).
func TestMaterializeValidatesOverrideURLs(t *testing.T) {
	const secret = "SECRET-CANARY-77413"

	t.Run("openai-compat override", func(t *testing.T) {
		cfg := &config.Config{Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
		}}
		_, err := materializeEffectiveConfig(cfg, "", "llamacpp", "http://127.0.0.1:8083?token="+secret)
		if !errors.Is(err, provider.ErrDestinationInvalid) {
			t.Fatalf("query-bearing oc override = %v, want ErrDestinationInvalid", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error echoed the secret: %v", err)
		}
	})

	t.Run("ollama override", func(t *testing.T) {
		_, err := materializeEffectiveConfig(nil, "http://user:"+secret+"@127.0.0.1:11434", "", "")
		if !errors.Is(err, provider.ErrDestinationInvalid) {
			t.Fatalf("userinfo-bearing ollama override = %v, want ErrDestinationInvalid", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error echoed the secret: %v", err)
		}
	})
}

// The effective copy carries the DEFAULTED api_format, so every consumer of
// the effective config — destination derivation included — sees the same
// value the constructed client acts on, instead of re-deriving the default
// at each site.
func TestMaterializeNormalizesAPIFormatOnTheCopy(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://localhost:11434"}, // APIFormat empty
	}}
	eff, err := materializeEffectiveConfig(cfg, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.cfg.Providers["ollama"].APIFormat; got != "ollama" {
		t.Errorf("effective api_format = %q, want the materialized default", got)
	}
	if got := cfg.Providers["ollama"].APIFormat; got != "" {
		t.Errorf("caller's config mutated: api_format = %q", got)
	}
}

// Both override families on one call: each lands in the effective config,
// and every provider gets a destination descriptor.
func TestMaterializeBothOverrideFamilies(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
		"ollama":   {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	eff, err := materializeEffectiveConfig(cfg, "http://127.0.0.1:7777", "llamacpp", "http://127.0.0.1:8083")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.cfg.Providers["llamacpp"].BaseURL; got != "http://127.0.0.1:8083" {
		t.Errorf("llamacpp = %q, want oc override", got)
	}
	if got := eff.cfg.Providers["ollama"].BaseURL; got != "http://127.0.0.1:7777" {
		t.Errorf("ollama = %q, want ollama override", got)
	}
	if len(eff.dests) != 2 {
		t.Errorf("destinations = %d, want one per provider", len(eff.dests))
	}
	// Purity again, with both overrides in play.
	if cfg.Providers["ollama"].BaseURL != "http://localhost:11434" ||
		cfg.Providers["llamacpp"].BaseURL != "http://127.0.0.1:8080" {
		t.Error("caller's config mutated by override application")
	}
}

// The Ollama override targets only an ollama-format provider named "ollama";
// an openai-compat provider that happens to be named "ollama" is left alone
// (mirrors the pre-existing WithOllamaURL contract).
func TestMaterializeOllamaOverrideSkipsNonOllamaFormat(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
	}}
	eff, err := materializeEffectiveConfig(cfg, "http://127.0.0.1:7777", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.cfg.Providers["ollama"].BaseURL; got != "http://127.0.0.1:8080" {
		t.Errorf("openai-compat 'ollama' provider rewritten to %q", got)
	}
}

// End-to-end through New: Bundle.Config is the effective config, so what a
// consumer displays is what the live client dials (I9 at the Bundle surface).
func TestNewBundleConfigReflectsOllamaOverride(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://configured.example:11434", APIFormat: "ollama"},
	}}
	bundle, err := New(t.Context(), Options{Config: cfg, OllamaURLOverride: "http://127.0.0.1:9999"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()
	if got := bundle.Config.Providers["ollama"].BaseURL; got != "http://127.0.0.1:9999" {
		t.Errorf("Bundle.Config base URL = %q, want the override the client dials", got)
	}
}
