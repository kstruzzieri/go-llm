package config

import (
	"encoding/json"
	"os"
	"path/filepath"
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
