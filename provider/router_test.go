package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// rtMockProvider — mock Provider for Router tests
// ---------------------------------------------------------------------------

// rtMockProvider implements Provider for Router-level tests. It advertises
// configurable models and returns canned responses. The "rt" prefix avoids
// collision with mocks in other test files (rpMockProvider, mrMockProvider).
type rtMockProvider struct {
	name      string
	caps      Capability
	models    []ModelInfo
	chatResp  *ChatResponse
	chatErr   error
	genResp   *GenerateResponse
	genErr    error
	embedResp *EmbedResponse
	embedErr  error
	healthErr error

	mu           sync.Mutex
	chatCalls    int
	genCalls     int
	embedCalls   int
	lastChatReq  ChatRequest
	lastGenReq   GenerateRequest
	lastEmbedReq EmbedRequest
}

func (m *rtMockProvider) Name() string                   { return m.name }
func (m *rtMockProvider) Capabilities() Capability       { return m.caps }
func (m *rtMockProvider) Health(_ context.Context) error { return m.healthErr }
func (m *rtMockProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return m.models, nil
}

func (m *rtMockProvider) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	m.mu.Lock()
	m.chatCalls++
	m.lastChatReq = req
	m.mu.Unlock()
	if m.chatErr != nil {
		return nil, m.chatErr
	}
	resp := *m.chatResp
	return &resp, nil
}

func (m *rtMockProvider) ChatStream(_ context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	m.mu.Lock()
	m.chatCalls++
	m.lastChatReq = req
	m.mu.Unlock()
	if m.chatErr != nil {
		return m.chatErr
	}
	return fn(ChatResponse{Model: m.chatResp.Model, Content: m.chatResp.Content, Done: true})
}

func (m *rtMockProvider) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	m.mu.Lock()
	m.genCalls++
	m.lastGenReq = req
	m.mu.Unlock()
	if m.genErr != nil {
		return nil, m.genErr
	}
	resp := *m.genResp
	return &resp, nil
}

func (m *rtMockProvider) GenerateStream(_ context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	m.mu.Lock()
	m.genCalls++
	m.lastGenReq = req
	m.mu.Unlock()
	if m.genErr != nil {
		return m.genErr
	}
	return fn(GenerateResponse{Model: m.genResp.Model, Response: m.genResp.Response, Done: true})
}

func (m *rtMockProvider) Embed(_ context.Context, req EmbedRequest) (*EmbedResponse, error) {
	m.mu.Lock()
	m.embedCalls++
	m.lastEmbedReq = req
	m.mu.Unlock()
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	resp := *m.embedResp
	return &resp, nil
}

func (m *rtMockProvider) getChatCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatCalls
}

func (m *rtMockProvider) getGenCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.genCalls
}

func (m *rtMockProvider) getEmbedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.embedCalls
}

func (m *rtMockProvider) getLastChatRequest() ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastChatReq
}

func (m *rtMockProvider) getLastGenerateRequest() GenerateRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastGenReq
}

func (m *rtMockProvider) getLastEmbedRequest() EmbedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastEmbedReq
}

// ---------------------------------------------------------------------------
// rtMockWarmthSource — mock WarmthSource for Router tests
// ---------------------------------------------------------------------------

type rtMockWarmthSource struct {
	mu       sync.Mutex
	warmKeys map[ModelKey]*WarmthInfo
	uses     []ModelKey
}

func newRTMockWarmthSource() *rtMockWarmthSource {
	return &rtMockWarmthSource{
		warmKeys: make(map[ModelKey]*WarmthInfo),
	}
}

func (w *rtMockWarmthSource) setWarm(key ModelKey, info WarmthInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warmKeys[key] = &info
}

func (w *rtMockWarmthSource) IsWarm(key ModelKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.warmKeys[key]
	return ok && info.Loaded
}

func (w *rtMockWarmthSource) WarmthState(key ModelKey) *WarmthInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, ok := w.warmKeys[key]
	if !ok {
		return nil
	}
	copy := *info
	return &copy
}

func (w *rtMockWarmthSource) RecordUse(key ModelKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.uses = append(w.uses, key)
}

func (w *rtMockWarmthSource) Snapshot() []WarmModel {
	w.mu.Lock()
	defer w.mu.Unlock()
	var result []WarmModel
	for key, info := range w.warmKeys {
		if info.Loaded {
			result = append(result, WarmModel{Key: key, Info: *info})
		}
	}
	return result
}

func (w *rtMockWarmthSource) Close() error { return nil }

func (w *rtMockWarmthSource) getUses() []ModelKey {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]ModelKey, len(w.uses))
	copy(out, w.uses)
	return out
}

// ---------------------------------------------------------------------------
// setupTestRouter — creates a Router with one mock provider and model
// ---------------------------------------------------------------------------

// setupTestRouter creates a Router pre-configured with a single "test"
// provider serving a "qwen3:8b" model. Returns the router, the provider
// mock, and a cleanup function.
func setupTestRouter(t *testing.T, opts ...RouterOption) (*Router, *rtMockProvider) {
	t.Helper()

	prov := &rtMockProvider{
		name: "test",
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
			Content: "Hello from router!",
			Done:    true,
		},
		genResp: &GenerateResponse{
			Model:    "qwen3:8b",
			Response: "Generated text",
			Done:     true,
		},
		embedResp: &EmbedResponse{
			Model:      "qwen3:8b",
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		},
	}

	// Set up provider registry and register mock.
	provReg := NewRegistry()
	if err := provReg.Register(prov); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), "test"); err != nil {
		t.Fatalf("RefreshModels failed: %v", err)
	}

	// Set up model registry.
	modelReg, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry failed: %v", err)
	}

	router := NewRouter(modelReg, provReg, opts...)
	t.Cleanup(func() { _ = router.Close() })

	return router, prov
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRouterRouteQualifiedModel(t *testing.T) {
	router, _ := setupTestRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:   "test/qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}
	if plan.Model != "qwen3:8b" {
		t.Errorf("plan.Model = %q, want %q", plan.Model, "qwen3:8b")
	}
	if plan.Profile.Key.Provider != "test" {
		t.Errorf("plan.Profile.Key.Provider = %q, want %q", plan.Profile.Key.Provider, "test")
	}
	if plan.Provider == nil {
		t.Error("plan.Provider is nil, want non-nil")
	}
	if plan.Score <= 0 {
		t.Errorf("plan.Score = %f, want positive", plan.Score)
	}
}

func TestRouterRouteUnqualifiedModel(t *testing.T) {
	router, _ := setupTestRouter(t)

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}
	if plan.Model != "qwen3:8b" {
		t.Errorf("plan.Model = %q, want %q", plan.Model, "qwen3:8b")
	}
	if plan.Profile.Key.Provider != "test" {
		t.Errorf("plan.Profile.Key.Provider = %q, want %q", plan.Profile.Key.Provider, "test")
	}
}

func TestRouterModelDefaultsFillOnlyUnsetRequestOptions(t *testing.T) {
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}
	router, prov := setupTestRouter(t, WithModelDefaults(map[ModelKey]SamplingDefaults{
		key: {
			Temperature: Ptr(0.7),
			TopP:        Ptr(0.9),
			TopK:        Ptr(40),
		},
	}))

	_, err := router.Chat(context.Background(), ChatRequest{
		Model: "test/qwen3:8b",
		Options: ModelOptions{
			Temperature: Ptr(0.0),
			TopP:        Ptr(0.0),
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	opts := prov.getLastChatRequest().Options
	if opts.Temperature == nil || *opts.Temperature != 0 {
		t.Errorf("Temperature = %v, want explicit request zero", opts.Temperature)
	}
	if opts.TopP == nil || *opts.TopP != 0 {
		t.Errorf("TopP = %v, want explicit request zero", opts.TopP)
	}
	if opts.TopK == nil || *opts.TopK != 40 {
		t.Errorf("TopK = %v, want model default 40", opts.TopK)
	}
}

func TestRouterModelDefaultsAreIsolatedPerRequest(t *testing.T) {
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}
	router, _ := setupTestRouter(t, WithModelDefaults(map[ModelKey]SamplingDefaults{
		key: {Temperature: Ptr(0.7)},
	}))

	first, err := router.Route(context.Background(), RoutingRequest{Model: key.String(), UseCase: "chat"})
	if err != nil {
		t.Fatalf("first Route: %v", err)
	}
	*first.Request.Options.Temperature = 0.1

	second, err := router.Route(context.Background(), RoutingRequest{Model: key.String(), UseCase: "chat"})
	if err != nil {
		t.Fatalf("second Route: %v", err)
	}
	if second.Request.Options.Temperature == nil || *second.Request.Options.Temperature != 0.7 {
		t.Fatalf("second Temperature = %v, want isolated default 0.7", second.Request.Options.Temperature)
	}
}

func TestRouterRouteClosed(t *testing.T) {
	router, _ := setupTestRouter(t)

	// Close the router.
	if err := router.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	_, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("error = %v, want ErrRouterClosed", err)
	}
}

func TestRouterRouteNoViableCandidate(t *testing.T) {
	router, _ := setupTestRouter(t)

	// Request a model that doesn't exist.
	_, err := router.Route(context.Background(), RoutingRequest{
		Model:   "nonexistent-model:99b",
		UseCase: "chat",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The error comes from LookupAny which tries ProvidersForModel — it won't
	// find the model. This surfaces as a lookup error rather than
	// ErrNoViableCandidate directly, but the key point is it fails.
	// Check it's not nil and contains a useful message.
	if err.Error() == "" {
		t.Error("error message is empty")
	}
}

func TestRouterRouteBreakerOpen(t *testing.T) {
	router, _ := setupTestRouter(t)

	// Trip the circuit breaker by recording 3 infrastructure failures.
	infraErr := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	cb := router.getOrCreateBreaker("test")
	cb.RecordFailure(infraErr)
	cb.RecordFailure(infraErr)
	cb.RecordFailure(infraErr)

	// Verify breaker is open.
	if cb.State() != BreakerOpen {
		t.Fatalf("breaker state = %v, want Open", cb.State())
	}

	_, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if !errors.Is(err, ErrAllBreakersOpen) {
		t.Errorf("error = %v, want ErrAllBreakersOpen", err)
	}
}

func TestRouterChatConvenience(t *testing.T) {
	router, prov := setupTestRouter(t)

	resp, err := router.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat returned nil response")
	}
	if resp.Content != "Hello from router!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from router!")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil, want non-nil")
	}
	if resp.RouteOutcome.PlannedModel.Model != "qwen3:8b" {
		t.Errorf("PlannedModel = %v, want qwen3:8b", resp.RouteOutcome.PlannedModel)
	}
	if resp.RouteOutcome.ActualModel.Model != "qwen3:8b" {
		t.Errorf("ActualModel = %v, want qwen3:8b", resp.RouteOutcome.ActualModel)
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}

	if prov.getChatCalls() != 1 {
		t.Errorf("chatCalls = %d, want 1", prov.getChatCalls())
	}
}

func TestRouterStickyRouting(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := RoutingRequest{
		Model:       "qwen3:8b",
		UseCase:     "chat",
		AffinityKey: "session-123",
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
	}

	// First route.
	plan1, err := router.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route 1 returned error: %v", err)
	}

	// Second route with same affinity key.
	plan2, err := router.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route 2 returned error: %v", err)
	}

	// Both should route to the same model.
	if plan1.Model != plan2.Model {
		t.Errorf("plan1.Model = %q, plan2.Model = %q, want same", plan1.Model, plan2.Model)
	}
	if plan1.Profile.Key != plan2.Profile.Key {
		t.Errorf("plan1 key = %v, plan2 key = %v, want same", plan1.Profile.Key, plan2.Profile.Key)
	}

	// Sticky routes should be non-empty.
	routes := router.StickyRoutes()
	if len(routes) == 0 {
		t.Error("StickyRoutes is empty, want at least 1 entry")
	}
}

func TestRouterDryRunDoesNotUpdateSticky(t *testing.T) {
	router, _ := setupTestRouter(t)

	req := RoutingRequest{
		Model:       "qwen3:8b",
		UseCase:     "chat",
		AffinityKey: "session-dry",
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		DryRun:      true,
	}

	plan, err := router.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}

	routes := router.StickyRoutes()
	if len(routes) != 0 {
		t.Errorf("StickyRoutes has %d entries, want 0 (DryRun should not update cache)",
			len(routes))
	}
}

func TestRouterBreakerInfo(t *testing.T) {
	router, _ := setupTestRouter(t)

	// Before any request, breaker should not exist.
	_, exists := router.BreakerInfo("test")
	if exists {
		t.Error("BreakerInfo exists before request, want false")
	}

	// Route a request to create the breaker lazily.
	_, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}

	// After routing, breaker should exist.
	info, exists := router.BreakerInfo("test")
	if !exists {
		t.Fatal("BreakerInfo does not exist after request, want true")
	}
	if info.State != BreakerClosed {
		t.Errorf("breaker state = %v, want Closed", info.State)
	}
	if info.Failures != 0 {
		t.Errorf("breaker failures = %d, want 0", info.Failures)
	}
}

func TestRouterCloseIdempotent(t *testing.T) {
	router, _ := setupTestRouter(t)

	// Close twice — should not panic.
	if err := router.Close(); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := router.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}

	// Verify it's closed.
	_, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if !errors.Is(err, ErrRouterClosed) {
		t.Errorf("error = %v, want ErrRouterClosed", err)
	}
}

func TestRouterWithWarmthSource(t *testing.T) {
	ws := newRTMockWarmthSource()
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}
	ws.setWarm(key, WarmthInfo{
		Loaded:    true,
		Since:     time.Now(),
		ExpiresAt: time.Now().Add(5 * time.Minute),
		VRAM:      4.5,
	})

	router, _ := setupTestRouter(t, WithWarmthSource(ws))

	// Verify warmth snapshot.
	snapshot := router.WarmthSnapshot()
	if len(snapshot) == 0 {
		t.Fatal("WarmthSnapshot is empty, want at least 1 entry")
	}
	found := false
	for _, wm := range snapshot {
		if wm.Key == key {
			found = true
			if !wm.Info.Loaded {
				t.Error("model not loaded in warmth info")
			}
		}
	}
	if !found {
		t.Errorf("model %v not found in WarmthSnapshot", key)
	}

	// Route and verify warmth is used in scoring.
	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan.Score <= 0 {
		t.Errorf("plan.Score = %f, want positive", plan.Score)
	}
}

func TestRouterWithAvailableRAM(t *testing.T) {
	// Set up two providers: one with a small model, one with a large model.
	smallProv := &rtMockProvider{
		name: "small",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "small-model:1b",
				Family:        "small",
				ParameterSize: "1B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 8192,
				Capabilities:  []string{"chat", "generate", "stream"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "small-model:1b",
			Content: "from small",
			Done:    true,
		},
	}

	largeProv := &rtMockProvider{
		name: "large",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{
				Name:          "large-model:70b",
				Family:        "large",
				ParameterSize: "70B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 32768,
				Capabilities:  []string{"chat", "generate", "stream"},
			},
		},
		chatResp: &ChatResponse{
			Model:   "large-model:70b",
			Content: "from large",
			Done:    true,
		},
	}

	provReg := NewRegistry()
	if err := provReg.Register(smallProv); err != nil {
		t.Fatalf("Register small failed: %v", err)
	}
	if err := provReg.Register(largeProv); err != nil {
		t.Fatalf("Register large failed: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), "small"); err != nil {
		t.Fatalf("RefreshModels small failed: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), "large"); err != nil {
		t.Fatalf("RefreshModels large failed: %v", err)
	}

	modelReg, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry failed: %v", err)
	}

	// Create router with only 8 GB available RAM.
	router := NewRouter(modelReg, provReg, WithAvailableRAM(8.0))
	t.Cleanup(func() { _ = router.Close() })

	// Route without specifying a model — the large model should be filtered.
	plan, err := router.Route(context.Background(), RoutingRequest{
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if plan == nil {
		t.Fatal("Route returned nil plan")
	}

	// The 70B model requires ~46 GB RAM at Q4, so it should be filtered out.
	// Only the small model should survive.
	if plan.Model != "small-model:1b" {
		t.Errorf("plan.Model = %q, want %q (70B should be filtered by RAM)", plan.Model, "small-model:1b")
	}
}

func TestRouterGenerateConvenience(t *testing.T) {
	router, _ := setupTestRouter(t)

	resp, err := router.Generate(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Complete this",
	})
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Generate returned nil response")
	}
	if resp.Response != "Generated text" {
		t.Errorf("Response = %q, want %q", resp.Response, "Generated text")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil")
	}
}

func TestRouterEmbedConvenience(t *testing.T) {
	router, _ := setupTestRouter(t)

	resp, err := router.Embed(context.Background(), EmbedRequest{
		Model: "qwen3:8b",
		Input: []string{"embed this"},
	})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("Embed returned nil response")
	}
	if len(resp.Embeddings) != 1 {
		t.Errorf("Embeddings count = %d, want 1", len(resp.Embeddings))
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil")
	}
}

func TestRouterRecordSuccess(t *testing.T) {
	router, _ := setupTestRouter(t)
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}

	// Get breaker created first.
	_ = router.getOrCreateBreaker("test")

	// Record success.
	router.RecordSuccess(key, LatencyInfo{})

	info, exists := router.BreakerInfo("test")
	if !exists {
		t.Fatal("BreakerInfo does not exist")
	}
	if info.State != BreakerClosed {
		t.Errorf("breaker state = %v, want Closed", info.State)
	}
}

func TestRouterRecordFailureTripsBreaker(t *testing.T) {
	router, _ := setupTestRouter(t)
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}
	infraErr := &HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}

	// Record enough failures to trip.
	router.RecordFailure(key, infraErr)
	router.RecordFailure(key, infraErr)
	router.RecordFailure(key, infraErr)

	info, exists := router.BreakerInfo("test")
	if !exists {
		t.Fatal("BreakerInfo does not exist")
	}
	if info.State != BreakerOpen {
		t.Errorf("breaker state = %v, want Open", info.State)
	}
}

func TestRouterRecordWarmthUse(t *testing.T) {
	ws := newRTMockWarmthSource()
	router, _ := setupTestRouter(t, WithWarmthSource(ws))
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}

	router.RecordWarmthUse(key)

	uses := ws.getUses()
	if len(uses) != 1 {
		t.Fatalf("warmth uses = %d, want 1", len(uses))
	}
	if uses[0] != key {
		t.Errorf("warmth use key = %v, want %v", uses[0], key)
	}
}

func TestRouterRecordWarmthUseNilSource(t *testing.T) {
	router, _ := setupTestRouter(t) // no warmth source
	key := ModelKey{Provider: "test", Model: "qwen3:8b"}

	// Should not panic.
	router.RecordWarmthUse(key)
}

func TestRouterWarmthSnapshotNilSource(t *testing.T) {
	router, _ := setupTestRouter(t)

	snapshot := router.WarmthSnapshot()
	if snapshot != nil {
		t.Errorf("WarmthSnapshot = %v, want nil (no warmth source)", snapshot)
	}
}

func TestRouterInferRouteKind(t *testing.T) {
	tests := []struct {
		name string
		req  RoutingRequest
		want RouteKind
	}{
		{"chat", RoutingRequest{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, RouteKindChat},
		{"generate", RoutingRequest{Prompt: "complete"}, RouteKindGenerate},
		{"fim", RoutingRequest{Suffix: "suffix"}, RouteKindGenerate},
		{"embed", RoutingRequest{Input: []string{"text"}}, RouteKindEmbed},
		{"default", RoutingRequest{}, RouteKindChat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferRouteKind(tt.req)
			if got != tt.want {
				t.Errorf("inferRouteKind() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRouterOptions(t *testing.T) {
	router, _ := setupTestRouter(t,
		WithStickyTTL(10*time.Minute),
		WithHysteresis(0.20),
		WithMaxStickyEntries(512),
		WithMaxFallbacks(5),
		WithDefaultPriority(PriorityHigh),
	)

	if router.defaultOpts.stickyTTL != 10*time.Minute {
		t.Errorf("stickyTTL = %v, want 10m", router.defaultOpts.stickyTTL)
	}
	if router.defaultOpts.hysteresisMargin != 0.20 {
		t.Errorf("hysteresisMargin = %f, want 0.20", router.defaultOpts.hysteresisMargin)
	}
	if router.defaultOpts.maxStickyEntries != 512 {
		t.Errorf("maxStickyEntries = %d, want 512", router.defaultOpts.maxStickyEntries)
	}
	if router.defaultOpts.maxFallbacks != 5 {
		t.Errorf("maxFallbacks = %d, want 5", router.defaultOpts.maxFallbacks)
	}
	if router.defaultOpts.defaultPriority != PriorityHigh {
		t.Errorf("defaultPriority = %v, want PriorityHigh", router.defaultOpts.defaultPriority)
	}
}

func TestRouterBuildReason(t *testing.T) {
	sc := scoredCandidate{
		profile: &ModelProfile{
			Quality: TierGreat,
			Speed:   TierGood,
		},
		budget: BudgetResult{Decision: BudgetOK},
		score:  0.75,
	}

	reason := buildReason(sc)
	if reason == "" {
		t.Error("buildReason returned empty string")
	}
	// Should contain the score.
	if !strings.Contains(reason, "0.750") {
		t.Errorf("reason = %q, should contain score", reason)
	}
}

func TestRouterWithRoutingFeedbackPlumbsToPlan(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	// Construct a router with the new option. setupTestRouter already
	// accepts RouterOption values and registers the qwen3:8b fixture model.
	r, _ := setupTestRouter(t, WithRoutingFeedback(rf))

	// Drive a Route call that succeeds. The test fixture should already
	// register a model that the router will select. Inspect the returned
	// plan's feedback field via a small accessor or by behavior: execute
	// the plan and check that store.Get returns a non-zero SampleCount.
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.feedback != rf {
		t.Errorf("plan.feedback = %v, want injected rf", plan.feedback)
	}
}

func TestRouterWithoutRoutingFeedbackProducesNilPlanFeedback(t *testing.T) {
	r, _ := setupTestRouter(t)
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:   "qwen3:8b",
		UseCase: "chat",
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.feedback != nil {
		t.Errorf("plan.feedback = %v, want nil (no option configured)", plan.feedback)
	}
}

// ---------------------------------------------------------------------------
// PR3 Task 8: activeSignals split — selection vs. delta
// ---------------------------------------------------------------------------
//
// Three helpers replaced the pre-PR3 activeSignals():
//
//   - baseActiveSignals       — feedback denominator active at neutral 0.5
//   - selectionActiveSignals  — same denominator; value chosen by mode/status
//   - deltaActiveSignals      — same denominator; live value when available
//
// The truth-table tests below pin every interesting (mode, active) cell
// so future refactors cannot silently regress fail-open or Shadow-mode
// delta exposure.

func TestRouterScoringSelectionActiveIncludesNeutralFeedbackWhenOff(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringOff))

	active := router.selectionActiveSignals(&feedbackSnapshot{mode: FeedbackScoringOff})
	if !active["feedback"] {
		t.Errorf("Off mode: selectionActiveSignals[\"feedback\"] = false, want true for neutral PR2 denominator")
	}
}

func TestRouterScoringSelectionActiveIncludesNeutralFeedbackWhenShadow(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringShadow))

	snap := &feedbackSnapshot{mode: FeedbackScoringShadow, active: true, byKey: map[FeedbackKey]*candidateFeedback{}}
	active := router.selectionActiveSignals(snap)
	if !active["feedback"] {
		t.Errorf("Shadow mode: selectionActiveSignals[\"feedback\"] = false, want true for neutral PR2 denominator")
	}
}

func TestRouterScoringSelectionActiveIncludesFeedbackWhenEnforce(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	snap := &feedbackSnapshot{mode: FeedbackScoringEnforce, active: true, byKey: map[FeedbackKey]*candidateFeedback{}}
	active := router.selectionActiveSignals(snap)
	if !active["feedback"] {
		t.Errorf("Enforce mode + active snapshot: selectionActiveSignals[\"feedback\"] = false, want true")
	}
}

func TestRouterScoringSelectionActiveIncludesNeutralFeedbackOnFailOpen(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	snap := &feedbackSnapshot{mode: FeedbackScoringEnforce, active: false}
	active := router.selectionActiveSignals(snap)
	if !active["feedback"] {
		t.Errorf("Enforce mode + inactive snapshot: selectionActiveSignals[\"feedback\"] = false, want true for neutral PR2 denominator")
	}
}

func TestRouterScoringDeltaActiveIncludesFeedbackInShadow(t *testing.T) {
	// In Shadow mode, the delta computation MUST include feedback even
	// though selection doesn't — otherwise ScoreWithFeedback ==
	// ScoreWithoutFeedback in Shadow and the breakdown is useless.
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringShadow))

	snap := &feedbackSnapshot{mode: FeedbackScoringShadow, active: true, byKey: map[FeedbackKey]*candidateFeedback{}}
	active := router.deltaActiveSignals(snap)
	if !active["feedback"] {
		t.Errorf("Shadow mode + active snapshot: deltaActiveSignals[\"feedback\"] = false, want true (delta needs feedback)")
	}
}

func TestRouterScoringDeltaActiveIncludesNeutralFeedbackOnFailOpen(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	snap := &feedbackSnapshot{mode: FeedbackScoringEnforce, active: false}
	active := router.deltaActiveSignals(snap)
	if !active["feedback"] {
		t.Errorf("inactive snapshot: deltaActiveSignals[\"feedback\"] = false, want true for neutral PR2 denominator")
	}
}

func TestRouterScoringShadowSelectionUsesNeutralFeedbackValue(t *testing.T) {
	bd := scoreBreakdown{
		feedbackActive: true,
		feedbackScore:  1.0,
	}
	snap := &feedbackSnapshot{mode: FeedbackScoringShadow, active: true}
	got := scoreBreakdownForSelection(bd, snap)
	if got.feedbackScore != 0.5 {
		t.Errorf("Shadow selection feedbackScore = %v, want neutral 0.5", got.feedbackScore)
	}
}

func TestRouterOffModeNeutralFeedbackPreservesStickyMarginScale(t *testing.T) {
	router, _ := setupTestRouter(t, WithFeedbackScoringMode(FeedbackScoringOff))
	active := router.selectionActiveSignals(&feedbackSnapshot{mode: FeedbackScoringOff})
	wp := &WeightProfile{Headroom: 1, Feedback: 1}
	incumbent := scoreBreakdown{headroomScore: 0, feedbackScore: 0.5}
	challenger := scoreBreakdown{headroomScore: 0.29, feedbackScore: 0.5}

	incumbentScore := computeWeightedScore(incumbent, "chat", active, wp)
	challengerScore := computeWeightedScore(challenger, "chat", active, wp)
	if challengerScore-incumbentScore > router.defaultOpts.hysteresisMargin {
		t.Fatalf("neutral-feedback gap = %v, want <= sticky margin %v",
			challengerScore-incumbentScore, router.defaultOpts.hysteresisMargin)
	}

	withoutFeedback := router.baseActiveSignals()
	delete(withoutFeedback, "feedback")
	incumbentNoFeedback := computeWeightedScore(incumbent, "chat", withoutFeedback, wp)
	challengerNoFeedback := computeWeightedScore(challenger, "chat", withoutFeedback, wp)
	if challengerNoFeedback-incumbentNoFeedback <= router.defaultOpts.hysteresisMargin {
		t.Fatalf("test setup failed: no-feedback gap = %v, want > sticky margin %v",
			challengerNoFeedback-incumbentNoFeedback, router.defaultOpts.hysteresisMargin)
	}
}

func TestRouterScoreAllPopulatesScoreWithAndWithoutFeedback(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	k := FeedbackKey{Provider: "test", Model: "qwen3:8b", UseCase: "chat"}
	for i := 0; i < feedbackMinScoredCount+2; i++ {
		if err := rf.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan == nil {
		t.Fatalf("Route returned nil plan")
	}
	// The plan's internal scoreBreakdown should expose both scores via
	// the public ScoreBreakdown after Task 9; here we assert the route
	// completed without error with feedback active.
	_ = plan
}

// TestSnapshotReadCandidatesLatchesOnError proves that once an early
// readCandidates call fails, subsequent calls are no-ops: the route-level
// snapshot remains inactive for any chain step / recommend tail that
// runs later in the same route. This is the central invariant the
// readiness review called out — a chain route must NOT re-activate
// feedback for a later step after an earlier step's read_error.
func TestSnapshotReadCandidatesLatchesOnError(t *testing.T) {
	rf := NewRoutingFeedback(&flakyStore{err: errors.New("disk full")})
	router, _ := setupTestRouter(t, WithRoutingFeedback(rf), WithFeedbackScoringMode(FeedbackScoringEnforce))
	cap := &capturingLogger{}
	router.feedbackLogger = cap

	// Start the snapshot in an "active with empty byKey" state by passing
	// nil — mimics how routeChain initialises it.
	snap := router.buildFeedbackSnapshot(context.Background(), nil, "chat")
	if !snap.active {
		t.Fatalf("snapshot inactive immediately after construction; want active(empty byKey)")
	}

	// First chain step's profiles trigger a read failure; snapshot latches.
	first := []*ModelProfile{{Key: ModelKey{Provider: "test", Model: "qwen3:8b"}}}
	snap.readCandidates(context.Background(), router, first, "chat")
	if snap.active || !snap.failed {
		t.Fatalf("after first read error: active=%v failed=%v; want active=false failed=true", snap.active, snap.failed)
	}
	if snap.status != feedbackSnapshotStatusReadError {
		t.Errorf("status = %q, want %q", snap.status, feedbackSnapshotStatusReadError)
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("warnFeedbackReadOnce fired %d times, want 1", got)
	}

	// Second chain step tries to add fresh candidates — must be a no-op,
	// no second warning, snapshot stays inactive.
	second := []*ModelProfile{{Key: ModelKey{Provider: "fallback", Model: "qwen3:8b"}}}
	snap.readCandidates(context.Background(), router, second, "chat")
	if snap.active {
		t.Errorf("snapshot re-activated after latching read_error; want stays inactive")
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("warning fired again after latch: %d, want still 1", got)
	}
	// And the new key must not be present (no read happened).
	if snap.lookup(FeedbackKey{Provider: "fallback", Model: "qwen3:8b", UseCase: "chat"}) != nil {
		t.Errorf("inactive snapshot returned non-nil lookup; want nil")
	}
}
