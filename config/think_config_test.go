package config

import "testing"

// TestLoad_ThinkModeValidation covers the think_mode and think_tags
// per-model overrides: valid enum values load and normalize to lowercase,
// unknown values fail loud (user config, unlike the lenient embedded-catalog
// parser), and think_tags requires both delimiters to be set and distinct.
func TestLoad_ThinkModeValidation(t *testing.T) {
	tests := []struct {
		name        string
		fragment    string
		wantErr     bool
		errContains string
		checkMode   string // expected cfg.Models["chat"].ThinkMode, checked when non-empty
	}{
		{
			name:      "valid toggle",
			fragment:  `"think_mode": "toggle"`,
			checkMode: "toggle",
		},
		{
			name:      "valid uppercase normalized",
			fragment:  `"think_mode": "NONE"`,
			checkMode: "none",
		},
		{
			name:        "invalid enum",
			fragment:    `"think_mode": "maybe"`,
			wantErr:     true,
			errContains: "think_mode",
		},
		{
			name:     "tags both set",
			fragment: `"think_tags": {"open": "<r>", "close": "</r>"}`,
		},
		{
			name:        "tags missing close",
			fragment:    `"think_tags": {"open": "<r>"}`,
			wantErr:     true,
			errContains: "think_tags",
		},
		{
			name:        "tags equal",
			fragment:    `"think_tags": {"open": "<r>", "close": "<r>"}`,
			wantErr:     true,
			errContains: "think_tags",
		},
		{
			name:     "tags without mode allowed",
			fragment: `"think_tags": {"open": "<think>", "close": "</think>"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempJSON(t, `{
				"providers": {"ollama": {"base_url": "http://localhost:11434"}},
				"models": {"chat": {"name": "x", "type": "dense", `+tt.fragment+`}},
				"defaults": {}
			}`)
			cfg, err := Load(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !contains(err.Error(), tt.errContains) {
					t.Errorf("error %q does not mention %q", err.Error(), tt.errContains)
				}
				if !contains(err.Error(), "chat") {
					t.Errorf("error %q does not name the model/role", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.checkMode != "" {
				m := cfg.Models["chat"]
				if m.ThinkMode != tt.checkMode {
					t.Errorf("ThinkMode: got %q, want %q", m.ThinkMode, tt.checkMode)
				}
			}
		})
	}
}
