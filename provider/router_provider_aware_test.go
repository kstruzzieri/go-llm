package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// setupTwoProviderRouter wires a Router with two distinct provider instances,
// each advertising overlapping model names so the Provider field is the only
// thing that distinguishes them. Returns the router and both mock providers.
func setupTwoProviderRouter(t *testing.T) (*Router, *rtMockProvider, *rtMockProvider) {
	t.Helper()

	provA := &rtMockProvider{
		name: "ollama-a",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion"}},
		},
		chatResp:  &ChatResponse{Model: "qwen3:8b", Content: "from A", Done: true},
		genResp:   &GenerateResponse{Model: "qwen3:8b", Response: "gen-from-A", Done: true},
		embedResp: &EmbedResponse{Model: "qwen3:8b", Embeddings: [][]float64{{0.1, 0.2, 0.3}}, Provider: "ollama-a"},
	}
	provB := &rtMockProvider{
		name: "ollama-b",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion"}},
		},
		chatResp:  &ChatResponse{Model: "qwen3:8b", Content: "from B", Done: true},
		genResp:   &GenerateResponse{Model: "qwen3:8b", Response: "gen-from-B", Done: true},
		embedResp: &EmbedResponse{Model: "qwen3:8b", Embeddings: [][]float64{{0.4, 0.5, 0.6}}, Provider: "ollama-b"},
	}

	reg := NewRegistry()
	if err := reg.Register(provA); err != nil {
		t.Fatalf("Register provA: %v", err)
	}
	if err := reg.Register(provB); err != nil {
		t.Fatalf("Register provB: %v", err)
	}
	ctx := context.Background()
	if err := reg.RefreshModels(ctx, "ollama-a"); err != nil {
		t.Fatalf("RefreshModels a: %v", err)
	}
	if err := reg.RefreshModels(ctx, "ollama-b"); err != nil {
		t.Fatalf("RefreshModels b: %v", err)
	}

	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	router := NewRouter(mr, reg)
	t.Cleanup(func() { _ = router.Close() })
	return router, provA, provB
}

// TestRoute_QualifiedModel_ConflictingProviderRejected verifies that a
// qualified Model whose provider prefix disagrees with the Provider field
// returns ErrProviderMismatch with a message that names both identifiers,
// rejected BEFORE candidate resolution. Uses "missing/..." as the qualified
// prefix so any leak past the invariant check would surface as a Lookup
// error rather than this specific sentinel.
func TestRoute_QualifiedModel_ConflictingProviderRejected(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	_, err := router.Route(context.Background(), RoutingRequest{
		Model:        "missing/qwen3:8b",
		Provider:     "ollama-b",
		UseCase:      "chat",
		RequiredCaps: CapChat,
	})
	if !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("err = %v, want ErrProviderMismatch", err)
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "ollama-b") {
		t.Errorf("err message %q must name both provider identifiers", err.Error())
	}
}

// TestRoute_PreferredChain_SuppressesProviderFilter verifies that a non-empty
// PreferredChain makes the Provider field a no-op for conflict checking. The
// chain selectors own provider identity; the per-request Provider hint must
// not silently filter chain candidates.
func TestRoute_PreferredChain_SuppressesProviderFilter(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:          "ollama-a/qwen3:8b",
		PreferredChain: []string{"ollama-a/qwen3:8b"},
		Provider:       "ollama-b", // would conflict if checked, but chain wins
		UseCase:        "chat",
		RequiredCaps:   CapChat,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama-a" {
		t.Errorf("plan.Profile.Key.Provider = %q, want ollama-a", plan.Profile.Key.Provider)
	}
}

// TestRoutingRequest_ProviderField_Preserved asserts that a RoutingRequest
// carrying a Provider that matches the qualified Model routes successfully
// (no error, no behavior change) — the field exists, is wired through, and
// does not break the existing qualified-Model path.
func TestRoutingRequest_ProviderField_Preserved(t *testing.T) {
	router, _ := setupTestRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:    "test/qwen3:8b",
		Provider: "test",
		UseCase:  "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan.Profile.Key.Provider != "test" {
		t.Errorf("plan.Profile.Key.Provider = %q, want %q", plan.Profile.Key.Provider, "test")
	}
}
