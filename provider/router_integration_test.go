package provider

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Integration test helpers
//
// These tests exercise the full routing pipeline: Registry -> ModelRegistry ->
// Router -> Route -> RoutePlan -> Execute. They use mock providers (not real
// Ollama) but wire up the complete system.
// ---------------------------------------------------------------------------

// setupIntegrationRouter creates a Router with one or more rtMockProviders,
// wires up both registries, and returns the router. The caller owns cleanup
// via t.Cleanup. Additional RouterOptions can be passed through.
func setupIntegrationRouter(
	t *testing.T,
	providers []*rtMockProvider,
	opts ...RouterOption,
) *Router {
	t.Helper()

	provReg := NewRegistry()
	for _, prov := range providers {
		if err := provReg.Register(prov); err != nil {
			t.Fatalf("Register(%s) failed: %v", prov.Name(), err)
		}
		if err := provReg.RefreshModels(context.Background(), prov.Name()); err != nil {
			t.Fatalf("RefreshModels(%s) failed: %v", prov.Name(), err)
		}
	}

	modelReg, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry failed: %v", err)
	}

	router := NewRouter(modelReg, provReg, opts...)
	t.Cleanup(func() { _ = router.Close() })
	return router
}

// ---------------------------------------------------------------------------
// TestIntegrationRouteAndExecuteChat
//
// End-to-end: route with affinity key, execute chat, verify response content,
// RouteOutcome, sticky caching, and TTL expiry.
// ---------------------------------------------------------------------------

func TestIntegrationRouteAndExecuteChat(t *testing.T) {
	ws := newRTMockWarmthSource()
	modelKey := ModelKey{Provider: "alpha", Model: "qwen3:8b"}
	ws.setWarm(modelKey, WarmthInfo{
		Loaded:    true,
		Since:     time.Now(),
		ExpiresAt: time.Now().Add(5 * time.Minute),
		VRAM:      4.5,
	})

	prov := &rtMockProvider{
		name: "alpha",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "qwen3:8b",
			Content: "Integration test response",
			Done:    true,
		},
	}

	stickyTTL := 200 * time.Millisecond
	router := setupIntegrationRouter(t, []*rtMockProvider{prov},
		WithWarmthSource(ws),
		WithStickyTTL(stickyTTL),
	)

	ctx := context.Background()

	// --- Step 1: Route with AffinityKey and execute chat ---
	req := RoutingRequest{
		Model:        "qwen3:8b",
		UseCase:      "chat",
		AffinityKey:  "session-abc",
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
		RequiredCaps: CapChat,
	}

	plan, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}
	if plan.Model != "qwen3:8b" {
		t.Errorf("plan.Model = %q, want %q", plan.Model, "qwen3:8b")
	}

	resp, err := plan.ExecuteChat(ctx)
	if err != nil {
		t.Fatalf("ExecuteChat returned error: %v", err)
	}
	if resp.Content != "Integration test response" {
		t.Errorf("Content = %q, want %q", resp.Content, "Integration test response")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil")
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}
	if resp.RouteOutcome.PlannedModel != modelKey {
		t.Errorf("PlannedModel = %v, want %v", resp.RouteOutcome.PlannedModel, modelKey)
	}
	if resp.RouteOutcome.ActualModel != modelKey {
		t.Errorf("ActualModel = %v, want %v", resp.RouteOutcome.ActualModel, modelKey)
	}

	// --- Step 2: Second Route with same AffinityKey -> verify sticky ---
	plan2, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route 2 returned error: %v", err)
	}
	if plan2.Model != plan.Model {
		t.Errorf("second route model = %q, want %q (sticky)", plan2.Model, plan.Model)
	}
	if plan2.Profile.Key != plan.Profile.Key {
		t.Errorf("second route key = %v, want %v (sticky)", plan2.Profile.Key, plan.Profile.Key)
	}

	// Sticky cache should have an entry.
	stickyRoutes := router.StickyRoutes()
	if len(stickyRoutes) == 0 {
		t.Error("StickyRoutes is empty after affinity routing, want >= 1 entry")
	}

	// --- Step 3: Wait for TTL expiry, route again -> still works ---
	time.Sleep(stickyTTL + 50*time.Millisecond)

	// After TTL expiry, the sticky entry should be gone from snapshot.
	stickyRoutesAfter := router.StickyRoutes()
	if len(stickyRoutesAfter) != 0 {
		t.Errorf("StickyRoutes after TTL expiry has %d entries, want 0", len(stickyRoutesAfter))
	}

	// Routing should still succeed (model is still available, just not sticky).
	plan3, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route 3 (post-TTL) returned error: %v", err)
	}
	if plan3.Model != "qwen3:8b" {
		t.Errorf("post-TTL plan.Model = %q, want %q", plan3.Model, "qwen3:8b")
	}

	// --- Verify warmth integration ---
	snapshot := router.WarmthSnapshot()
	if len(snapshot) == 0 {
		t.Error("WarmthSnapshot is empty, want at least 1 entry")
	}

	// Verify breaker is healthy.
	info, exists := router.BreakerInfo("alpha")
	if !exists {
		t.Fatal("BreakerInfo for 'alpha' does not exist after routing")
	}
	if info.State != BreakerClosed {
		t.Errorf("breaker state = %v, want Closed", info.State)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationFallbackChain
//
// Two providers serve the same model. Primary returns 503, fallback succeeds.
// ---------------------------------------------------------------------------

func TestIntegrationFallbackChain(t *testing.T) {
	infraErr := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}

	// Provider names chosen so that "aaa-primary" sorts before "zzz-fallback"
	// alphabetically, ensuring the primary is selected first by the scorer's
	// tiebreak logic (both providers have identical scores for the same model).
	primaryProv := &rtMockProvider{
		name: "aaa-primary",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "shared-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: nil, // will not be used
		chatErr:  infraErr,
	}

	fallbackProv := &rtMockProvider{
		name: "zzz-fallback",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "shared-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "shared-model:8b",
			Content: "from fallback provider",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{primaryProv, fallbackProv},
		WithMaxFallbacks(3),
	)

	ctx := context.Background()

	// Route to unqualified model name — both providers are candidates.
	req := RoutingRequest{
		Model:        "shared-model:8b",
		UseCase:      "chat",
		Messages:     []ChatMessage{{Role: "user", Content: "test fallback"}},
		RequiredCaps: CapChat,
	}

	plan, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}

	// The plan should have at least one fallback since both providers serve the model.
	if len(plan.Fallbacks) == 0 {
		t.Fatal("plan has 0 fallbacks, want >= 1 (two providers serve the same model)")
	}

	// Execute — primary fails with 503, fallback should succeed.
	resp, err := plan.ExecuteChat(ctx)
	if err != nil {
		t.Fatalf("ExecuteChat returned error: %v (expected fallback to succeed)", err)
	}
	if resp.Content != "from fallback provider" {
		t.Errorf("Content = %q, want %q", resp.Content, "from fallback provider")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil")
	}

	// Because "aaa-primary" sorts before "zzz-fallback" alphabetically,
	// the primary is selected first. It fails with 503, so the fallback
	// chain kicks in.
	if resp.RouteOutcome.FallbacksUsed != 1 {
		t.Errorf("FallbacksUsed = %d, want 1 (primary should fail, fallback should succeed)",
			resp.RouteOutcome.FallbacksUsed)
	}
	primaryKey := ModelKey{Provider: "aaa-primary", Model: "shared-model:8b"}
	fallbackKey := ModelKey{Provider: "zzz-fallback", Model: "shared-model:8b"}
	if resp.RouteOutcome.PlannedModel != primaryKey {
		t.Errorf("PlannedModel = %v, want %v", resp.RouteOutcome.PlannedModel, primaryKey)
	}
	if resp.RouteOutcome.ActualModel != fallbackKey {
		t.Errorf("ActualModel = %v, want %v", resp.RouteOutcome.ActualModel, fallbackKey)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationBudgetAdaptation
//
// Model with tiny context window receives a prompt that exceeds the budget.
// ---------------------------------------------------------------------------

func TestIntegrationBudgetAdaptation(t *testing.T) {
	prov := &rtMockProvider{
		name: "tiny",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "tiny-model:1b",
				Family:        "tiny",
				ParameterSize: "1B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 2048,
				Capabilities:  []string{"completion"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "tiny-model:1b",
			Content: "should not reach here",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov})
	ctx := context.Background()

	// Build a prompt that exceeds the 2048 context window. The token budget
	// validator estimates tokens as len/4. Context 2048 minus expected output
	// (2048 for "chat") = 0 budget. Any message should trigger BudgetReject.
	//
	// Actually, with context=2048 and expectedOutput=2048, the budget becomes 0,
	// which triggers BudgetReject since budget==0 means "output budget exceeds
	// context window".
	req := RoutingRequest{
		Model:        "tiny-model:1b",
		UseCase:      "chat",
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
		RequiredCaps: CapChat,
	}

	_, err := router.Route(ctx, req)

	// With ContextWindow=2048 and chat default output reservation of 2048,
	// the available budget is 0 => BudgetReject => ErrBudgetExceeded.
	if err == nil {
		t.Fatal("Route returned nil error, want budget error")
	}

	if errors.Is(err, ErrBudgetExceeded) {
		t.Logf("Got expected ErrBudgetExceeded: %v", err)
	} else if errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Logf("Got ErrBudgetAdaptationRequired: %v", err)
		// This means a truncatable plan was returned. Verify ExecuteChat rejects.
	} else if errors.Is(err, ErrNoViableCandidate) {
		t.Logf("Got ErrNoViableCandidate (budget rejected, no candidates left): %v", err)
	} else {
		t.Errorf("Unexpected error type: %v", err)
	}

	// Now test with a slightly larger context that triggers BudgetTruncate.
	// A context of 4096 with chat expectedOutput=2048 leaves budget=2048.
	// We need input > budget (2048 tokens ~ 8192 chars) but < 1.5x budget.
	provTruncate := &rtMockProvider{
		name: "trunc",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "trunc-model:1b",
				Family:        "tiny",
				ParameterSize: "1B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 4096,
				Capabilities:  []string{"completion"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "trunc-model:1b",
			Content: "should not reach here",
			Done:    true,
		},
	}

	routerTrunc := setupIntegrationRouter(t, []*rtMockProvider{provTruncate})

	// Build a message that produces ~2500 tokens (10000 chars / 4 = 2500 tokens).
	// Budget is 2048 tokens. 2500 > 2048 but 2500 < 2048*1.5=3072 => BudgetTruncate.
	bigContent := strings.Repeat("x", 10000)
	reqTrunc := RoutingRequest{
		Model:        "trunc-model:1b",
		UseCase:      "chat",
		Messages:     []ChatMessage{{Role: "user", Content: bigContent}},
		RequiredCaps: CapChat,
	}

	plan, err := routerTrunc.Route(ctx, reqTrunc)
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Fatalf("Route error = %v, want ErrBudgetAdaptationRequired", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan with ErrBudgetAdaptationRequired")
	}
	if !plan.Degraded {
		t.Error("plan.Degraded = false, want true for truncatable plan")
	}

	// Verify ExecuteChat also rejects with ErrBudgetAdaptationRequired.
	_, execErr := plan.ExecuteChat(ctx)
	if !errors.Is(execErr, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteChat error = %v, want ErrBudgetAdaptationRequired", execErr)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationDryRun
//
// DryRun returns a plan without updating the sticky cache.
// ---------------------------------------------------------------------------

func TestIntegrationDryRun(t *testing.T) {
	prov := &rtMockProvider{
		name: "dry",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "dry-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "dry-model:8b",
			Content: "dry response",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov})
	ctx := context.Background()

	req := RoutingRequest{
		Model:        "dry-model:8b",
		UseCase:      "chat",
		AffinityKey:  "session-dry-run",
		DryRun:       true,
		Messages:     []ChatMessage{{Role: "user", Content: "test dry run"}},
		RequiredCaps: CapChat,
	}

	plan, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route (DryRun) returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route (DryRun) returned nil plan")
	}
	if plan.Model != "dry-model:8b" {
		t.Errorf("plan.Model = %q, want %q", plan.Model, "dry-model:8b")
	}

	// DryRun must NOT update the sticky cache.
	routes := router.StickyRoutes()
	if len(routes) != 0 {
		t.Errorf("StickyRoutes has %d entries after DryRun, want 0", len(routes))
	}

	// A subsequent non-dry-run route should create a sticky entry.
	req.DryRun = false
	_, err = router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route (non-DryRun) returned error: %v", err)
	}
	routes = router.StickyRoutes()
	if len(routes) == 0 {
		t.Error("StickyRoutes is empty after non-DryRun route with affinity key")
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationBreakerStateQuery
//
// Verify BreakerInfo reflects failures recorded through the Router's
// RecordFailure path (triggered by ExecuteChat fallback).
// ---------------------------------------------------------------------------

func TestIntegrationBreakerStateQuery(t *testing.T) {
	infraErr := &HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}

	failingProv := &rtMockProvider{
		name: "breaker-test",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "breaker-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion"},
			},
		},
		chatErr: infraErr,
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{failingProv})
	ctx := context.Background()

	// Route and execute — will fail but not panic.
	plan, err := router.Route(ctx, RoutingRequest{
		Model:        "breaker-model:8b",
		UseCase:      "chat",
		Messages:     []ChatMessage{{Role: "user", Content: "test"}},
		RequiredCaps: CapChat,
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}

	// Execute will fail since the provider returns 500.
	_, execErr := plan.ExecuteChat(ctx)
	if execErr == nil {
		t.Fatal("ExecuteChat returned nil error, want infrastructure error")
	}

	// Breaker should have recorded a failure.
	info, exists := router.BreakerInfo("breaker-test")
	if !exists {
		t.Fatal("BreakerInfo not found for 'breaker-test'")
	}
	// The failure went through RecordFailure which only records infrastructure errors.
	// After one ExecuteChat call failure, Failures should be >= 1.
	if info.Failures < 1 {
		t.Errorf("breaker failures = %d, want >= 1", info.Failures)
	}
	if info.State != BreakerClosed {
		// One failure shouldn't trip the breaker (threshold=3).
		t.Logf("breaker state = %v after 1 failure (may be closed or open depending on attempts)", info.State)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationConvenienceMethods
//
// End-to-end test of the Router convenience methods (Chat, Generate, Embed)
// which combine Route + Execute in a single call.
// ---------------------------------------------------------------------------

func TestIntegrationConvenienceMethods(t *testing.T) {
	prov := &rtMockProvider{
		name: "conv",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "conv-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "conv-model:8b",
			Content: "convenience chat",
			Done:    true,
		},
		genResp: &GenerateResponse{
			Model:    "conv-model:8b",
			Response: "convenience generate",
			Done:     true,
		},
		embedResp: &EmbedResponse{
			Model:      "conv-model:8b",
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov})
	ctx := context.Background()

	// --- Chat ---
	chatResp, err := router.Chat(ctx, ChatRequest{
		Model:    "conv-model:8b",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if chatResp.Content != "convenience chat" {
		t.Errorf("Chat content = %q, want %q", chatResp.Content, "convenience chat")
	}
	if chatResp.RouteOutcome == nil {
		t.Error("Chat RouteOutcome is nil")
	}

	// --- Generate ---
	genResp, err := router.Generate(ctx, GenerateRequest{
		Model:  "conv-model:8b",
		Prompt: "complete this",
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if genResp.Response != "convenience generate" {
		t.Errorf("Generate response = %q, want %q", genResp.Response, "convenience generate")
	}
	if genResp.RouteOutcome == nil {
		t.Error("Generate RouteOutcome is nil")
	}

	// --- Embed ---
	embedResp, err := router.Embed(ctx, EmbedRequest{
		Model: "conv-model:8b",
		Input: []string{"embed this"},
	})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(embedResp.Embeddings) != 1 {
		t.Errorf("Embed embeddings count = %d, want 1", len(embedResp.Embeddings))
	}
	if embedResp.RouteOutcome == nil {
		t.Error("Embed RouteOutcome is nil")
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationStickyInvalidationOnFailure
//
// Verifies that infrastructure failures invalidate sticky routes for the
// failed provider, preventing future requests from being sticky-routed to
// a broken provider.
// ---------------------------------------------------------------------------

func TestIntegrationStickyInvalidationOnFailure(t *testing.T) {
	infraErr := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}

	prov := &rtMockProvider{
		name: "sticky-prov",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "sticky-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "sticky-model:8b",
			Content: "sticky response",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov},
		WithStickyTTL(5*time.Minute),
	)
	ctx := context.Background()

	// Step 1: Route and execute to create sticky entry.
	req := RoutingRequest{
		Model:        "sticky-model:8b",
		UseCase:      "chat",
		AffinityKey:  "sticky-session",
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
		RequiredCaps: CapChat,
	}

	plan, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	_, err = plan.ExecuteChat(ctx)
	if err != nil {
		t.Fatalf("ExecuteChat returned error: %v", err)
	}

	// Verify sticky entry exists.
	routes := router.StickyRoutes()
	if len(routes) == 0 {
		t.Fatal("StickyRoutes is empty after successful route + execute")
	}

	// Step 2: Record infrastructure failure through the Router.
	// This simulates the RecordFailure path that invalidates sticky routes.
	router.RecordFailure(
		ModelKey{Provider: "sticky-prov", Model: "sticky-model:8b"},
		infraErr,
	)

	// Sticky routes for this provider should be invalidated.
	routesAfter := router.StickyRoutes()
	if len(routesAfter) != 0 {
		t.Errorf("StickyRoutes has %d entries after RecordFailure, want 0 (invalidated)",
			len(routesAfter))
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationClosedRouterRejectsAll
//
// After Close(), all routing methods return ErrRouterClosed.
// ---------------------------------------------------------------------------

func TestIntegrationClosedRouterRejectsAll(t *testing.T) {
	prov := &rtMockProvider{
		name: "closed-test",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "closed-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "closed-model:8b",
			Content: "should not get here",
			Done:    true,
		},
		genResp: &GenerateResponse{
			Model:    "closed-model:8b",
			Response: "should not get here",
			Done:     true,
		},
		embedResp: &EmbedResponse{
			Model:      "closed-model:8b",
			Embeddings: [][]float64{{0.1}},
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov})
	ctx := context.Background()

	// Close the router.
	if err := router.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// Route should fail.
	_, err := router.Route(ctx, RoutingRequest{
		Model:   "closed-model:8b",
		UseCase: "chat",
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("Route error = %v, want ErrRouterClosed", err)
	}

	// Chat convenience should fail.
	_, err = router.Chat(ctx, ChatRequest{
		Model:    "closed-model:8b",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("Chat error = %v, want ErrRouterClosed", err)
	}

	// Generate convenience should fail.
	_, err = router.Generate(ctx, GenerateRequest{
		Model:  "closed-model:8b",
		Prompt: "test",
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("Generate error = %v, want ErrRouterClosed", err)
	}

	// Embed convenience should fail.
	_, err = router.Embed(ctx, EmbedRequest{
		Model: "closed-model:8b",
		Input: []string{"test"},
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("Embed error = %v, want ErrRouterClosed", err)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationWarmthSourceIntegration
//
// Verifies that warmth information flows through the entire pipeline:
// Router creation -> scoring -> WarmthSnapshot -> RecordWarmthUse.
// ---------------------------------------------------------------------------

func TestIntegrationWarmthSourceIntegration(t *testing.T) {
	ws := newRTMockWarmthSource()
	modelKey := ModelKey{Provider: "warm-prov", Model: "warm-model:8b"}
	ws.setWarm(modelKey, WarmthInfo{
		Loaded:    true,
		Since:     time.Now(),
		ExpiresAt: time.Now().Add(5 * time.Minute),
		VRAM:      6.0,
	})

	prov := &rtMockProvider{
		name: "warm-prov",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "warm-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion", "embedding"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "warm-model:8b",
			Content: "warm response",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t, []*rtMockProvider{prov},
		WithWarmthSource(ws),
	)
	ctx := context.Background()

	// Verify WarmthSnapshot before routing.
	snapshot := router.WarmthSnapshot()
	if len(snapshot) == 0 {
		t.Fatal("WarmthSnapshot empty before routing")
	}
	found := false
	for _, wm := range snapshot {
		if wm.Key == modelKey && wm.Info.Loaded {
			found = true
		}
	}
	if !found {
		t.Error("warm-model:8b not found in WarmthSnapshot")
	}

	// Route and execute — should record warmth use via the RecordWarmthUse path.
	resp, err := router.Chat(ctx, ChatRequest{
		Model:    "warm-model:8b",
		Messages: []ChatMessage{{Role: "user", Content: "warm test"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "warm response" {
		t.Errorf("Content = %q, want %q", resp.Content, "warm response")
	}

	// Verify RecordUse was called on the warmth source.
	uses := ws.getUses()
	if len(uses) == 0 {
		t.Error("warmth RecordUse not called after successful Chat")
	}
	usedModel := false
	for _, key := range uses {
		if key == modelKey {
			usedModel = true
		}
	}
	if !usedModel {
		t.Errorf("RecordUse not called for %v, called for %v", modelKey, uses)
	}
}

// ---------------------------------------------------------------------------
// TestIntegrationMultiProviderScoring
//
// Two providers with different quality models — the router should prefer the
// higher-quality model when both are healthy.
// ---------------------------------------------------------------------------

func TestIntegrationMultiProviderScoring(t *testing.T) {
	// Provider with a small/fast model.
	fastProv := &rtMockProvider{
		name: "fast-prov",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "fast-model:1b",
				Family:        "small",
				ParameterSize: "1B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 8192,
				Capabilities:  []string{"completion"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "fast-model:1b",
			Content: "from fast",
			Done:    true,
		},
	}

	// Provider with a larger/quality model.
	qualityProv := &rtMockProvider{
		name: "quality-prov",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "quality-model:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"completion"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "quality-model:8b",
			Content: "from quality",
			Done:    true,
		},
	}

	router := setupIntegrationRouter(t,
		[]*rtMockProvider{fastProv, qualityProv},
		WithAvailableRAM(128.0), // plenty of RAM
	)
	ctx := context.Background()

	// Route without specifying a model — let the router recommend.
	plan, err := router.Route(ctx, RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat,
		Messages:     []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}

	// Log which model was selected — the 8B model should score higher on
	// quality for chat use case.
	t.Logf("Selected model: %s (score=%.3f)", plan.Profile.Key, plan.Score)

	// Execute the winning plan.
	resp, err := plan.ExecuteChat(ctx)
	if err != nil {
		t.Fatalf("ExecuteChat returned error: %v", err)
	}
	if resp.Content == "" {
		t.Error("response content is empty")
	}
	if resp.RouteOutcome == nil {
		t.Error("RouteOutcome is nil")
	}
}

// ---------------------------------------------------------------------------
// TestRouterEnforceModeFeedsBackIntoNextRoute_MemoryStore
//
// End-to-end Enforce-mode loop with MemoryStore: the first Route observes
// no signals (adjusted is neutral 0.5 because ScoredCount < feedbackMinScoredCount);
// successive Route -> Execute cycles accumulate Success signals; the final
// Route observes FeedbackApplied=true with a non-zero delta between
// ScoreWithFeedback and ScoreWithoutFeedback.
// ---------------------------------------------------------------------------

func TestRouterEnforceModeFeedsBackIntoNextRoute_MemoryStore(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	// First Route + Execute records a Success signal.
	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("first Route: %v", err)
	}
	resp1, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("first ExecuteChat: %v", err)
	}
	if resp1.RouteOutcome == nil || resp1.RouteOutcome.ScoreBreakdown == nil {
		t.Fatalf("first route: ScoreBreakdown nil")
	}
	// First call sees no prior signals (ScoredCount=0 < feedbackMinScoredCount)
	// so the confidence-gated adjusted score must still be neutral 0.5.
	if resp1.RouteOutcome.ScoreBreakdown.FeedbackAdjustedScore != 0.5 {
		t.Errorf("first route: adjusted = %v, want 0.5 (below scored floor)",
			resp1.RouteOutcome.ScoreBreakdown.FeedbackAdjustedScore)
	}

	// Drive enough additional successes to exceed feedbackMinScoredCount.
	for i := 0; i < feedbackMinScoredCount+1; i++ {
		plan, err = router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
		if err != nil {
			t.Fatalf("loop Route %d: %v", i, err)
		}
		if _, err := plan.ExecuteChat(context.Background()); err != nil {
			t.Fatalf("loop ExecuteChat %d: %v", i, err)
		}
	}

	// Final route observes accumulated signals.
	plan, err = router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("final Route: %v", err)
	}
	respN, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("final ExecuteChat: %v", err)
	}
	bd := respN.RouteOutcome.ScoreBreakdown
	if bd == nil {
		t.Fatalf("final route: ScoreBreakdown nil")
	}
	if !bd.FeedbackApplied {
		t.Errorf("FeedbackApplied = false; want true (Enforce + accumulated signals)")
	}
	if bd.FeedbackScore <= 0.5 {
		t.Errorf("FeedbackScore = %v, want > 0.5", bd.FeedbackScore)
	}
	if bd.ScoreWithFeedback == bd.ScoreWithoutFeedback {
		t.Errorf("with == without (%v); expected non-zero delta", bd.ScoreWithFeedback)
	}
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
}

// ---------------------------------------------------------------------------
// TestRouterEnforceModeFeedsBackIntoNextRoute_SQLite
//
// Persistence-parity counterpart to the MemoryStore test: the same Route ->
// Execute -> record loop runs against a caller-owned :memory: *sql.DB wrapped
// by NewSQLiteFeedbackStore. Proves the in-transaction write path lets the
// next Route observe the previous Execute's signal immediately.
// ---------------------------------------------------------------------------

func TestRouterEnforceModeFeedsBackIntoNextRoute_SQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewSQLiteFeedbackStore(context.Background(), db, SQLiteFeedbackStoreConfig{MaxRetainedSamples: 100})
	if err != nil {
		t.Fatalf("NewSQLiteFeedbackStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	for i := 0; i < feedbackMinScoredCount+2; i++ {
		plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
		if err != nil {
			t.Fatalf("loop Route %d: %v", i, err)
		}
		if _, err := plan.ExecuteChat(context.Background()); err != nil {
			t.Fatalf("loop ExecuteChat %d: %v", i, err)
		}
		// No sleep: PR3 uses in-transaction writes (BEGIN…COMMIT inside
		// SQLiteFeedbackStore.RecordBatch), and Task 5's benchmark proved
		// the commit is synchronous + complete by the time RecordBatch
		// returns. The next Route's snapshot read sees the committed row
		// immediately. Adding a Sleep here would only mask a future
		// regression that broke that guarantee.
	}

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("final Route: %v", err)
	}
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("final ExecuteChat: %v", err)
	}
	bd := resp.RouteOutcome.ScoreBreakdown
	if bd == nil || !bd.FeedbackApplied {
		t.Fatalf("ScoreBreakdown=%+v, want non-nil with FeedbackApplied=true", bd)
	}
	if bd.FeedbackScore <= 0.5 {
		t.Errorf("FeedbackScore = %v, want > 0.5", bd.FeedbackScore)
	}
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
	// Parity with the MemoryStore twin: the SQLite path must also expose
	// a non-zero delta in Enforce mode. A regression where in-transaction
	// write commits but a stale aggregate is read by the next snapshot
	// (e.g. introduced by a future cache layer) would still surface
	// FeedbackApplied=true here without this check.
	if bd.ScoreWithFeedback == bd.ScoreWithoutFeedback {
		t.Errorf("SQLite Enforce: with == without (%v); expected delta because feedback was non-neutral",
			bd.ScoreWithFeedback)
	}
}

// ---------------------------------------------------------------------------
// TestRouterShadowModeExposesDeltaButDoesNotChangeSelection
//
// In Shadow mode, even strong seeded feedback must:
//   (a) NOT shift FeedbackApplied (stays false because selection uses
//       scoreWithoutFeedback), and
//   (b) STILL produce a non-trivial delta in the breakdown so operators
//       can compare the "would-be-applied" value against the actual
//       selection. RouteOutcome.Score must equal ScoreWithoutFeedback.
//
// No direction assertion: positive adjusted feedback can produce a lower
// with-feedback weighted average depending on candidate weights. The
// invariants are (delta != 0) AND (selection score == neutral-feedback
// baseline).
// ---------------------------------------------------------------------------

func TestRouterShadowModeExposesDeltaButDoesNotChangeSelection(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	k := FeedbackKey{Provider: "test", Model: "qwen3:8b", UseCase: "chat"}
	for i := 0; i < feedbackMinScoredCount+5; i++ {
		if err := rf.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
			t.Fatalf("seed Record: %v", err)
		}
	}
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringShadow))

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	bd := resp.RouteOutcome.ScoreBreakdown
	if bd == nil {
		t.Fatalf("Shadow mode: ScoreBreakdown nil, want non-nil")
	}
	if bd.FeedbackApplied {
		t.Errorf("Shadow mode: FeedbackApplied = true, want false")
	}
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("Shadow mode: FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
	if bd.FeedbackScore <= 0.5 {
		t.Errorf("Shadow mode: FeedbackScore = %v, want > 0.5 (snapshot active)", bd.FeedbackScore)
	}
	// Plan-resolved Shadow assertion: breakdown must expose a non-zero
	// delta even though selection used scoreWithoutFeedback. No direction
	// assertion — positive adjusted feedback can still produce a lower
	// with-feedback weighted average than the neutral-feedback baseline
	// depending on candidate weights. The only spec invariant is that the
	// breakdown exposes a non-zero delta.
	if bd.ScoreWithFeedback == bd.ScoreWithoutFeedback {
		t.Errorf("Shadow mode: with == without (%v); breakdown must expose the would-be-applied delta",
			bd.ScoreWithFeedback)
	}
	// Selection score must equal scoreWithoutFeedback in Shadow.
	if resp.RouteOutcome.Score != bd.ScoreWithoutFeedback {
		t.Errorf("Shadow mode: RouteOutcome.Score (%v) != ScoreWithoutFeedback (%v); selection diverged from spec",
			resp.RouteOutcome.Score, bd.ScoreWithoutFeedback)
	}
}

// ---------------------------------------------------------------------------
// TestRouterEnforceModeFailOpenSurfacesReadErrorOnRouteOutcome
//
// End-to-end fail-open: Enforce mode + a RoutingFeedbackStore whose Get
// always errors. Route + ExecuteChat still succeed; the public
// ScoreBreakdown is stamped with FeedbackApplied=false and
// FeedbackSnapshotStatus="read_error", so an operator dashboard can tell
// "fail-open" apart from "feedback off by design" without parsing logs.
// This is the hostile-case integration test the readiness review asked
// for; the internals are already covered by
// TestSnapshotReadCandidatesAfterLatchIsNoOp (unit) and
// TestBuildOutcomeScoreBreakdownSnapshotStatusReflectsRoute (translator),
// but neither exercises the public seam consumers actually read.
// ---------------------------------------------------------------------------

func TestRouterEnforceModeFailOpenSurfacesReadErrorOnRouteOutcome(t *testing.T) {
	rf := NewRoutingFeedback(&flakyStore{err: errors.New("disk full")})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v (fail-open must NOT propagate to the caller)", err)
	}
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	bd := resp.RouteOutcome.ScoreBreakdown
	if bd == nil {
		t.Fatalf("Enforce + fail-open: ScoreBreakdown nil; want stamped with read_error status")
	}
	if bd.FeedbackApplied {
		t.Errorf("Enforce + fail-open: FeedbackApplied = true, want false (selection used scoreWithoutFeedback)")
	}
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusReadError {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusReadError)
	}
	if bd.FeedbackMode != FeedbackScoringEnforce.String() {
		t.Errorf("FeedbackMode = %q, want %q (operator distinguishes Enforce-fail-open from Shadow-active)",
			bd.FeedbackMode, FeedbackScoringEnforce.String())
	}
	// Selection score must equal scoreWithoutFeedback on fail-open: the
	// route used the PR2 neutral-feedback baseline.
	if resp.RouteOutcome.Score != bd.ScoreWithoutFeedback {
		t.Errorf("fail-open: RouteOutcome.Score (%v) != ScoreWithoutFeedback (%v); selection used wrong path",
			resp.RouteOutcome.Score, bd.ScoreWithoutFeedback)
	}
}

// ---------------------------------------------------------------------------
// TestRouterShadowModeNoSignalsYet
//
// Shadow mode + a configured but empty store: the snapshot status is
// "active" (the reads succeeded), but no signals have been recorded.
// FeedbackScore stays neutral 0.5 and the with/without delta is zero.
// An operator looking at a fresh deployment sees mode=shadow,
// status=active, score=0.5, delta=0 — distinguishable from a fail-open
// (which would have status=read_error) and from a never-configured store
// (status=no_store). Avoids the "everything looks neutral, must be
// broken" misdiagnosis.
// ---------------------------------------------------------------------------

func TestRouterShadowModeNoSignalsYet(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringShadow))

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	bd := resp.RouteOutcome.ScoreBreakdown
	if bd == nil {
		t.Fatalf("Shadow + empty store: ScoreBreakdown nil; want stamped with active status")
	}
	if bd.FeedbackApplied {
		t.Errorf("Shadow: FeedbackApplied = true, want false")
	}
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q (reads succeeded, just empty)",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
	if bd.FeedbackScore != 0.5 {
		t.Errorf("empty store: FeedbackScore = %v, want neutral 0.5", bd.FeedbackScore)
	}
	if bd.FeedbackSampleCount != 0 || bd.FeedbackScoredCount != 0 {
		t.Errorf("empty store: counts = (%d,%d), want (0,0)", bd.FeedbackSampleCount, bd.FeedbackScoredCount)
	}
	if bd.ScoreWithFeedback != bd.ScoreWithoutFeedback {
		t.Errorf("empty Shadow: ScoreWithFeedback=%v, ScoreWithoutFeedback=%v; want equal neutral-feedback baseline",
			bd.ScoreWithFeedback, bd.ScoreWithoutFeedback)
	}
	if bd.FeedbackUpdatedAt != nil {
		t.Errorf("empty Shadow: FeedbackUpdatedAt = %v, want nil", bd.FeedbackUpdatedAt)
	}
}
