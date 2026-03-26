package mcp

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func TestResolveModelExplicit(t *testing.T) {
	s := &Server{
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModel("explicit-model", "chat")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "explicit-model" {
		t.Errorf("resolveModel() = %q, want %q", got, "explicit-model")
	}
}

func TestResolveModelFromCache(t *testing.T) {
	s := &Server{
		cfg: &config.Config{}, // non-nil config
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "qwen2.5:72b", Role: "large-chat"},
		},
	}

	got, err := s.resolveModel("", "chat")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "qwen2.5:72b" {
		t.Errorf("resolveModel() = %q, want %q", got, "qwen2.5:72b")
	}
}

func TestResolveModelNoConfigNoExplicit(t *testing.T) {
	s := &Server{
		cfg:      nil,
		resolved: make(map[string]config.ResolvedModel),
	}

	_, err := s.resolveModel("", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "resolve model: model parameter required") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "resolve model: model parameter required")
	}
}

func TestResolveModelDefaultsUnavailable(t *testing.T) {
	s := &Server{
		cfg:      &config.Config{}, // non-nil config
		resolved: make(map[string]config.ResolvedModel),
	}

	_, err := s.resolveModel("", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "defaults unavailable") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "defaults unavailable")
	}
}

func TestResolveModelUseCaseNotConfigured(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "qwen2.5:72b", Role: "large-chat"},
		},
	}

	_, err := s.resolveModel("", "embedding")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "no model configured for use-case") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "no model configured for use-case")
	}
}

func TestResolveModelDidNotResolve(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "", Role: "large-chat"},
		},
	}

	_, err := s.resolveModel("", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "did not resolve") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "did not resolve")
	}
}
