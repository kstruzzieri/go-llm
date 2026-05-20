package provider

import (
	"context"
	"testing"
)

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
