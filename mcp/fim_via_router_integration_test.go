//go:build integration

package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestFIMViaRouter_Smoke is the end-to-end falsifiable smoke test for the
// FIM-via-Router wiring: it constructs a real *Server, wraps the live
// router engine in a recording shim, drives newCompletionProvider through
// to Complete against a real Ollama instance, and asserts the routing
// trail (UseCase = "fim", RequiredCaps include CapInsert, Model pinned
// per the FIM-pin policy, Priority forwarded as PriorityHigh).
//
// Skip semantics:
//   - OLLAMA_FIM_INTEGRATION_MODEL unset → skip with explicit reason.
//   - Ollama unreachable at OLLAMA_URL (or default) → skip with explicit
//     reachability message.
//   - Server lacks a router (e.g., NewServer fell back to single-provider
//     mode due to an unusual config) → skip with explicit unavailability
//     message.
//
// No unconditional skip is allowed: every skip path documents WHY.
func TestFIMViaRouter_Smoke(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("OLLAMA_URL")
	if url == "" {
		url = defaultOllamaURL
	}
	model := os.Getenv("OLLAMA_FIM_INTEGRATION_MODEL")
	if model == "" {
		t.Skip("OLLAMA_FIM_INTEGRATION_MODEL not set; skip (set to a native-FIM model such as qwen3-coder-next:latest)")
	}

	client := ollama.NewClient(ollama.WithBaseURL(url), ollama.WithTimeout(30*time.Second))
	if !client.IsAvailable(ctx) {
		t.Skipf("Ollama not reachable at %s; skip", url)
	}

	cfgPath := writeFIMIntegrationConfig(t, t.TempDir(), url, model)
	s, err := NewServer(ctx, WithConfig(cfgPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	inner := s.routerSnapshot()
	if inner == nil {
		t.Skip("router unavailable; skip (NewServer did not wire a routeEngine — likely a single-provider degraded mode)")
	}
	rec := &recordingRealRouteEngine{inner: inner}
	s.mu.Lock()
	s.router = rec
	s.mu.Unlock()

	p, err := s.newCompletionProvider(ctx, model, "ollama")
	if err != nil {
		t.Fatalf("newCompletionProvider: %v", err)
	}
	// MaxTokens=8 is intentionally small — the smoke test only needs the
	// routing path to fire and stamp rec.last; we don't assert anything
	// about completion content. A small budget keeps the test fast even
	// when the integration model has a slow cold-start.
	if _, err := p.Complete(ctx, completion.FIMRequest{
		Prefix:    "func add(a, b int) int {\n\treturn ",
		Suffix:    "\n}",
		FilePath:  "math.go",
		MaxTokens: 8,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !rec.called {
		t.Fatal("router was not invoked")
	}
	if rec.last.UseCase != "fim" {
		t.Errorf("UseCase = %q, want fim", rec.last.UseCase)
	}
	wantCaps := provider.CapGenerate | provider.CapInsert
	if rec.last.RequiredCaps != wantCaps {
		t.Errorf("RequiredCaps = %v, want %v", rec.last.RequiredCaps, wantCaps)
	}
	if want := "ollama/" + model; rec.last.Model != want {
		t.Errorf("Model = %q, want %q", rec.last.Model, want)
	}
	if rec.last.Priority != provider.PriorityHigh {
		t.Errorf("Priority = %v, want %v", rec.last.Priority, provider.PriorityHigh)
	}
}

// recordingRealRouteEngine wraps a real routeEngine and captures the
// last RoutingRequest it observed, while still delegating execution to
// the wrapped engine. Used to assert routing-shape invariants
// end-to-end without bypassing the real Router's plan construction.
//
// Single-call fixture: last and called are written without a mutex.
// The current smoke test issues exactly one Complete call. A future
// test that exercises concurrent FIM completions through this wrapper
// must add its own synchronization or use a different fixture.
type recordingRealRouteEngine struct {
	inner  routeEngine
	last   provider.RoutingRequest
	called bool
}

func (r *recordingRealRouteEngine) Route(ctx context.Context, req provider.RoutingRequest) (*provider.RoutePlan, error) {
	r.last = req
	r.called = true
	return r.inner.Route(ctx, req)
}
func (r *recordingRealRouteEngine) Close() error { return r.inner.Close() }
func (r *recordingRealRouteEngine) BreakerInfo(name string) (provider.BreakerInfo, bool) {
	return r.inner.BreakerInfo(name)
}
func (r *recordingRealRouteEngine) WarmthSnapshot() []provider.WarmModel {
	return r.inner.WarmthSnapshot()
}
func (r *recordingRealRouteEngine) StickyRoutes() map[string]provider.StickyRouteInfo {
	return r.inner.StickyRoutes()
}

func writeFIMIntegrationConfig(t *testing.T, dir, baseURL, model string) string {
	t.Helper()
	path := filepath.Join(dir, "models.json")
	data := fmt.Sprintf(`{
  "providers": {"ollama": {"base_url": %q, "timeout": "30s"}},
  "models": {"completion": {"name": %q, "provider": "ollama", "type": "dense"}},
  "defaults": {"completion": "completion"}
}`, baseURL, model)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}
