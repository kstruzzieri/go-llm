package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
)

func TestResolveModelExplicit(t *testing.T) {
	s := &Server{
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModel(context.Background(), "explicit-model", "chat")
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

	got, err := s.resolveModel(context.Background(), "", "chat")
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

	_, err := s.resolveModel(context.Background(), "", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "model parameter required") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "model parameter required")
	}
}

func TestResolveModelDefaultsUnavailable(t *testing.T) {
	s := &Server{
		cfg:      &config.Config{}, // non-nil config
		resolved: make(map[string]config.ResolvedModel),
	}

	_, err := s.resolveModel(context.Background(), "", "chat")
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

	_, err := s.resolveModel(context.Background(), "", "embedding")
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

	_, err := s.resolveModel(context.Background(), "", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "did not resolve") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "did not resolve")
	}
}

func TestResolveModelRefreshesDefaultsAfterOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3-coder-next:latest"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := &Server{
		client: ollama.NewClient(ollama.WithBaseURL(srv.URL)),
		cfg: &config.Config{
			Defaults: map[string]string{"completion": "coding"},
			Models: map[string]config.ModelConfig{
				"coding": {Name: "qwen3-coder-next:latest", Provider: "ollama"},
			},
		},
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModel(context.Background(), "", "completion")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "qwen3-coder-next:latest" {
		t.Fatalf("resolveModel() = %q, want %q", got, "qwen3-coder-next:latest")
	}
	if len(s.resolved) == 0 {
		t.Fatal("resolved cache was not refreshed")
	}
}
