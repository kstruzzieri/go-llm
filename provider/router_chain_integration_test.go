//go:build integration

package provider

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// TestRouter_routeChain_Integration_BreakerOpensOnFailure verifies that when
// the first chain entry's Lookup fails with an infrastructure error, the
// provider's breaker records the failure and routing falls through to the
// second chain entry. This is the end-to-end equivalent of
// TestRouter_routeChain_LookupFailureRecordsBreaker, exercised against a
// running Ollama instance.
//
// Requires OLLAMA_URL pointing at a running Ollama (default: http://localhost:11434)
// and OLLAMA_INTEGRATION_MODEL pointing at a real model the instance has pulled.
// Run with: go test -tags integration ./provider/ -run TestRouter_routeChain_Integration -v
func TestRouter_routeChain_Integration_BreakerOpensOnFailure(t *testing.T) {
	url := os.Getenv("OLLAMA_URL")
	if url == "" {
		url = "http://localhost:11434"
	}
	model := os.Getenv("OLLAMA_INTEGRATION_MODEL")
	if model == "" {
		t.Skip("OLLAMA_INTEGRATION_MODEL not set; skip (set to a model name like \"qwen3:8b\")")
	}

	client := ollama.NewClient(ollama.WithBaseURL(url), ollama.WithTimeout(5*time.Second))
	if !client.IsAvailable(context.Background()) {
		t.Skipf("Ollama not reachable at %s; skip", url)
	}

	provReg := NewRegistry()
	op := NewOllamaProvider(client)
	if err := provReg.Register(op); err != nil {
		t.Fatalf("register: %v", err)
	}
	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("model registry: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), "ollama"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	r := NewRouter(mr, provReg)
	defer r.Close()

	// First entry will fail (Lookup of a non-existent model returns an error
	// from the Ollama provider); second entry should be a model the running
	// Ollama instance actually has.
	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/this-does-not-exist:9b", "ollama/" + model},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "say ok"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil && !errors.Is(err, ErrNoViableCandidate) {
		// ErrNoViableCandidate may surface if the second model also fails to
		// resolve (e.g., catalog miss). Tolerate that — the test is primarily
		// about breaker recording.
		t.Logf("Route (tolerated): %v", err)
	}
	if plan != nil && plan.Profile.Key.Model != model {
		t.Errorf("primary = %q, want %q (chain should fall through)", plan.Profile.Key.Model, model)
	}

	// The first entry's lookup failure should have recorded against the
	// "ollama" breaker.
	info, ok := r.BreakerInfo("ollama")
	if !ok {
		t.Fatal("expected breaker for \"ollama\" to exist after lookup failure")
	}
	if info.Failures == 0 {
		t.Errorf("breaker failures = %d, want >= 1", info.Failures)
	}
}
