package provider

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// rpMockProvider
// ---------------------------------------------------------------------------

// rpMockProvider implements the Provider interface for testing RoutePlan
// execution. It supports configurable responses, errors, and stream chunks,
// and tracks call counts for verification.
type rpMockProvider struct {
	name             string
	caps             Capability
	chatResp         *ChatResponse
	chatErr          error
	chatStreamChunks []ChatResponse
	chatStreamErr    error
	genResp          *GenerateResponse
	genErr           error
	genStreamChunks  []GenerateResponse
	genStreamErr     error
	embedResp        *EmbedResponse
	embedErr         error
	models           []ModelInfo
	healthErr        error
	chatCalls        int
	genCalls         int
	embedCalls       int
	mu               sync.Mutex
}

func (m *rpMockProvider) Name() string         { return m.name }
func (m *rpMockProvider) Capabilities() Capability { return m.caps }
func (m *rpMockProvider) Health(_ context.Context) error { return m.healthErr }
func (m *rpMockProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return m.models, nil
}

func (m *rpMockProvider) Chat(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
	m.mu.Lock()
	m.chatCalls++
	m.mu.Unlock()
	if m.chatErr != nil {
		return nil, m.chatErr
	}
	// Return a copy so tests can inspect without races.
	resp := *m.chatResp
	return &resp, nil
}

func (m *rpMockProvider) ChatStream(_ context.Context, _ ChatRequest, fn func(ChatResponse) error) error {
	m.mu.Lock()
	m.chatCalls++
	m.mu.Unlock()
	for _, chunk := range m.chatStreamChunks {
		if err := fn(chunk); err != nil {
			return err
		}
	}
	return m.chatStreamErr
}

func (m *rpMockProvider) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	m.mu.Lock()
	m.genCalls++
	m.mu.Unlock()
	if m.genErr != nil {
		return nil, m.genErr
	}
	resp := *m.genResp
	return &resp, nil
}

func (m *rpMockProvider) GenerateStream(_ context.Context, _ GenerateRequest, fn func(GenerateResponse) error) error {
	m.mu.Lock()
	m.genCalls++
	m.mu.Unlock()
	for _, chunk := range m.genStreamChunks {
		if err := fn(chunk); err != nil {
			return err
		}
	}
	return m.genStreamErr
}

func (m *rpMockProvider) Embed(_ context.Context, _ EmbedRequest) (*EmbedResponse, error) {
	m.mu.Lock()
	m.embedCalls++
	m.mu.Unlock()
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	resp := *m.embedResp
	return &resp, nil
}

// getChatCalls returns the chat call count in a thread-safe manner.
func (m *rpMockProvider) getChatCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatCalls
}

// getGenCalls returns the generate call count in a thread-safe manner.
func (m *rpMockProvider) getGenCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.genCalls
}

// getEmbedCalls returns the embed call count in a thread-safe manner.
func (m *rpMockProvider) getEmbedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.embedCalls
}

// ---------------------------------------------------------------------------
// rpMockRecorder
// ---------------------------------------------------------------------------

// rpMockRecorder implements RouteRecorder and captures signals for assertions.
type rpMockRecorder struct {
	mu         sync.Mutex
	successes  []ModelKey
	failures   []ModelKey
	warmthUses []ModelKey
}

func (r *rpMockRecorder) RecordSuccess(key ModelKey, _ LatencyInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.successes = append(r.successes, key)
}

func (r *rpMockRecorder) RecordFailure(key ModelKey, _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, key)
}

func (r *rpMockRecorder) RecordWarmthUse(key ModelKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warmthUses = append(r.warmthUses, key)
}

func (r *rpMockRecorder) getSuccesses() []ModelKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelKey, len(r.successes))
	copy(out, r.successes)
	return out
}

func (r *rpMockRecorder) getFailures() []ModelKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelKey, len(r.failures))
	copy(out, r.failures)
	return out
}

func (r *rpMockRecorder) getWarmthUses() []ModelKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelKey, len(r.warmthUses))
	copy(out, r.warmthUses)
	return out
}

// ---------------------------------------------------------------------------
// Helper: build a basic RoutePlan for tests
// ---------------------------------------------------------------------------

func newTestPlan(prov *rpMockProvider, rec *rpMockRecorder) *RoutePlan {
	key := ModelKey{Provider: prov.name, Model: "test-model"}
	return &RoutePlan{
		Kind:     RouteKindChat,
		Provider: prov,
		Model:    "test-model",
		Profile: &ModelProfile{
			Key:           key,
			Name:          "test-model",
			Family:        "test",
			Provider:      prov.name,
			ContextWindow: 32768,
		},
		Request: RoutingRequest{
			Model:    "test-model",
			UseCase:  "chat",
			Messages: []ChatMessage{{Role: "user", Content: "hello"}},
		},
		Score:  0.85,
		Budget: BudgetResult{Decision: BudgetOK},
		Reason: "best candidate",
		recorder: rec,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRoutePlanExecuteChat(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapChat,
		chatResp: &ChatResponse{
			Model:   "test-model",
			Content: "Hello, world!",
			Done:    true,
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)

	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat returned unexpected error: %v", err)
	}
	if resp.Content != "Hello, world!" {
		t.Errorf("content = %q, want %q", resp.Content, "Hello, world!")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil, want non-nil")
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}
	if resp.RouteOutcome.Score != 0.85 {
		t.Errorf("Score = %.2f, want 0.85", resp.RouteOutcome.Score)
	}
	if resp.RouteOutcome.PlannedModel.Model != "test-model" {
		t.Errorf("PlannedModel = %v, want test-model", resp.RouteOutcome.PlannedModel)
	}

	successes := rec.getSuccesses()
	if len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
	if successes[0].Model != "test-model" {
		t.Errorf("success key = %v, want test-model", successes[0])
	}

	warmth := rec.getWarmthUses()
	if len(warmth) != 1 {
		t.Fatalf("warmthUses = %d, want 1", len(warmth))
	}
}

func TestRoutePlanExecuteChatStreamAttachesRouteOutcome(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapChat | CapStream,
		chatStreamChunks: []ChatResponse{
			{Model: "test-model", Content: "Hello", Partial: true},
			{Model: "test-model", Content: ", world!", Done: true},
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)
	plan.wasSticky = true

	var chunks []ChatResponse
	err := plan.ExecuteChatStream(context.Background(), func(chunk ChatResponse) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteChatStream returned unexpected error: %v", err)
	}

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}

	// First chunk should NOT have RouteOutcome.
	if chunks[0].RouteOutcome != nil {
		t.Error("first chunk has RouteOutcome, want nil")
	}

	// Final Done chunk MUST have RouteOutcome.
	final := chunks[1]
	if final.RouteOutcome == nil {
		t.Fatal("final chunk RouteOutcome is nil, want non-nil")
	}
	if !final.RouteOutcome.WasSticky {
		t.Error("WasSticky = false, want true")
	}
	if final.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", final.RouteOutcome.FallbacksUsed)
	}
}

func TestRoutePlanBudgetTruncateRejectsExecution(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapChat,
		chatResp: &ChatResponse{
			Model:   "test-model",
			Content: "should not get here",
			Done:    true,
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)
	plan.Budget = BudgetResult{Decision: BudgetTruncate}

	// Chat
	_, err := plan.ExecuteChat(context.Background())
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteChat error = %v, want ErrBudgetAdaptationRequired", err)
	}

	// ChatStream
	err = plan.ExecuteChatStream(context.Background(), func(_ ChatResponse) error {
		t.Error("callback should not be invoked")
		return nil
	})
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteChatStream error = %v, want ErrBudgetAdaptationRequired", err)
	}

	// Generate
	plan.genProvider()
	_, err = plan.ExecuteGenerate(context.Background())
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteGenerate error = %v, want ErrBudgetAdaptationRequired", err)
	}

	// GenerateStream
	err = plan.ExecuteGenerateStream(context.Background(), func(_ GenerateResponse) error {
		t.Error("callback should not be invoked")
		return nil
	})
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteGenerateStream error = %v, want ErrBudgetAdaptationRequired", err)
	}

	// Embed
	_, err = plan.ExecuteEmbed(context.Background())
	if !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("ExecuteEmbed error = %v, want ErrBudgetAdaptationRequired", err)
	}

	// Provider should never have been called.
	if prov.getChatCalls() != 0 {
		t.Errorf("chatCalls = %d, want 0", prov.getChatCalls())
	}
}

// genProvider is a helper that does nothing; the plan already has a provider.
// It exists to make the test read clearly.
func (rp *RoutePlan) genProvider() {}

func TestRoutePlanFallbackOnInfraError(t *testing.T) {
	infraErr := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}

	primaryProv := &rpMockProvider{
		name:    "primary",
		caps:    CapChat,
		chatErr: infraErr,
	}
	fallbackProv := &rpMockProvider{
		name: "fallback",
		caps: CapChat,
		chatResp: &ChatResponse{
			Model:   "fallback-model",
			Content: "from fallback",
			Done:    true,
		},
	}

	rec := &rpMockRecorder{}
	primaryKey := ModelKey{Provider: "primary", Model: "primary-model"}
	fallbackKey := ModelKey{Provider: "fallback", Model: "fallback-model"}

	plan := &RoutePlan{
		Kind:     RouteKindChat,
		Provider: primaryProv,
		Model:    "primary-model",
		Profile:  &ModelProfile{Key: primaryKey, ContextWindow: 32768},
		Request: RoutingRequest{
			Messages: []ChatMessage{{Role: "user", Content: "test"}},
		},
		Score:  0.90,
		Budget: BudgetResult{Decision: BudgetOK},
		Reason: "primary selected",
		Fallbacks: []RoutePlan{
			{
				Kind:     RouteKindChat,
				Provider: fallbackProv,
				Model:    "fallback-model",
				Profile:  &ModelProfile{Key: fallbackKey, ContextWindow: 32768},
				Request: RoutingRequest{
					Messages: []ChatMessage{{Role: "user", Content: "test"}},
				},
				Score:  0.70,
				Budget: BudgetResult{Decision: BudgetOK},
				Reason: "fallback",
			},
		},
		recorder: rec,
	}

	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat returned error: %v", err)
	}
	if resp.Content != "from fallback" {
		t.Errorf("content = %q, want %q", resp.Content, "from fallback")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil")
	}
	if resp.RouteOutcome.FallbacksUsed != 1 {
		t.Errorf("FallbacksUsed = %d, want 1", resp.RouteOutcome.FallbacksUsed)
	}
	if resp.RouteOutcome.ActualModel != fallbackKey {
		t.Errorf("ActualModel = %v, want %v", resp.RouteOutcome.ActualModel, fallbackKey)
	}
	if resp.RouteOutcome.PlannedModel != primaryKey {
		t.Errorf("PlannedModel = %v, want %v", resp.RouteOutcome.PlannedModel, primaryKey)
	}

	// Primary should have 1 failure recorded.
	failures := rec.getFailures()
	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(failures))
	}
	if failures[0] != primaryKey {
		t.Errorf("failed key = %v, want %v", failures[0], primaryKey)
	}

	// Fallback success should be recorded.
	successes := rec.getSuccesses()
	if len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
	if successes[0] != fallbackKey {
		t.Errorf("success key = %v, want %v", successes[0], fallbackKey)
	}
}

func TestRoutePlanChatStreamDoesNotFallbackAfterFirstChunk(t *testing.T) {
	infraErr := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}

	// Primary delivers one content chunk, then returns infra error.
	primaryProv := &rpMockProvider{
		name: "primary",
		caps: CapChat | CapStream,
		chatStreamChunks: []ChatResponse{
			{Model: "primary-model", Content: "partial content", Partial: true},
		},
		chatStreamErr: infraErr,
	}

	fallbackProv := &rpMockProvider{
		name: "fallback",
		caps: CapChat | CapStream,
		chatStreamChunks: []ChatResponse{
			{Model: "fallback-model", Content: "fallback content", Done: true},
		},
	}

	rec := &rpMockRecorder{}
	primaryKey := ModelKey{Provider: "primary", Model: "primary-model"}
	fallbackKey := ModelKey{Provider: "fallback", Model: "fallback-model"}

	plan := &RoutePlan{
		Kind:     RouteKindChat,
		Provider: primaryProv,
		Model:    "primary-model",
		Profile:  &ModelProfile{Key: primaryKey, ContextWindow: 32768},
		Request:  RoutingRequest{Messages: []ChatMessage{{Role: "user", Content: "test"}}},
		Score:    0.90,
		Budget:   BudgetResult{Decision: BudgetOK},
		Reason:   "primary",
		Fallbacks: []RoutePlan{
			{
				Kind:     RouteKindChat,
				Provider: fallbackProv,
				Model:    "fallback-model",
				Profile:  &ModelProfile{Key: fallbackKey, ContextWindow: 32768},
				Request:  RoutingRequest{Messages: []ChatMessage{{Role: "user", Content: "test"}}},
				Score:    0.70,
				Budget:   BudgetResult{Decision: BudgetOK},
				Reason:   "fallback",
			},
		},
		recorder: rec,
	}

	var chunks []ChatResponse
	err := plan.ExecuteChatStream(context.Background(), func(chunk ChatResponse) error {
		chunks = append(chunks, chunk)
		return nil
	})

	// Error should be returned (no fallback after content delivery).
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, infraErr) {
		t.Errorf("error = %v, want %v", err, infraErr)
	}

	// Fallback provider should NOT have been called.
	if fallbackProv.getChatCalls() != 0 {
		t.Errorf("fallback chatCalls = %d, want 0", fallbackProv.getChatCalls())
	}

	// One chunk should have been delivered from primary.
	if len(chunks) != 1 {
		t.Errorf("chunks = %d, want 1", len(chunks))
	}
}

func TestRoutePlanCancellationNotRecorded(t *testing.T) {
	prov := &rpMockProvider{
		name:    "ollama",
		caps:    CapChat,
		chatErr: context.Canceled,
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)

	_, err := plan.ExecuteChat(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// No success, no failure — but warmthUse IS recorded.
	successes := rec.getSuccesses()
	if len(successes) != 0 {
		t.Errorf("successes = %d, want 0", len(successes))
	}
	failures := rec.getFailures()
	if len(failures) != 0 {
		t.Errorf("failures = %d, want 0", len(failures))
	}
	warmth := rec.getWarmthUses()
	if len(warmth) != 1 {
		t.Errorf("warmthUses = %d, want 1", len(warmth))
	}
}

func TestRoutePlanExecuteGenerate(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapGenerate,
		genResp: &GenerateResponse{
			Model:    "test-model",
			Response: "generated text",
			Done:     true,
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindGenerate
	plan.Request.Prompt = "complete this"

	resp, err := plan.ExecuteGenerate(context.Background())
	if err != nil {
		t.Fatalf("ExecuteGenerate returned unexpected error: %v", err)
	}
	if resp.Response != "generated text" {
		t.Errorf("response = %q, want %q", resp.Response, "generated text")
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil, want non-nil")
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}

	successes := rec.getSuccesses()
	if len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}

func TestRoutePlanExecuteGenerateStreamAttachesRouteOutcome(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapGenerate | CapStream,
		genStreamChunks: []GenerateResponse{
			{Model: "test-model", Response: "chunk1", Partial: true},
			{Model: "test-model", Response: "chunk2", Done: true},
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindGenerate
	plan.Request.Prompt = "generate this"

	var chunks []GenerateResponse
	err := plan.ExecuteGenerateStream(context.Background(), func(chunk GenerateResponse) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteGenerateStream returned unexpected error: %v", err)
	}

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}

	// First chunk should NOT have RouteOutcome.
	if chunks[0].RouteOutcome != nil {
		t.Error("first chunk has RouteOutcome, want nil")
	}

	// Final Done chunk MUST have RouteOutcome.
	final := chunks[1]
	if final.RouteOutcome == nil {
		t.Fatal("final chunk RouteOutcome is nil, want non-nil")
	}
	if final.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", final.RouteOutcome.FallbacksUsed)
	}
}

func TestRoutePlanExecuteEmbed(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama",
		caps: CapEmbed,
		embedResp: &EmbedResponse{
			Model:      "embed-model",
			Embeddings: [][]float64{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}},
		},
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindEmbed
	plan.Model = "embed-model"
	plan.Request.Input = []string{"text1", "text2"}

	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil {
		t.Fatalf("ExecuteEmbed returned unexpected error: %v", err)
	}
	if len(resp.Embeddings) != 2 {
		t.Fatalf("embeddings count = %d, want 2", len(resp.Embeddings))
	}
	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome is nil, want non-nil")
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}

	successes := rec.getSuccesses()
	if len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}

func TestRoutePlanRecordOutcomeFeedbackNilSafe(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*RoutePlan)
		outcome *RouteOutcome
	}{
		{
			name:    "nil-feedback",
			setup:   func(rp *RoutePlan) { rp.SetFeedback(nil) },
			outcome: &RouteOutcome{},
		},
		{
			name:    "nil-outcome",
			setup:   func(rp *RoutePlan) {},
			outcome: nil,
		},
		{
			name: "empty-usecase",
			setup: func(rp *RoutePlan) {
				rp.Request = RoutingRequest{UseCase: ""}
				store, _ := NewMemoryStore(MemoryStoreConfig{})
				rp.SetFeedback(NewRoutingFeedback(store))
			},
			outcome: &RouteOutcome{
				PlannedModel: ModelKey{Provider: "p", Model: "m"},
				Attempts: []RouteAttempt{
					{Key: ModelKey{Provider: "p", Model: "m"}, Status: AttemptStatusSucceeded},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := &RoutePlan{Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}}}
			tc.setup(rp)
			// Must not panic, must not return any error (helper returns void).
			rp.recordOutcomeFeedback(tc.outcome)
		})
	}
}

func TestRoutePlanRecordOutcomeFeedbackDelegatesWhenConfigured(t *testing.T) {
	store, _ := NewMemoryStore(MemoryStoreConfig{})
	rf := NewRoutingFeedback(store)
	rp := &RoutePlan{
		Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}},
		Request: RoutingRequest{UseCase: "chat"},
	}
	rp.SetFeedback(rf)

	outcome := &RouteOutcome{
		PlannedModel: ModelKey{Provider: "p", Model: "m"},
		Attempts: []RouteAttempt{
			// Keep LatencyMs at zero so this helper test only asserts
			// delegation of the Success signal. Latency emission is covered
			// by RecordOutcome tests and Task 11's end-to-end test.
			{Key: ModelKey{Provider: "p", Model: "m"}, Status: AttemptStatusSucceeded},
		},
	}
	rp.recordOutcomeFeedback(outcome)

	agg, _ := store.Get(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"})
	if agg.SampleCount != 1 {
		t.Fatalf("SampleCount = %d, want 1 (helper should have recorded the success)", agg.SampleCount)
	}
}

func TestRoutePlanRecordOutcomeFeedbackSwallowsStoreErrors(t *testing.T) {
	// NewRoutingFeedback(nil) returns a wrapper that returns
	// ErrNilRoutingFeedbackStore on every call. recordOutcomeFeedback must
	// swallow that error rather than propagating.
	rp := &RoutePlan{
		Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}},
		Request: RoutingRequest{UseCase: "chat"},
	}
	rp.SetFeedback(NewRoutingFeedback(nil))

	outcome := &RouteOutcome{
		Attempts: []RouteAttempt{{Key: ModelKey{Provider: "p", Model: "m"}, Status: AttemptStatusSucceeded}},
	}
	// Must not panic; must not log; must return void.
	rp.recordOutcomeFeedback(outcome)
}

func TestHandleResultAlwaysBuildsOutcomeOnSuccess(t *testing.T) {
	rp := newTestPlan(&rpMockProvider{name: "ollama", caps: CapChat}, &rpMockRecorder{})
	out := rp.handleResult(nil, 0, nil)
	if out == nil {
		t.Fatal("outcome is nil; want non-nil on success")
	}
	if out.PlannedModel.Provider != "ollama" {
		t.Errorf("PlannedModel.Provider = %q", out.PlannedModel.Provider)
	}
}

func TestHandleResultAlwaysBuildsOutcomeOnCancellation(t *testing.T) {
	rp := newTestPlan(&rpMockProvider{name: "ollama", caps: CapChat}, &rpMockRecorder{})
	out := rp.handleResult(context.Canceled, 0, nil)
	if out == nil {
		t.Fatal("outcome is nil; want non-nil on cancellation (PR2 always builds)")
	}
}

func TestHandleResultAlwaysBuildsOutcomeOnInfraFailure(t *testing.T) {
	rp := newTestPlan(&rpMockProvider{name: "ollama", caps: CapChat}, &rpMockRecorder{})
	out := rp.handleResult(&HTTPStatusError{StatusCode: 500}, 0, nil)
	if out == nil {
		t.Fatal("outcome is nil; want non-nil on infra failure")
	}
}

func TestHandleResultUsesLastAttemptKeyForCancellationWarmth(t *testing.T) {
	rec := &rpMockRecorder{}
	rp := newTestPlan(&rpMockProvider{name: "ollama-a", caps: CapChat}, rec)
	// Fabricate two attempts — primary failed (network), fallback canceled.
	attempts := []RouteAttempt{
		{Key: ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "network"},
		{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}, Status: AttemptStatusUnknown},
	}
	_ = rp.handleResult(context.Canceled, 0, attempts)

	warmth := rec.getWarmthUses()
	if len(warmth) != 1 {
		t.Fatalf("warmthUses = %d, want 1", len(warmth))
	}
	if warmth[0].Provider != "ollama-b" {
		t.Errorf("warmth target = %q, want ollama-b (last attempted key)", warmth[0].Provider)
	}
}

func TestExecuteChatTracksPrimarySuccessAttempt(t *testing.T) {
	prov := &rpMockProvider{name: "ollama", caps: CapChat, chatResp: &ChatResponse{Content: "hi", Done: true}}
	plan := newTestPlan(prov, &rpMockRecorder{})
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp == nil || resp.RouteOutcome == nil {
		t.Fatal("resp/outcome nil")
	}
	att := resp.RouteOutcome.Attempts
	if len(att) != 1 {
		t.Fatalf("Attempts len = %d, want 1", len(att))
	}
	if att[0].Status != AttemptStatusSucceeded {
		t.Errorf("Status = %v, want Succeeded", att[0].Status)
	}
	if att[0].ErrorClass != "" {
		t.Errorf("ErrorClass = %q, want empty on success", att[0].ErrorClass)
	}
}

func TestExecuteChatTracksPrimaryFailFallbackSuccessAttempts(t *testing.T) {
	primary := &rpMockProvider{name: "ollama-a", caps: CapChat, chatErr: &HTTPStatusError{StatusCode: 500}}
	fallback := &rpMockProvider{name: "ollama-b", caps: CapChat, chatResp: &ChatResponse{Content: "ok", Done: true}}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.RouteOutcome == nil {
		t.Fatal("outcome nil")
	}
	att := resp.RouteOutcome.Attempts
	if len(att) != 2 {
		t.Fatalf("Attempts len = %d, want 2 (primary fail + fallback success)", len(att))
	}
	if att[0].Status != AttemptStatusFailed || att[0].ErrorClass != string(ErrorClass5xx) {
		t.Errorf("att[0] = %+v, want Failed/5xx", att[0])
	}
	if att[1].Status != AttemptStatusSucceeded || att[1].Key.Provider != "ollama-b" {
		t.Errorf("att[1] = %+v, want Succeeded for ollama-b", att[1])
	}
}

func TestExecuteChatTracksAllFailAttempts(t *testing.T) {
	prov := &rpMockProvider{name: "ollama-a", caps: CapChat, chatErr: &HTTPStatusError{StatusCode: 500}}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: &rpMockProvider{name: "ollama-b", caps: CapChat, chatErr: &HTTPStatusError{StatusCode: 503}},
		Model:    "qwen3:8b",
	}}

	_, err := plan.ExecuteChat(context.Background())
	if err == nil {
		t.Fatal("err nil; want failure")
	}
	// resp is nil so we cannot read outcome via response. Instead, the
	// outcome is built and consumed only by recordOutcomeFeedback (no-op
	// here because feedback is nil). We verify by inspecting the plan via
	// a future integration test (Task 11). For now we just assert no panic
	// and the right error is surfaced.
	if !errors.As(err, new(*HTTPStatusError)) {
		t.Errorf("err type = %T, want *HTTPStatusError", err)
	}
}

func TestExecuteChatCancellationProducesUnknownAttempt(t *testing.T) {
	prov := &rpMockProvider{name: "ollama", caps: CapChat, chatErr: context.Canceled}
	plan := newTestPlan(prov, &rpMockRecorder{})

	resp, err := plan.ExecuteChat(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if resp != nil {
		t.Fatalf("resp = %+v, want nil on cancellation", resp)
	}
	// outcome is built but only consumed by the (nil) feedback seam.
	// We can't observe Attempts from the test surface without integration
	// wiring — Task 11 covers that. Here we just lock in that the path
	// completes without panic.
}

func TestRoutePlanString(t *testing.T) {
	plan := &RoutePlan{
		Model: "qwen3:8b",
		Profile: &ModelProfile{
			Key: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		},
		Score:  0.85,
		Budget: BudgetResult{Decision: BudgetOK},
		Fallbacks: []RoutePlan{
			{Model: "fallback"},
		},
		Reason: "best candidate",
	}

	got := plan.String()
	want := "ollama/qwen3:8b (score=0.85, budget=ok, fallbacks=1): best candidate"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestExecuteGenerateTracksPrimarySuccessAttempt(t *testing.T) {
	prov := &rpMockProvider{name: "ollama", caps: CapGenerate, genResp: &GenerateResponse{Response: "hi", Done: true}}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	resp, err := plan.ExecuteGenerate(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.RouteOutcome == nil || len(resp.RouteOutcome.Attempts) != 1 {
		t.Fatalf("Attempts len = %d, want 1", len(resp.RouteOutcome.Attempts))
	}
	if resp.RouteOutcome.Attempts[0].Status != AttemptStatusSucceeded {
		t.Errorf("Status = %v, want Succeeded", resp.RouteOutcome.Attempts[0].Status)
	}
}

func TestExecuteGenerateTracksPrimaryFailFallbackSuccessAttempts(t *testing.T) {
	primary := &rpMockProvider{name: "ollama-a", caps: CapGenerate, genErr: &HTTPStatusError{StatusCode: 500}}
	fallback := &rpMockProvider{name: "ollama-b", caps: CapGenerate, genResp: &GenerateResponse{Response: "ok", Done: true}}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	resp, err := plan.ExecuteGenerate(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	att := resp.RouteOutcome.Attempts
	if len(att) != 2 {
		t.Fatalf("Attempts len = %d, want 2", len(att))
	}
	if att[0].ErrorClass != string(ErrorClass5xx) {
		t.Errorf("att[0].ErrorClass = %q, want 5xx", att[0].ErrorClass)
	}
	if att[1].Status != AttemptStatusSucceeded {
		t.Errorf("att[1].Status = %v, want Succeeded", att[1].Status)
	}
}

func TestExecuteEmbedTracksPrimarySuccessAttempt(t *testing.T) {
	prov := &rpMockProvider{name: "ollama", caps: CapEmbed, embedResp: &EmbedResponse{Embeddings: [][]float64{{0.1}}}}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Kind = RouteKindEmbed
	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.RouteOutcome == nil || len(resp.RouteOutcome.Attempts) != 1 {
		t.Fatalf("Attempts len = %d, want 1", len(resp.RouteOutcome.Attempts))
	}
	if resp.RouteOutcome.Attempts[0].Status != AttemptStatusSucceeded {
		t.Errorf("Status = %v, want Succeeded", resp.RouteOutcome.Attempts[0].Status)
	}
}

func TestExecuteEmbedTracksPrimaryFailFallbackSuccessAttempts(t *testing.T) {
	primary := &rpMockProvider{name: "ollama-a", caps: CapEmbed, embedErr: &HTTPStatusError{StatusCode: 500}}
	fallback := &rpMockProvider{name: "ollama-b", caps: CapEmbed, embedResp: &EmbedResponse{Embeddings: [][]float64{{0.2}}}}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Kind = RouteKindEmbed
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "embed-1"}},
		Provider: fallback,
		Model:    "embed-1",
	}}

	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	att := resp.RouteOutcome.Attempts
	if len(att) != 2 || att[0].ErrorClass != string(ErrorClass5xx) || att[1].Status != AttemptStatusSucceeded {
		t.Fatalf("attempts = %+v, want [Failed/5xx, Succeeded]", att)
	}
}

func TestExecuteChatStreamFinalizesOnNonPartialDone(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Content: "i", Done: true}, // Partial defaults to false
		},
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	var got []ChatResponse
	err := plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	final := got[len(got)-1]
	if final.RouteOutcome == nil {
		t.Fatal("final chunk RouteOutcome nil")
	}
	att := final.RouteOutcome.Attempts
	if len(att) != 1 || att[0].Status != AttemptStatusSucceeded {
		t.Fatalf("attempts = %+v, want [Succeeded]", att)
	}
}

func TestExecuteChatStreamDonePartialDefersToPostStream(t *testing.T) {
	// Ollama emits {Done: true, Partial: true} on cancellation, then
	// returns ctx.Err(). The handler must NOT finalize on partial-Done;
	// post-stream code finalizes with the real error.
	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Done: true, Partial: true}, // synthetic partial Done
		},
		chatStreamErr: context.Canceled,
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	var lastChunk ChatResponse
	err := plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		lastChunk = c
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	// The partial-Done chunk must NOT have carried a RouteOutcome.
	if lastChunk.RouteOutcome != nil {
		t.Errorf("partial-Done chunk got RouteOutcome; want nil (post-stream finalizes)")
	}
	// We can't observe Attempts via the response (no resp returned for
	// streams), but the test's purpose is the negative assertion above
	// + ensuring no panic. Task 11 integration test asserts the attempt
	// is finalized as Unknown via a real RoutingFeedback store.
}

func TestExecuteChatStreamCallbackErrorAttributedAsUnknown(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Content: "i", Done: true},
		},
	}
	plan := newTestPlan(prov, &rpMockRecorder{})

	callbackErr := errors.New("user aborted")
	gotCalls := 0
	err := plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		gotCalls++
		if gotCalls == 1 {
			return callbackErr // abort after first chunk
		}
		return nil
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}
	// We can't inspect attempts here; the load-bearing test is in Task 11's
	// integration coverage with a real feedback store. The point of THIS
	// test is to confirm the error path completes without panic and the
	// callback error is propagated as-is.
}

func TestExecuteChatStreamFallbackOnPrimaryInfraError(t *testing.T) {
	primary := &rpMockProvider{
		name: "ollama-a", caps: CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "ok"},
			{Content: "", Done: true},
		},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	var lastChunk ChatResponse
	err := plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		lastChunk = c
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil after fallback success", err)
	}
	if lastChunk.RouteOutcome == nil {
		t.Fatal("final-chunk outcome nil")
	}
	att := lastChunk.RouteOutcome.Attempts
	if len(att) != 2 {
		t.Fatalf("attempts len = %d, want 2 (primary Failed + fallback Succeeded)", len(att))
	}
	if att[0].Status != AttemptStatusFailed || att[0].ErrorClass != string(ErrorClass5xx) {
		t.Errorf("att[0] = %+v, want Failed/5xx", att[0])
	}
	if att[1].Status != AttemptStatusSucceeded || att[1].Key.Provider != "ollama-b" {
		t.Errorf("att[1] = %+v, want Succeeded/ollama-b", att[1])
	}
}

func TestExecuteChatStreamFallbackSuccessWithoutDoneRecordsAttempt(t *testing.T) {
	// A fallback provider that returns err==nil without emitting a Done
	// chunk must still call handleResult so the feedback seam records the
	// success. Without the fix, return-nil-on-success short-circuited the
	// post-stream finalization, leaving the feedback store empty.
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	primary := &rpMockProvider{
		name: "ollama-a", caps: CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	// Fallback emits one chunk but NO Done chunk, then returns nil.
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "partial without Done"},
		},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Request.UseCase = "chat"
	plan.SetFeedback(rf)
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	if err := plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteChatStream: %v", err)
	}

	// Fallback success should be recorded against ollama-b.
	key := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount < 1 {
		t.Errorf("fallback ScoredCount = %d, want >= 1 (Success signal should have been recorded)", agg.ScoredCount)
	}
}

func TestExecuteGenerateStreamFinalizesOnNonPartialDone(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "h"},
			{Response: "i", Done: true},
		},
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	var lastChunk GenerateResponse
	err := plan.ExecuteGenerateStream(context.Background(), func(c GenerateResponse) error {
		lastChunk = c
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if lastChunk.RouteOutcome == nil {
		t.Fatal("outcome nil")
	}
	att := lastChunk.RouteOutcome.Attempts
	if len(att) != 1 || att[0].Status != AttemptStatusSucceeded {
		t.Fatalf("attempts = %+v, want [Succeeded]", att)
	}
}

func TestExecuteGenerateStreamDonePartialDefersToPostStream(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "h"},
			{Done: true, Partial: true},
		},
		genStreamErr: context.Canceled,
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	var lastChunk GenerateResponse
	err := plan.ExecuteGenerateStream(context.Background(), func(c GenerateResponse) error {
		lastChunk = c
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	if lastChunk.RouteOutcome != nil {
		t.Errorf("partial-Done chunk got RouteOutcome; want nil")
	}
}

func TestExecuteGenerateStreamCallbackErrorAttributedAsUnknown(t *testing.T) {
	prov := &rpMockProvider{
		name: "ollama", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "h"},
			{Response: "i", Done: true},
		},
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate

	callbackErr := errors.New("user aborted")
	err := plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}
}

func TestExecuteGenerateStreamFallbackOnPrimaryInfraError(t *testing.T) {
	primary := &rpMockProvider{
		name: "ollama-a", caps: CapGenerate,
		genStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "ok"},
			{Done: true},
		},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	var lastChunk GenerateResponse
	err := plan.ExecuteGenerateStream(context.Background(), func(c GenerateResponse) error {
		lastChunk = c
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	att := lastChunk.RouteOutcome.Attempts
	if len(att) != 2 || att[0].ErrorClass != string(ErrorClass5xx) || att[1].Status != AttemptStatusSucceeded {
		t.Fatalf("attempts = %+v, want [Failed/5xx, Succeeded]", att)
	}
}

func TestExecuteChatEndToEndRecordsPerAttemptFeedback(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	primary := &rpMockProvider{
		name: "ollama-a", caps: CapChat,
		chatErr: &net.OpError{Op: "dial", Err: errors.New("refused")},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapChat,
		chatResp: &ChatResponse{Content: "hello", Done: true},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Request.UseCase = "chat"
	plan.Model = "qwen3:8b"
	plan.Profile.Key.Model = "qwen3:8b"
	plan.SetFeedback(rf)
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	if resp.RouteOutcome == nil {
		t.Fatal("response RouteOutcome nil")
	}

	// Primary key should have one scored Failure (network). Latency is
	// emitted only when elapsed wall-clock milliseconds are >0, so this
	// test must not require it.
	primaryKey := FeedbackKey{Provider: "ollama-a", Model: "qwen3:8b", UseCase: "chat"}
	pa, _ := store.Get(context.Background(), primaryKey)
	if pa.SampleCount < 1 || pa.SampleCount > 2 {
		t.Errorf("primary SampleCount = %d, want 1 or 2 (Failure, optional Latency)", pa.SampleCount)
	}
	if pa.ScoredCount != 1 {
		t.Errorf("primary ScoredCount = %d, want 1 (Failure scores; optional Latency doesn't)", pa.ScoredCount)
	}
	// Score for one Failure (-0.7) clipped = -0.7; mean = -0.7; score = 0.15.
	const wantPrimaryScore = 0.15
	if abs := pa.Score - wantPrimaryScore; abs > 1e-15 || abs < -1e-15 {
		t.Errorf("primary Score = %v, want %v (within 1e-15)", pa.Score, wantPrimaryScore)
	}

	// Fallback key should have one scored Success (+0.5), plus optional
	// Latency if elapsed milliseconds were measurable.
	fallbackKey := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
	fa, _ := store.Get(context.Background(), fallbackKey)
	if fa.SampleCount < 1 || fa.SampleCount > 2 {
		t.Errorf("fallback SampleCount = %d, want 1 or 2 (Success, optional Latency)", fa.SampleCount)
	}
	if fa.ScoredCount != 1 {
		t.Errorf("fallback ScoredCount = %d, want 1", fa.ScoredCount)
	}
	if fa.Score != 0.75 {
		t.Errorf("fallback Score = %v, want 0.75", fa.Score)
	}

	// Verify the stored signals carry the right ErrorClass for the
	// primary failure. Inspect under lock since signals isn't exported.
	store.mu.Lock()
	pSigs := store.signals[primaryKey]
	store.mu.Unlock()
	var failureCount int
	for _, sig := range pSigs {
		switch sig.Kind {
		case RoutingSignalFailure:
			failureCount++
			if sig.ErrorClass != "network" {
				t.Errorf("primary Failure ErrorClass = %q, want %q", sig.ErrorClass, "network")
			}
		}
	}
	if failureCount != 1 {
		t.Errorf("primary Failure signals = %d, want 1", failureCount)
	}
}

func TestExecuteChatStreamPartialCanceledDoesNotRecordSuccess(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Done: true, Partial: true},
		},
		chatStreamErr: context.Canceled,
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.SetFeedback(rf)

	var last ChatResponse
	err = plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		last = c
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if last.RouteOutcome != nil {
		t.Fatalf("partial Done chunk carried RouteOutcome; want nil")
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0 (Canceled is AttemptStatusUnknown)", agg.SampleCount)
	}
}

func TestExecuteChatStreamCallbackErrorDoesNotRecordProviderFailure(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Content: "i", Done: true},
		},
	}
	plan := newTestPlan(prov, &rpMockRecorder{})
	plan.SetFeedback(rf)

	callbackErr := errors.New("user aborted")
	err = plan.ExecuteChatStream(context.Background(), func(ChatResponse) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0 (callback abort is AttemptStatusUnknown)", agg.SampleCount)
	}
}

func TestExecuteChatStreamFinalDoneCallbackErrorDoesNotRecordSuccess(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Content: "h"},
			{Done: true},
		},
	}
	plan := newTestPlan(prov, rec)
	plan.SetFeedback(rf)

	callbackErr := errors.New("client disconnected on final chunk")
	err = plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		if c.Done {
			if c.RouteOutcome == nil {
				t.Fatal("final Done chunk RouteOutcome nil")
			}
			return callbackErr
		}
		return nil
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0 (final callback abort is AttemptStatusUnknown)", agg.SampleCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 0 {
		t.Fatalf("successes = %d, want 0", len(successes))
	}
}

func TestExecuteGenerateStreamFallbackSuccessWithoutDoneRecordsAttempt(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)

	primary := &rpMockProvider{
		name: "ollama-a", caps: CapGenerate,
		genStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "partial without Done"},
		},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Kind = RouteKindGenerate
	plan.Request.UseCase = "chat"
	plan.SetFeedback(rf)
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	if err := plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteGenerateStream: %v", err)
	}

	key := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount < 1 {
		t.Errorf("fallback ScoredCount = %d, want >= 1 (Success signal should have been recorded)", agg.ScoredCount)
	}
}

func TestExecuteGenerateStreamFinalDoneCallbackErrorDoesNotRecordSuccess(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	prov := &rpMockProvider{
		name: "ollama", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Response: "h"},
			{Done: true},
		},
	}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindGenerate
	plan.SetFeedback(rf)

	callbackErr := errors.New("client disconnected on final chunk")
	err = plan.ExecuteGenerateStream(context.Background(), func(c GenerateResponse) error {
		if c.Done {
			if c.RouteOutcome == nil {
				t.Fatal("final Done chunk RouteOutcome nil")
			}
			return callbackErr
		}
		return nil
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.SampleCount != 0 {
		t.Fatalf("SampleCount = %d, want 0 (final callback abort is AttemptStatusUnknown)", agg.SampleCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 0 {
		t.Fatalf("successes = %d, want 0", len(successes))
	}
}

func TestExecuteChatStreamDuplicateDoneRecordsAttemptOnce(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	prov := &rpMockProvider{
		name: "ollama", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Done: true},
			{Done: true},
		},
	}
	plan := newTestPlan(prov, rec)
	plan.SetFeedback(rf)

	if err := plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteChatStream: %v", err)
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1 (duplicate Done must not replay outcome)", agg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}

func TestExecuteGenerateStreamDuplicateDoneRecordsAttemptOnce(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	prov := &rpMockProvider{
		name: "ollama", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Done: true},
			{Done: true},
		},
	}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindGenerate
	plan.SetFeedback(rf)

	if err := plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteGenerateStream: %v", err)
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1 (duplicate Done must not replay outcome)", agg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}

func TestExecuteChatStreamFallbackDuplicateDoneRecordsAttemptOnce(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	primary := &rpMockProvider{
		name:          "ollama-a",
		caps:          CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			{Done: true},
			{Done: true},
		},
	}
	plan := newTestPlan(primary, rec)
	plan.Profile.Key.Model = "qwen3:8b"
	plan.SetFeedback(rf)
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	if err := plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteChatStream: %v", err)
	}

	primaryKey := FeedbackKey{Provider: "ollama-a", Model: "qwen3:8b", UseCase: "chat"}
	primaryAgg, _ := store.Get(context.Background(), primaryKey)
	if primaryAgg.ScoredCount != 1 {
		t.Fatalf("primary ScoredCount = %d, want 1", primaryAgg.ScoredCount)
	}
	fallbackKey := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
	fallbackAgg, _ := store.Get(context.Background(), fallbackKey)
	if fallbackAgg.ScoredCount != 1 {
		t.Fatalf("fallback ScoredCount = %d, want 1", fallbackAgg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}

func TestExecuteGenerateStreamFallbackDuplicateDoneRecordsAttemptOnce(t *testing.T) {
	store, err := NewMemoryStore(MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	rf := NewRoutingFeedback(store)
	rec := &rpMockRecorder{}

	primary := &rpMockProvider{
		name:         "ollama-a",
		caps:         CapGenerate,
		genStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fallback := &rpMockProvider{
		name: "ollama-b", caps: CapGenerate,
		genStreamChunks: []GenerateResponse{
			{Done: true},
			{Done: true},
		},
	}
	plan := newTestPlan(primary, rec)
	plan.Kind = RouteKindGenerate
	plan.Profile.Key.Model = "qwen3:8b"
	plan.SetFeedback(rf)
	plan.Fallbacks = []RoutePlan{{
		Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
		Provider: fallback,
		Model:    "qwen3:8b",
	}}

	if err := plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil }); err != nil {
		t.Fatalf("ExecuteGenerateStream: %v", err)
	}

	primaryKey := FeedbackKey{Provider: "ollama-a", Model: "qwen3:8b", UseCase: "chat"}
	primaryAgg, _ := store.Get(context.Background(), primaryKey)
	if primaryAgg.ScoredCount != 1 {
		t.Fatalf("primary ScoredCount = %d, want 1", primaryAgg.ScoredCount)
	}
	fallbackKey := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
	fallbackAgg, _ := store.Get(context.Background(), fallbackKey)
	if fallbackAgg.ScoredCount != 1 {
		t.Fatalf("fallback ScoredCount = %d, want 1", fallbackAgg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
}
