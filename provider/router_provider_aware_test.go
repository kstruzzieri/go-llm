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
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion", "embedding"}},
		},
		chatResp:  &ChatResponse{Model: "qwen3:8b", Content: "from A", Done: true},
		genResp:   &GenerateResponse{Model: "qwen3:8b", Response: "gen-from-A", Done: true},
		embedResp: &EmbedResponse{Model: "qwen3:8b", Embeddings: [][]float64{{0.1, 0.2, 0.3}}, Provider: "ollama-a"},
	}
	provB := &rtMockProvider{
		name: "ollama-b",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion", "embedding"}},
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

// TestRoute_EmptyModel_ProviderScopesRecommend verifies that an empty Model
// with a non-empty Provider restricts the Recommend candidate set to that
// provider, even when another provider has capable models too.
func TestRoute_EmptyModel_ProviderScopesRecommend(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		UseCase:      "chat",
		Provider:     "ollama-b",
		RequiredCaps: CapChat,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama-b" {
		t.Errorf("plan.Profile.Key.Provider = %q, want ollama-b", plan.Profile.Key.Provider)
	}
}

// TestRoute_UnqualifiedModel_ProviderResolvesByKey verifies that an
// unqualified Model with a Provider pins resolution to ModelKey{Provider,
// Model} via Lookup, bypassing LookupAny's multi-provider scan. Pins
// ollama-b specifically so it fails clearly before the provider-aware
// implementation if Lookup-by-key is not used.
func TestRoute_UnqualifiedModel_ProviderResolvesByKey(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:        "qwen3:8b",
		Provider:     "ollama-b",
		UseCase:      "chat",
		RequiredCaps: CapChat,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama-b" {
		t.Errorf("plan.Profile.Key.Provider = %q, want ollama-b", plan.Profile.Key.Provider)
	}
	if plan.Model != "qwen3:8b" {
		t.Errorf("plan.Model = %q, want qwen3:8b", plan.Model)
	}
}

// TestRoute_QualifiedModel_MatchingProviderAccepted verifies that a Provider
// matching the qualified Model prefix is accepted (parity with qualified-only).
func TestRoute_QualifiedModel_MatchingProviderAccepted(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:        "ollama-a/qwen3:8b",
		Provider:     "ollama-a",
		UseCase:      "chat",
		RequiredCaps: CapChat,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama-a" {
		t.Errorf("plan.Profile.Key.Provider = %q, want ollama-a", plan.Profile.Key.Provider)
	}
}

// TestStickyKey_IncludesProvider verifies that two provider-scoped requests
// with the same affinity/model/use-case get distinct sticky slots. Candidate
// filtering prevents wrong routing either way, but without Provider in the
// sticky key these requests churn the same sticky cache entry and lose
// per-provider affinity.
func TestStickyKey_IncludesProvider(t *testing.T) {
	router, _, _ := setupTwoProviderRouter(t)

	for _, providerName := range []string{"ollama-a", "ollama-b"} {
		_, err := router.Route(context.Background(), RoutingRequest{
			Model:        "qwen3:8b",
			Provider:     providerName,
			AffinityKey:  "session-1",
			UseCase:      "chat",
			RequiredCaps: CapChat,
		})
		if err != nil {
			t.Fatalf("Route(%s): %v", providerName, err)
		}
	}

	if got := len(router.StickyRoutes()); got != 2 {
		t.Fatalf("sticky route count = %d, want 2 provider-scoped entries", got)
	}
}

// TestChat_ProviderForwarded verifies that ChatRequest.Provider is forwarded
// into the routing request so that the Chat convenience method honors the
// per-request provider pin. Uses two providers with the same model name and
// asserts the response content matches the pinned provider's mock content
// AND that Provider was NOT stamped onto the concrete execution request
// (the provider already knows its identity by the time Chat is called).
func TestChat_ProviderForwarded(t *testing.T) {
	router, _, provB := setupTwoProviderRouter(t)

	resp, err := router.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Provider: "ollama-b",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from B" {
		t.Errorf("resp.Content = %q, want %q (provB pinned)", resp.Content, "from B")
	}
	if provB.getChatCalls() != 1 {
		t.Errorf("provB.chatCalls = %d, want 1", provB.getChatCalls())
	}
	if got := provB.getLastChatRequest().Provider; got != "" {
		t.Errorf("executed ChatRequest.Provider = %q, want empty (selection metadata must not be forwarded)", got)
	}
}

// TestChatStream_ProviderForwarded mirrors TestChat_ProviderForwarded for
// the streaming Chat convenience method.
func TestChatStream_ProviderForwarded(t *testing.T) {
	router, _, provB := setupTwoProviderRouter(t)

	var got ChatResponse
	err := router.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Provider: "ollama-b",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(resp ChatResponse) error {
		got = resp
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.Content != "from B" {
		t.Errorf("stream content = %q, want %q", got.Content, "from B")
	}
	if provB.getChatCalls() != 1 {
		t.Errorf("provB.chatCalls = %d, want 1", provB.getChatCalls())
	}
	if got := provB.getLastChatRequest().Provider; got != "" {
		t.Errorf("executed ChatStream request Provider = %q, want empty", got)
	}
}

// TestGenerate_ProviderForwarded mirrors TestChat_ProviderForwarded for
// the Generate convenience method.
func TestGenerate_ProviderForwarded(t *testing.T) {
	router, _, provB := setupTwoProviderRouter(t)

	resp, err := router.Generate(context.Background(), GenerateRequest{
		Model:    "qwen3:8b",
		Provider: "ollama-b",
		Prompt:   "hi",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Response != "gen-from-B" {
		t.Errorf("resp.Response = %q, want %q", resp.Response, "gen-from-B")
	}
	if provB.getGenCalls() != 1 {
		t.Errorf("provB.genCalls = %d, want 1", provB.getGenCalls())
	}
	if got := provB.getLastGenerateRequest().Provider; got != "" {
		t.Errorf("executed GenerateRequest.Provider = %q, want empty (selection metadata must not be forwarded)", got)
	}
}

// TestGenerateStream_ProviderForwarded mirrors TestGenerate_ProviderForwarded
// for the streaming Generate convenience method.
func TestGenerateStream_ProviderForwarded(t *testing.T) {
	router, _, provB := setupTwoProviderRouter(t)

	var got GenerateResponse
	err := router.GenerateStream(context.Background(), GenerateRequest{
		Model:    "qwen3:8b",
		Provider: "ollama-b",
		Prompt:   "hi",
	}, func(resp GenerateResponse) error {
		got = resp
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if got.Response != "gen-from-B" {
		t.Errorf("stream response = %q, want %q", got.Response, "gen-from-B")
	}
	if provB.getGenCalls() != 1 {
		t.Errorf("provB.genCalls = %d, want 1", provB.getGenCalls())
	}
	if got := provB.getLastGenerateRequest().Provider; got != "" {
		t.Errorf("executed GenerateStream request Provider = %q, want empty", got)
	}
}

// TestEmbed_ProviderForwarded mirrors the others for Embed.
func TestEmbed_ProviderForwarded(t *testing.T) {
	router, _, provB := setupTwoProviderRouter(t)

	resp, err := router.Embed(context.Background(), EmbedRequest{
		Model:    "qwen3:8b",
		Provider: "ollama-b",
		Input:    []string{"x"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Provider != "ollama-b" {
		t.Errorf("resp.Provider = %q, want ollama-b", resp.Provider)
	}
	if provB.getEmbedCalls() != 1 {
		t.Errorf("provB.embedCalls = %d, want 1", provB.getEmbedCalls())
	}
	if got := provB.getLastEmbedRequest().Provider; got != "" {
		t.Errorf("executed EmbedRequest.Provider = %q, want empty (selection metadata must not be forwarded)", got)
	}
}

// TestRoute_ProviderMatrix is the table-driven contract test consolidating
// every (Model × Provider × PreferredChain) combination Router.Route must
// honor. New scoping cases land here, not in scattered tests.
func TestRoute_ProviderMatrix(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		provider    string
		chain       []string
		wantProv    string
		wantErrSent error // nil for success, otherwise errors.Is target
	}{
		{
			name:     "empty_model_empty_provider_picks_any",
			model:    "",
			provider: "",
			wantProv: "", // either; assert via err==nil only
		},
		{
			name:     "empty_model_provider_set_scopes_recommend",
			model:    "",
			provider: "ollama-b",
			wantProv: "ollama-b",
		},
		{
			name:     "unqualified_model_empty_provider_lookupany",
			model:    "qwen3:8b",
			provider: "",
			wantProv: "", // either; assert via err==nil only
		},
		{
			name:     "unqualified_model_provider_set_pins_lookup",
			model:    "qwen3:8b",
			provider: "ollama-a",
			wantProv: "ollama-a",
		},
		{
			name:     "qualified_model_empty_provider_unchanged",
			model:    "ollama-b/qwen3:8b",
			provider: "",
			wantProv: "ollama-b",
		},
		{
			name:     "qualified_model_matching_provider_accepted",
			model:    "ollama-a/qwen3:8b",
			provider: "ollama-a",
			wantProv: "ollama-a",
		},
		{
			name:        "qualified_model_conflicting_provider_rejected",
			model:       "ollama-a/qwen3:8b",
			provider:    "ollama-b",
			wantErrSent: ErrProviderMismatch,
		},
		{
			name:     "chain_set_provider_ignored_no_conflict_error",
			model:    "ollama-a/qwen3:8b",
			provider: "ollama-b",
			chain:    []string{"ollama-a/qwen3:8b"},
			wantProv: "ollama-a",
		},
		{
			name:     "chain_set_empty_model_provider_still_ignored",
			model:    "",
			provider: "ollama-b",
			chain:    []string{"ollama-a/qwen3:8b"},
			wantProv: "ollama-a",
		},
		{
			name:     "chain_recommend_tail_ignores_provider",
			model:    "",
			provider: "nonexistent",
			chain:    []string{"missing/qwen3:8b"},
			wantProv: "ollama-a", // non-strict tail is unrestricted by Provider
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, _, _ := setupTwoProviderRouter(t)
			plan, err := router.Route(context.Background(), RoutingRequest{
				Model:          tc.model,
				Provider:       tc.provider,
				PreferredChain: tc.chain,
				UseCase:        "chat",
				RequiredCaps:   CapChat,
			})
			if tc.wantErrSent != nil {
				if !errors.Is(err, tc.wantErrSent) {
					t.Fatalf("err = %v, want errors.Is(%v)", err, tc.wantErrSent)
				}
				return
			}
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if tc.wantProv != "" && plan.Profile.Key.Provider != tc.wantProv {
				t.Errorf("plan.Profile.Key.Provider = %q, want %q", plan.Profile.Key.Provider, tc.wantProv)
			}
		})
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
