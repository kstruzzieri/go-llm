package main

import (
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func TestNewPreflightEndpointResolver(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama":   {BaseURL: "http://localhost:11434", APIFormat: "ollama"},
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", APIFormat: "openai-compat"},
		"legacy":   {BaseURL: "http://legacy:1", APIFormat: ""}, // empty => ollama
	}}

	tests := []struct {
		name     string
		cfg      *config.Config
		override string
		provider string
		wantOK   bool
		wantURL  string
		wantPath string
	}{
		{"openai-compat path", cfg, "", "llamacpp", true, "http://127.0.0.1:8080", "/v1/models"},
		{"ollama path", cfg, "", "ollama", true, "http://localhost:11434", "/api/tags"},
		{"empty api_format treated as ollama", cfg, "", "legacy", true, "http://legacy:1", "/api/tags"},
		{"ollama override applied", cfg, "http://override:9", "ollama", true, "http://override:9", "/api/tags"},
		{"override ignored for non-ollama", cfg, "http://override:9", "llamacpp", true, "http://127.0.0.1:8080", "/v1/models"},
		{"absent provider", cfg, "", "ghost", false, "", ""},
		{"nil config", nil, "", "ollama", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newPreflightEndpointResolver(tt.cfg, tt.override)
			ep, ok := r(tt.provider)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if ep.BaseURL != tt.wantURL || ep.ModelsPath != tt.wantPath {
				t.Errorf("ep = %+v, want {BaseURL:%q ModelsPath:%q}", ep, tt.wantURL, tt.wantPath)
			}
		})
	}
}

func TestResolvePreflightEndpoint_NilResolver(t *testing.T) {
	if _, ok := resolvePreflightEndpoint(nil, "ollama"); ok {
		t.Error("nil resolver should report ok=false")
	}
	r := newPreflightEndpointResolver(&config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://x:1", APIFormat: "ollama"},
	}}, "")
	if _, ok := resolvePreflightEndpoint(r, "ollama"); !ok {
		t.Error("present provider should report ok=true")
	}
}
