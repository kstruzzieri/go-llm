package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDuration_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr bool
	}{
		{
			name:  "minutes",
			input: `{"d":"5m"}`,
			want:  5 * time.Minute,
		},
		{
			name:  "seconds",
			input: `{"d":"30s"}`,
			want:  30 * time.Second,
		},
		{
			name:  "hours",
			input: `{"d":"2h"}`,
			want:  2 * time.Hour,
		},
		{
			name:  "combined",
			input: `{"d":"1h30m"}`,
			want:  1*time.Hour + 30*time.Minute,
		},
		{
			name:    "invalid",
			input:   `{"d":"notaduration"}`,
			wantErr: true,
		},
		{
			name:    "empty",
			input:   `{"d":""}`,
			wantErr: true,
		},
		{
			name:    "number",
			input:   `{"d":123}`,
			wantErr: true,
		},
		{
			name:    "negative",
			input:   `{"d":"-5m"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v struct {
				D Duration `json:"d"`
			}
			err := json.Unmarshal([]byte(tt.input), &v)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.D.Duration != tt.want {
				t.Errorf("got %v, want %v", v.D.Duration, tt.want)
			}
		})
	}
}

func TestDuration_MarshalJSON(t *testing.T) {
	d := Duration{Duration: 5 * time.Minute}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `"5m0s"`
	if string(data) != want {
		t.Errorf("got %s, want %s", string(data), want)
	}
}

// writeTempJSON writes content to a temporary JSON file and returns its path.
func writeTempJSON(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestLoad_ValidConfig(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(cfg.Providers); got != 1 {
		t.Errorf("providers: got %d, want 1", got)
	}
	if got := len(cfg.Models); got != 5 {
		t.Errorf("models: got %d, want 5", got)
	}
	if got := len(cfg.Defaults); got != 5 {
		t.Errorf("defaults: got %d, want 5", got)
	}
}

func TestLoad_MinimalConfig(t *testing.T) {
	cfg, err := Load("testdata/minimal.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The model has no explicit provider; Load should materialize "ollama" as the implicit provider.
	m := cfg.RoleConfig("default")
	if m == nil {
		t.Fatal("expected model 'default' to exist")
	}
	if m.Provider != "ollama" {
		t.Errorf("expected implicit provider 'ollama', got %q", m.Provider)
	}
}

func TestLoad_TimeoutDefaulting(t *testing.T) {
	// minimal.json has an explicit 30s timeout — it should be preserved.
	cfg, err := Load("testdata/minimal.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := cfg.Provider("ollama")
	if p == nil {
		t.Fatal("expected provider 'ollama' to exist")
	}
	if p.Timeout.Duration != 30*time.Second {
		t.Errorf("explicit timeout: got %v, want 30s", p.Timeout.Duration)
	}

	// Build a config where the timeout is omitted — it should default to 5m.
	noTimeout := writeTempJSON(t, `{
		"providers": {"ollama": {"base_url": "http://localhost:11434"}},
		"models": {"m": {"name": "x", "type": "dense"}},
		"defaults": {}
	}`)
	cfg2, err := Load(noTimeout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p2 := cfg2.Provider("ollama")
	if p2 == nil {
		t.Fatal("expected provider 'ollama' to exist")
	}
	if p2.Timeout.Duration != 5*time.Minute {
		t.Errorf("default timeout: got %v, want 5m", p2.Timeout.Duration)
	}
}

func TestLoad_APIFormatDefaulting(t *testing.T) {
	// Empty api_format must materialize to "ollama" via applyDefaults so
	// configs that predate the field keep working.
	noFormat := writeTempJSON(t, `{
		"providers": {"ollama": {"base_url": "http://localhost:11434"}},
		"models": {"m": {"name": "x", "type": "dense"}},
		"defaults": {}
	}`)
	cfg, err := Load(noFormat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := cfg.Provider("ollama")
	if p == nil {
		t.Fatal("expected provider 'ollama' to exist")
	}
	if p.APIFormat != "ollama" {
		t.Errorf("default api_format: got %q, want %q", p.APIFormat, "ollama")
	}

	// Explicit known values pass through unchanged.
	explicit := writeTempJSON(t, `{
		"providers": {"local": {"base_url": "http://localhost:8080", "api_format": "openai-compat"}},
		"models": {"m": {"name": "x", "type": "dense", "provider": "local"}},
		"defaults": {}
	}`)
	cfg2, err := Load(explicit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p2 := cfg2.Provider("local")
	if p2 == nil {
		t.Fatal("expected provider 'local' to exist")
	}
	if p2.APIFormat != "openai-compat" {
		t.Errorf("explicit api_format: got %q, want %q", p2.APIFormat, "openai-compat")
	}

	// Explicit api_format on a provider with non-zero timeout must survive
	// applyDefaults — guards against the regression where the write-back
	// only fired inside the timeout-default branch.
	explicitBoth := writeTempJSON(t, `{
		"providers": {"local": {"base_url": "http://localhost:8080", "api_format": "openai-compat", "timeout": "10s"}},
		"models": {"m": {"name": "x", "type": "dense", "provider": "local"}},
		"defaults": {}
	}`)
	cfg3, err := Load(explicitBoth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p3 := cfg3.Provider("local")
	if p3 == nil {
		t.Fatal("expected provider 'local' to exist")
	}
	if p3.APIFormat != "openai-compat" {
		t.Errorf("api_format with explicit timeout: got %q, want %q (applyDefaults regression)", p3.APIFormat, "openai-compat")
	}
	if p3.Timeout.Duration != 10*time.Second {
		t.Errorf("explicit timeout: got %v, want 10s", p3.Timeout.Duration)
	}
}

// TestLoad_ProductionModelsJSON_NoBehaviorChange pins the claim from the
// PR description: existing single-Ollama deployments see no behavior change
// from the new Capabilities field. The repo's actual models.json has no
// capabilities populated on any model; every ResolvedCapabilities() result
// must match the derive-from-type defaults so routing decisions made on
// top of those caps remain identical to develop's behavior.
func TestLoad_ProductionModelsJSON_NoBehaviorChange(t *testing.T) {
	cfg, err := Load("../models.json")
	if err != nil {
		t.Fatalf("repo models.json fails to load after PR1 changes: %v", err)
	}

	for role, m := range cfg.Models {
		t.Run(role, func(t *testing.T) {
			if len(m.Capabilities) != 0 {
				t.Skipf("model %q now has explicit capabilities; remove this skip when production config opts in", role)
			}
			got := m.ResolvedCapabilities()
			var want []string
			switch m.Type {
			case "dense", "moe":
				want = []string{"chat", "generate", "stream"}
			case "embedding":
				want = []string{"embed"}
			default:
				t.Fatalf("model %q has unexpected type %q", role, m.Type)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ResolvedCapabilities = %v, want %v (production model %q derive-from-type)", got, want, role)
			}
		})
	}
}

// TestLoad_CapabilitiesExample exercises the testdata/capabilities.json
// fixture so the documented usage stays valid and a regression in the
// schema vocabulary cannot quietly desync from the example.
func TestLoad_CapabilitiesExample(t *testing.T) {
	cfg, err := Load("testdata/capabilities.json")
	if err != nil {
		t.Fatalf("capabilities.json fixture fails to load: %v", err)
	}

	carved := cfg.Models["carved-down"]
	if got := carved.ResolvedCapabilities(); !reflect.DeepEqual(got, []string{"chat", "stream"}) {
		t.Errorf("carved-down capabilities = %v, want [chat stream] (explicit replaces derived)", got)
	}

	withTools := cfg.Models["with-tools"]
	if got := withTools.ResolvedCapabilities(); !reflect.DeepEqual(got, []string{"chat", "generate", "stream", "tool_call"}) {
		t.Errorf("with-tools capabilities = %v, want [chat generate stream tool_call]", got)
	}

	general := cfg.Models["general"]
	if got := general.ResolvedCapabilities(); !reflect.DeepEqual(got, []string{"chat", "generate", "stream"}) {
		t.Errorf("general (derive-from-type) = %v, want [chat generate stream]", got)
	}

	emb := cfg.Models["embedding"]
	if got := emb.ResolvedCapabilities(); !reflect.DeepEqual(got, []string{"embed"}) {
		t.Errorf("embedding (derive-from-type) = %v, want [embed]", got)
	}

	// The remote-llamacpp provider should carry api_format through Load.
	remote := cfg.Provider("remote-llamacpp")
	if remote == nil {
		t.Fatal("remote-llamacpp provider missing")
	}
	if remote.APIFormat != "openai-compat" {
		t.Errorf("remote-llamacpp api_format = %q, want openai-compat", remote.APIFormat)
	}
}

func TestModelConfig_ResolvedCapabilities(t *testing.T) {
	tests := []struct {
		name string
		m    ModelConfig
		want []string
	}{
		{
			name: "dense default",
			m:    ModelConfig{Type: "dense"},
			want: []string{"chat", "generate", "stream"},
		},
		{
			name: "moe default",
			m:    ModelConfig{Type: "moe"},
			want: []string{"chat", "generate", "stream"},
		},
		{
			name: "embedding default",
			m:    ModelConfig{Type: "embedding"},
			want: []string{"embed"},
		},
		{
			name: "explicit replaces derived (carve-down for missing /v1/completions)",
			m:    ModelConfig{Type: "dense", Capabilities: []string{"chat", "stream"}},
			want: []string{"chat", "stream"},
		},
		{
			name: "explicit replaces derived (no merge with chat/generate/stream)",
			m:    ModelConfig{Type: "dense", Capabilities: []string{"chat"}},
			want: []string{"chat"},
		},
		{
			name: "explicit on embedding",
			m:    ModelConfig{Type: "embedding", Capabilities: []string{"embed"}},
			want: []string{"embed"},
		},
		{
			name: "unknown type yields nil",
			m:    ModelConfig{Type: "bogus"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.m.ResolvedCapabilities()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("returned slice is independent copy", func(t *testing.T) {
		m := ModelConfig{Type: "dense"}
		first := m.ResolvedCapabilities()
		first[0] = "mutated"
		second := m.ResolvedCapabilities()
		if second[0] == "mutated" {
			t.Error("ResolvedCapabilities returned aliased slice; mutation leaked across calls")
		}
	})
}

func TestLoad_CapabilityValidation(t *testing.T) {
	t.Run("unknown capability rejected", func(t *testing.T) {
		bad := writeTempJSON(t, `{
			"providers": {"ollama": {"base_url": "http://localhost:11434"}},
			"models": {"m": {"name": "x", "type": "dense", "capabilities": ["chat", "nonexistent"]}},
			"defaults": {}
		}`)
		_, err := Load(bad)
		if err == nil {
			t.Fatal("expected error for unknown capability, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("error should mention bad capability, got: %v", err)
		}
	})

	t.Run("embedding type with non-embedding capability rejected", func(t *testing.T) {
		bad := writeTempJSON(t, `{
			"providers": {"ollama": {"base_url": "http://localhost:11434"}},
			"models": {"m": {"name": "x", "type": "embedding", "capabilities": ["chat"]}},
			"defaults": {}
		}`)
		_, err := Load(bad)
		if err == nil {
			t.Fatal("expected error for embedding type with chat capability, got nil")
		}
		if !strings.Contains(err.Error(), "embedding") || !strings.Contains(err.Error(), "chat") {
			t.Errorf("error should mention type and bad capability, got: %v", err)
		}
	})

	t.Run("embedding type with explicit embed capability accepted", func(t *testing.T) {
		good := writeTempJSON(t, `{
			"providers": {"ollama": {"base_url": "http://localhost:11434"}},
			"models": {"m": {"name": "x", "type": "embedding", "capabilities": ["embed"]}},
			"defaults": {}
		}`)
		if _, err := Load(good); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("dense type with carved-down capabilities accepted", func(t *testing.T) {
		good := writeTempJSON(t, `{
			"providers": {"ollama": {"base_url": "http://localhost:11434"}},
			"models": {"m": {"name": "x", "type": "dense", "capabilities": ["chat", "stream"]}},
			"defaults": {}
		}`)
		if _, err := Load(good); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// User config rejects multi-bit aliases: the REPLACES contract on
	// override would otherwise leak expanded bits the user excluded.
	// Catalog/runtime metadata sources may still use them; user config
	// must declare canonical single-bit names only.
	t.Run("multi-bit aliases rejected in user config", func(t *testing.T) {
		for _, alias := range []string{"completion", "tools", "embedding"} {
			bad := writeTempJSON(t, `{
				"providers": {"ollama": {"base_url": "http://localhost:11434"}},
				"models": {"m": {"name": "x", "type": "dense", "capabilities": ["`+alias+`"]}},
				"defaults": {}
			}`)
			_, err := Load(bad)
			if err == nil {
				t.Errorf("expected error for alias %q in user config, got nil", alias)
			}
		}
	})

	// JSON-source casing must not produce silent disagreement between
	// config validation and provider parsing. Both lowercase before lookup.
	t.Run("mixed-case capability tokens accepted", func(t *testing.T) {
		good := writeTempJSON(t, `{
			"providers": {"ollama": {"base_url": "http://localhost:11434"}},
			"models": {"m": {"name": "x", "type": "dense", "capabilities": ["Chat", "STREAM"]}},
			"defaults": {}
		}`)
		if _, err := Load(good); err != nil {
			t.Fatalf("unexpected error for mixed-case tokens: %v", err)
		}
	})
}

func TestLoad_APIFormatValidation(t *testing.T) {
	// Unknown explicit api_format must be rejected at load time so typos
	// surface immediately instead of silently defaulting.
	bad := writeTempJSON(t, `{
		"providers": {"ollama": {"base_url": "http://localhost:11434", "api_format": "ollma"}},
		"models": {"m": {"name": "x", "type": "dense"}},
		"defaults": {}
	}`)
	_, err := Load(bad)
	if err == nil {
		t.Fatal("expected error for unknown api_format, got nil")
	}
	if !strings.Contains(err.Error(), "api_format") || !strings.Contains(err.Error(), "ollma") {
		t.Errorf("error should mention api_format and the bad value, got: %v", err)
	}
}

func TestLoad_MoEFallbackCompatibility(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "fast" is a MoE model that falls back to "lightweight" (dense) — this should be allowed.
	fast := cfg.RoleConfig("fast")
	if fast == nil {
		t.Fatal("expected model 'fast' to exist")
	}
	if fast.Type != "moe" {
		t.Errorf("expected type 'moe', got %q", fast.Type)
	}
	if len(fast.Fallbacks) != 1 || fast.Fallbacks[0] != "lightweight" {
		t.Errorf("expected fallbacks [lightweight], got %v", fast.Fallbacks)
	}
}

func TestLoad_Validation(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "malformed json",
			json:    `{not json`,
			wantErr: "config: parse",
		},
		{
			name:    "no providers",
			json:    `{"providers": {}, "models": {"m": {"name": "x", "type": "dense"}}, "defaults": {}}`,
			wantErr: "config: at least one provider is required",
		},
		{
			name:    "empty provider name",
			json:    `{"providers": {"": {"base_url": "http://localhost:11434"}}, "models": {"m": {"name": "x", "type": "dense"}}, "defaults": {}}`,
			wantErr: "config: provider name must not be empty",
		},
		{
			name:    "provider name contains slash",
			json:    `{"providers": {"team/local": {"base_url": "http://localhost:11434"}}, "models": {"m": {"name": "x", "type": "dense", "provider": "team/local"}}, "defaults": {}}`,
			wantErr: `config: provider name "team/local" must not contain "/"`,
		},
		{
			name:    "empty base_url",
			json:    `{"providers": {"ollama": {"base_url": ""}}, "models": {"m": {"name": "x", "type": "dense"}}, "defaults": {}}`,
			wantErr: `config: provider "ollama": base_url is required`,
		},
		{
			name:    "invalid base_url",
			json:    `{"providers": {"ollama": {"base_url": "://bad"}}, "models": {"m": {"name": "x", "type": "dense"}}, "defaults": {}}`,
			wantErr: `config: provider "ollama": invalid base_url`,
		},
		{
			name:    "empty model name",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "", "type": "dense"}}, "defaults": {}}`,
			wantErr: `config: model "x": name is required`,
		},
		{
			name:    "missing model type",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "m"}}, "defaults": {}}`,
			wantErr: `config: model "x": type is required`,
		},
		{
			name:    "invalid model type",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "m", "type": "huge"}}, "defaults": {}}`,
			wantErr: `config: model "x": invalid type "huge"`,
		},
		{
			name:    "default references missing role",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "m", "type": "dense"}}, "defaults": {"chat": "missing"}}`,
			wantErr: `config: default "chat" references unknown role "missing"`,
		},
		{
			name:    "explicit provider missing",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "m", "type": "dense", "provider": "vllm"}}, "defaults": {}}`,
			wantErr: `config: model "x": provider "vllm" not found`,
		},
		{
			name:    "implicit provider missing",
			json:    `{"providers": {"vllm": {"base_url": "http://localhost:8000"}}, "models": {"x": {"name": "m", "type": "dense"}}, "defaults": {}}`,
			wantErr: `config: model "x": implicit provider "ollama" not found`,
		},
		{
			name:    "fallback references missing role",
			json:    `{"providers": {"ollama": {"base_url": "http://localhost:11434"}}, "models": {"x": {"name": "m", "type": "dense", "fallbacks": ["missing"]}}, "defaults": {}}`,
			wantErr: `config: model "x": fallback "missing" references unknown role`,
		},
		{
			name: "type-incompatible fallback",
			json: `{
				"providers": {"ollama": {"base_url": "http://localhost:11434"}},
				"models": {
					"x": {"name": "m", "type": "dense", "fallbacks": ["e"]},
					"e": {"name": "emb", "type": "embedding"}
				},
				"defaults": {}
			}`,
			wantErr: `config: model "x": fallback "e" has incompatible type`,
		},
		{
			name:    "self-referencing fallback",
			json:    `{"providers":{"ollama":{"base_url":"http://localhost:11434"}},"models":{"x":{"name":"m","type":"dense","fallbacks":["x"]}},"defaults":{}}`,
			wantErr: `config: model "x": lists itself as a fallback`,
		},
		{
			name: "circular fallback",
			json: `{
				"providers": {"ollama": {"base_url": "http://localhost:11434"}},
				"models": {
					"a": {"name": "m1", "type": "dense", "fallbacks": ["b"]},
					"b": {"name": "m2", "type": "dense", "fallbacks": ["a"]}
				},
				"defaults": {}
			}`,
			wantErr: `config: model "a": circular fallback chain`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempJSON(t, tt.json)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestModelFor(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		useCase string
		want    string
	}{
		{"chat", "qwen3.5:27b"},
		{"completion", "qwen3-coder-next:latest"},
		{"embedding", "qwen3-embedding:8b"},
		{"agent", "qwen3.5:35b-a3b"},
		{"analysis", "qwen3.5:27b"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.useCase, func(t *testing.T) {
			got := cfg.ModelFor(tt.useCase)
			if got != tt.want {
				t.Errorf("ModelFor(%q) = %q, want %q", tt.useCase, got, tt.want)
			}
		})
	}
}

func TestMustModelFor_Panics(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected MustModelFor(\"unknown\") to panic")
		}
	}()

	cfg.MustModelFor("unknown")
}

func TestRoleConfig(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify embedding role returns Dimensions=4096.
	rc := cfg.RoleConfig("embedding")
	if rc == nil {
		t.Fatal("expected RoleConfig(\"embedding\") to be non-nil")
	}
	if rc.Dimensions != 4096 {
		t.Errorf("Dimensions = %d, want 4096", rc.Dimensions)
	}

	// Nonexistent role returns nil.
	if cfg.RoleConfig("nonexistent") != nil {
		t.Error("expected RoleConfig(\"nonexistent\") to be nil")
	}
}

func TestProvider(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify "ollama" returns correct BaseURL.
	p := cfg.Provider("ollama")
	if p == nil {
		t.Fatal("expected Provider(\"ollama\") to be non-nil")
	}
	if p.BaseURL != "http://localhost:11434" {
		t.Errorf("BaseURL = %q, want %q", p.BaseURL, "http://localhost:11434")
	}

	// Nonexistent provider returns nil.
	if cfg.Provider("nonexistent") != nil {
		t.Error("expected Provider(\"nonexistent\") to be nil")
	}
}

func TestProviderFor(t *testing.T) {
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify "general" role resolves to the ollama provider.
	p := cfg.ProviderFor("general")
	if p == nil {
		t.Fatal("expected ProviderFor(\"general\") to be non-nil")
	}
	if p.BaseURL != "http://localhost:11434" {
		t.Errorf("BaseURL = %q, want %q", p.BaseURL, "http://localhost:11434")
	}

	// Nonexistent role returns nil.
	if cfg.ProviderFor("nonexistent") != nil {
		t.Error("expected ProviderFor(\"nonexistent\") to be nil")
	}

	// Programmatic Config with empty Provider returns nil rather than
	// silently defaulting to "ollama" — mirrors the resolveRole contract.
	// applyDefaults (run by Load) materializes the default; callers that
	// bypass Load must set Provider explicitly.
	manualCfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: "http://manual:11434"},
		},
		Models: map[string]ModelConfig{
			"test": {Name: "m", Type: "dense"}, // no Provider set
		},
	}
	if mp := manualCfg.ProviderFor("test"); mp != nil {
		t.Errorf("ProviderFor with empty Provider must return nil, got %+v", mp)
	}

	// Programmatic Config with explicit Provider works as expected.
	// APIFormat must flow through unchanged so downstream callers that
	// branch on it (e.g. provider construction picking ollama vs
	// openai-compat) see the same value the config declared.
	explicit := &Config{
		Providers: map[string]ProviderConfig{
			"my-instance": {BaseURL: "http://manual:11434", APIFormat: "openai-compat"},
		},
		Models: map[string]ModelConfig{
			"test": {Name: "m", Type: "dense", Provider: "my-instance"},
		},
	}
	ep := explicit.ProviderFor("test")
	if ep == nil || ep.BaseURL != "http://manual:11434" {
		t.Errorf("ProviderFor with explicit Provider should return the named instance, got %+v", ep)
	}
	if ep != nil && ep.APIFormat != "openai-compat" {
		t.Errorf("ProviderFor APIFormat = %q, want %q (must flow through resolved ProviderConfig)", ep.APIFormat, "openai-compat")
	}
}

func TestMustLoad_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected MustLoad to panic on nonexistent file")
		}
	}()
	MustLoad("/nonexistent/models.json")
}

func TestDefault_EnvVar(t *testing.T) {
	// Get absolute path to testdata/valid.json.
	absPath, err := filepath.Abs("testdata/valid.json")
	if err != nil {
		t.Fatalf("failed to get abs path: %v", err)
	}

	t.Setenv("GO_LLM_CONFIG", absPath)

	cfg, err := Default()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(cfg.Providers); got != 1 {
		t.Errorf("providers: got %d, want 1", got)
	}
	if got := len(cfg.Models); got != 5 {
		t.Errorf("models: got %d, want 5", got)
	}
}

func TestDefault_WorkingDir(t *testing.T) {
	// Copy valid.json into a temp dir as models.json.
	src, err := os.ReadFile("testdata/valid.json")
	if err != nil {
		t.Fatalf("failed to read valid.json: %v", err)
	}
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "models.json")
	if err := os.WriteFile(dst, src, 0644); err != nil {
		t.Fatalf("failed to write models.json: %v", err)
	}

	// Unset env var so it doesn't take precedence.
	_ = os.Unsetenv("GO_LLM_CONFIG")

	// Chdir into the temp dir.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cfg, err := Default()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(cfg.Models); got != 5 {
		t.Errorf("models: got %d, want 5", got)
	}
}

func TestDefault_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	// Unset env var.
	_ = os.Unsetenv("GO_LLM_CONFIG")
	// Set HOME to empty temp dir so ~/.config/go-llm/models.json won't be found.
	t.Setenv("HOME", tmpDir)

	// Chdir into the empty temp dir.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working dir: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	_, err = Default()
	if err == nil {
		t.Fatal("expected Default() to return error when no config found")
	}
	if !contains(err.Error(), "config: no configuration file found") {
		t.Errorf("error = %q, want substring %q", err.Error(), "config: no configuration file found")
	}
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("errors.Is(err, ErrConfigNotFound) = false, want true")
	}
}

func TestDefault_EmptyEnvVar(t *testing.T) {
	t.Setenv("GO_LLM_CONFIG", "")
	_, err := Default()
	if err == nil {
		t.Fatal("expected error when GO_LLM_CONFIG is set but empty")
	}
	if !contains(err.Error(), "GO_LLM_CONFIG is set but empty") {
		t.Errorf("error = %q, want substring about empty GO_LLM_CONFIG", err.Error())
	}
}

func TestLoad_RootModelsJSON(t *testing.T) {
	cfg, err := Load("../models.json")
	if err != nil {
		t.Fatalf("Load(root models.json) error: %v", err)
	}
	if cfg.ModelFor("chat") == "" {
		t.Error("expected chat model from root models.json")
	}
	if len(cfg.Models) != 7 {
		t.Errorf("expected 7 models, got %d", len(cfg.Models))
	}
}

func TestExpandAPIKeyRefs(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		value    string
		env      map[string]string
		want     string
		wantErr  string // substring; "" means no error
	}{
		{name: "literal unchanged", provider: "p", value: "sk-literal", want: "sk-literal"},
		{name: "empty unchanged", provider: "p", value: "", want: ""},
		{name: "dollar without brace literal", provider: "p", value: "a$b", want: "a$b"},
		{name: "whole value expands", provider: "p", value: "${K}", env: map[string]string{"K": "sk-1"}, want: "sk-1"},
		{name: "embedded expands", provider: "p", value: "Bearer-${K}", env: map[string]string{"K": "tok"}, want: "Bearer-tok"},
		{name: "multiple refs", provider: "p", value: "${A}-${B}", env: map[string]string{"A": "x", "B": "y"}, want: "x-y"},
		{name: "unset var errors", provider: "openai", value: "${GO_LLM_TEST_MISSING_API_KEY}", wantErr: `provider "openai" api_key references unset or empty environment variable "GO_LLM_TEST_MISSING_API_KEY"`},
		{name: "empty var errors", provider: "openai", value: "${E}", env: map[string]string{"E": ""}, wantErr: `references unset or empty environment variable "E"`},
		{name: "malformed empty name", provider: "p", value: "${}", wantErr: "malformed environment reference"},
		{name: "unterminated", provider: "p", value: "${K", wantErr: "malformed environment reference"},
		{name: "illegal char", provider: "p", value: "${A B}", wantErr: "malformed environment reference"},
		{name: "leading digit invalid", provider: "p", value: "${1A}", wantErr: "malformed environment reference"},
		{name: "adjacent refs no separator", provider: "p", value: "${A}${B}", env: map[string]string{"A": "x", "B": "y"}, want: "xy"},
		{name: "non-ascii name invalid", provider: "p", value: "${KÉY}", wantErr: "malformed environment reference"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.name == "unset var errors" {
				old, hadOld := os.LookupEnv("GO_LLM_TEST_MISSING_API_KEY")
				if err := os.Unsetenv("GO_LLM_TEST_MISSING_API_KEY"); err != nil {
					t.Fatalf("unset test env: %v", err)
				}
				t.Cleanup(func() {
					if hadOld {
						_ = os.Setenv("GO_LLM_TEST_MISSING_API_KEY", old)
					} else {
						_ = os.Unsetenv("GO_LLM_TEST_MISSING_API_KEY")
					}
				})
			}
			got, err := expandAPIKeyRefs(tc.provider, tc.value)
			if tc.wantErr != "" {
				if err == nil || !contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_APIKeyEnvExpansion(t *testing.T) {
	t.Setenv("GOLEM_TEST_KEY", "sk-secret-123")
	cfg := `{
  "providers": {
    "hosted": {"base_url": "https://api.openai.com", "api_format": "openai-compat", "api_key": "${GOLEM_TEST_KEY}"}
  },
  "models": {"agent": {"name": "gpt-4o", "provider": "hosted", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`
	path := writeTempJSON(t, cfg)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := loaded.Providers["hosted"].APIKey; got != "sk-secret-123" {
		t.Errorf("api_key = %q, want expanded %q", got, "sk-secret-123")
	}
}

func TestLoad_APIKeyEnvExpansion_UnsetErrors(t *testing.T) {
	cfg := `{
  "providers": {
    "hosted": {"base_url": "https://api.openai.com", "api_format": "openai-compat", "api_key": "${GOLEM_TEST_MISSING}"}
  },
  "models": {"agent": {"name": "gpt-4o", "provider": "hosted", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`
	path := writeTempJSON(t, cfg)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unset env var, got nil")
	}
	if !contains(err.Error(), `provider "hosted"`) || !contains(err.Error(), "GOLEM_TEST_MISSING") {
		t.Errorf("error = %q, want provider name and var name", err.Error())
	}
}

func TestLoad_LiteralAPIKeyUnchanged(t *testing.T) {
	cfg := `{
  "providers": {
    "hosted": {"base_url": "https://api.openai.com", "api_format": "openai-compat", "api_key": "sk-plain"}
  },
  "models": {"agent": {"name": "gpt-4o", "provider": "hosted", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`
	path := writeTempJSON(t, cfg)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := loaded.Providers["hosted"].APIKey; got != "sk-plain" {
		t.Errorf("api_key = %q, want unchanged literal", got)
	}
}
