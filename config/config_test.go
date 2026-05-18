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

	// ProviderFor defaults to "ollama" for programmatically constructed configs
	// where the Provider field was never materialized by Load.
	manualCfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: "http://manual:11434"},
		},
		Models: map[string]ModelConfig{
			"test": {Name: "m", Type: "dense"}, // no Provider set
		},
	}
	mp := manualCfg.ProviderFor("test")
	if mp == nil {
		t.Fatal("expected ProviderFor to default to ollama for empty Provider field")
	}
	if mp.BaseURL != "http://manual:11434" {
		t.Errorf("BaseURL = %q, want %q", mp.BaseURL, "http://manual:11434")
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
	if len(cfg.Models) != 6 {
		t.Errorf("expected 6 models, got %d", len(cfg.Models))
	}
}
