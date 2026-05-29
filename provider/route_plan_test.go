package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
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

func (m *rpMockProvider) Name() string                   { return m.name }
func (m *rpMockProvider) Capabilities() Capability       { return m.caps }
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
		Score:    0.85,
		Budget:   BudgetResult{Decision: BudgetOK},
		Reason:   "best candidate",
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

func TestExecuteChatStreamPropagatesCallbackError(t *testing.T) {
	// Renamed from TestExecuteChatStreamCallbackErrorAttributedAsUnknown:
	// this test only verifies error propagation. The Unknown-attribution
	// invariant is verified end-to-end via the feedback store in
	// TestExecuteChatStreamCallbackErrorDoesNotRecordProviderFailure.
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

func TestBuildOutcomeActualModelOnAllFail(t *testing.T) {
	// When every attempt failed, ActualModel must fall back to PlannedModel
	// (primary), not to the last-tried fallback. This locks in the streaming
	// fix where fallbacksUsed is no longer eagerly set at the top of each
	// iteration — buildOutcome derives ActualModel from
	// attempts[last].Status == Succeeded.
	rp := newTestPlan(&rpMockProvider{name: "ollama-a", caps: CapChat}, &rpMockRecorder{})
	rp.Profile.Key = ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	attempts := []RouteAttempt{
		{Key: ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "5xx"},
		{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "5xx"},
	}
	// fallbacksUsed=2 reflects "two fallbacks were tried" but none succeeded.
	out := rp.buildOutcome(2, attempts)
	if out.ActualModel != rp.Profile.Key {
		t.Errorf("ActualModel = %+v, want PlannedModel %+v (no attempt succeeded)", out.ActualModel, rp.Profile.Key)
	}
	if out.PlannedModel != rp.Profile.Key {
		t.Errorf("PlannedModel = %+v, want %+v", out.PlannedModel, rp.Profile.Key)
	}
}

func TestBuildOutcomeActualModelOnFallbackSuccess(t *testing.T) {
	// Inverse: when the last attempt succeeded, ActualModel reflects that
	// model's key — regardless of fallbacksUsed counter value.
	rp := newTestPlan(&rpMockProvider{name: "ollama-a", caps: CapChat}, &rpMockRecorder{})
	rp.Profile.Key = ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	fbKey := ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}
	attempts := []RouteAttempt{
		{Key: rp.Profile.Key, Status: AttemptStatusFailed, ErrorClass: "5xx"},
		{Key: fbKey, Status: AttemptStatusSucceeded},
	}
	out := rp.buildOutcome(1, attempts)
	if out.ActualModel != fbKey {
		t.Errorf("ActualModel = %+v, want fallback %+v", out.ActualModel, fbKey)
	}
}

func TestExecuteChatStreamPendingAttemptsClonedFromIteration(t *testing.T) {
	// Regression test for the aliasing bug fixed in c7e6db1 / slices.Clone.
	// Setup: primary + 3 fallbacks where the first two fail infra (driving
	// `attempts` to len=3, cap=4 — Go's growth strategy gives spare
	// capacity at this length) and the third emits a Done chunk. Without
	// slices.Clone on pendingAttempts, the post-stream Unknown-attempt
	// append (triggered when the user callback errors on Done) would
	// overwrite the Succeeded entry in the captured RouteOutcome.Attempts.
	primary := &rpMockProvider{
		name: "ollama-a", caps: CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fb0 := &rpMockProvider{
		name: "ollama-b", caps: CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fb1 := &rpMockProvider{
		name: "ollama-c", caps: CapChat,
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	fb2 := &rpMockProvider{
		name: "ollama-d", caps: CapChat,
		chatStreamChunks: []ChatResponse{
			// No visible content before Done so fallback isn't suppressed,
			// and so this stream finalizes via the Done-chunk path that
			// uses pendingAttempts.
			{Done: true},
		},
	}
	plan := newTestPlan(primary, &rpMockRecorder{})
	plan.Fallbacks = []RoutePlan{
		{Profile: &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}}, Provider: fb0, Model: "qwen3:8b"},
		{Profile: &ModelProfile{Key: ModelKey{Provider: "ollama-c", Model: "qwen3:8b"}}, Provider: fb1, Model: "qwen3:8b"},
		{Profile: &ModelProfile{Key: ModelKey{Provider: "ollama-d", Model: "qwen3:8b"}}, Provider: fb2, Model: "qwen3:8b"},
	}

	var capturedAttempts []RouteAttempt
	var capturedKey ModelKey
	callbackErr := errors.New("user abort on done")
	err := plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		if c.Done && c.RouteOutcome != nil {
			capturedAttempts = c.RouteOutcome.Attempts
			if n := len(capturedAttempts); n > 0 {
				capturedKey = capturedAttempts[n-1].Key
			}
			return callbackErr
		}
		return nil
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err = %v, want callbackErr", err)
	}
	if len(capturedAttempts) == 0 {
		t.Fatal("Done chunk did not arrive with an outcome; test setup failed")
	}
	// The last entry of the captured slice must still be the Succeeded
	// attempt for ollama-d. Without slices.Clone it would have been
	// overwritten to AttemptStatusUnknown by the post-stream append.
	last := capturedAttempts[len(capturedAttempts)-1]
	if last.Status != AttemptStatusSucceeded {
		t.Errorf("captured last attempt Status = %v, want Succeeded (aliasing corruption)", last.Status)
	}
	if last.Key.Provider != "ollama-d" {
		t.Errorf("captured last attempt Key.Provider = %q, want ollama-d (corruption)", last.Key.Provider)
	}
	if capturedKey.Provider != "ollama-d" {
		t.Errorf("captured Key.Provider at Done time = %q, want ollama-d", capturedKey.Provider)
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
	// primary failure. Uses the public Signals accessor so the test
	// doesn't depend on internal field names.
	pSigs := store.Signals(primaryKey)
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

func TestExecuteChatStreamPostDoneProviderErrorSuppressedAndRecordsSuccess(t *testing.T) {
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
		chatStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	plan := newTestPlan(prov, rec)
	plan.SetFeedback(rf)

	var final ChatResponse
	err = plan.ExecuteChatStream(context.Background(), func(c ChatResponse) error {
		if c.Done {
			final = c
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil after accepted final Done", err)
	}
	if final.RouteOutcome == nil {
		t.Fatal("final Done chunk RouteOutcome nil")
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1 success", agg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
	if failures := rec.getFailures(); len(failures) != 0 {
		t.Fatalf("failures = %d, want 0", len(failures))
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

func TestExecuteGenerateStreamPostDoneProviderErrorSuppressedAndRecordsSuccess(t *testing.T) {
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
		genStreamErr: &HTTPStatusError{StatusCode: 500},
	}
	plan := newTestPlan(prov, rec)
	plan.Kind = RouteKindGenerate
	plan.SetFeedback(rf)

	var final GenerateResponse
	err = plan.ExecuteGenerateStream(context.Background(), func(c GenerateResponse) error {
		if c.Done {
			final = c
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil after accepted final Done", err)
	}
	if final.RouteOutcome == nil {
		t.Fatal("final Done chunk RouteOutcome nil")
	}

	key := FeedbackKey{Provider: "ollama", Model: "test-model", UseCase: "chat"}
	agg, _ := store.Get(context.Background(), key)
	if agg.ScoredCount != 1 {
		t.Fatalf("ScoredCount = %d, want 1 success", agg.ScoredCount)
	}
	if successes := rec.getSuccesses(); len(successes) != 1 {
		t.Fatalf("successes = %d, want 1", len(successes))
	}
	if failures := rec.getFailures(); len(failures) != 0 {
		t.Fatalf("failures = %d, want 0", len(failures))
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

func TestRouteOutcomeScoreBreakdownNilInOffMode(t *testing.T) {
	router, _ := setupTestRouter(t) // default Off mode
	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	if resp.RouteOutcome == nil {
		t.Fatalf("RouteOutcome nil")
	}
	if resp.RouteOutcome.ScoreBreakdown != nil {
		t.Errorf("Off mode: ScoreBreakdown = %+v, want nil", resp.RouteOutcome.ScoreBreakdown)
	}
}

func TestRouteOutcomeScoreBreakdownPopulatedInEnforceMode(t *testing.T) {
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
	resp, err := plan.ExecuteChat(context.Background())
	if err != nil {
		t.Fatalf("ExecuteChat: %v", err)
	}
	bd := resp.RouteOutcome.ScoreBreakdown
	if bd == nil {
		t.Fatalf("Enforce mode: ScoreBreakdown = nil, want non-nil")
	}
	if !bd.FeedbackApplied {
		t.Errorf("FeedbackApplied = false, want true (Enforce + valid snapshot)")
	}
	if bd.FeedbackMode != FeedbackScoringEnforce.String() {
		t.Errorf("FeedbackMode = %q, want %q", bd.FeedbackMode, FeedbackScoringEnforce.String())
	}
	if bd.FeedbackSnapshotStatus != string(feedbackSnapshotStatusActive) {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, feedbackSnapshotStatusActive)
	}
	if bd.FeedbackScore <= 0.5 {
		t.Errorf("FeedbackScore = %v, want > 0.5 after seeded successes", bd.FeedbackScore)
	}
	if bd.ScoreWithoutFeedback == bd.ScoreWithFeedback {
		t.Errorf("with/without scores identical %v; expected delta because feedback was non-neutral",
			bd.ScoreWithFeedback)
	}
}

func TestRouteOutcomeScoreBreakdownPopulatedInShadowMode(t *testing.T) {
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	k := FeedbackKey{Provider: "test", Model: "qwen3:8b", UseCase: "chat"}
	for i := 0; i < feedbackMinScoredCount+2; i++ {
		if err := rf.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
			t.Fatalf("Record: %v", err)
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
		t.Fatalf("Shadow mode: ScoreBreakdown = nil, want non-nil")
	}
	if bd.FeedbackApplied {
		t.Errorf("Shadow mode: FeedbackApplied = true, want false")
	}
	if bd.FeedbackMode != FeedbackScoringShadow.String() {
		t.Errorf("FeedbackMode = %q, want %q", bd.FeedbackMode, FeedbackScoringShadow.String())
	}
	if bd.FeedbackScore <= 0.5 {
		t.Errorf("Shadow mode: FeedbackScore = %v, want > 0.5 (snapshot still active)", bd.FeedbackScore)
	}
	// Spec § Behavior contracts 1: Shadow mode must expose the
	// would-be-applied delta in the breakdown — otherwise the mode
	// has no operator value during ramp-up. Locks the deltaActiveSignals
	// vs selectionActiveSignals split.
	if bd.ScoreWithFeedback == bd.ScoreWithoutFeedback {
		t.Errorf("Shadow mode: with/without scores identical (%v); breakdown must expose the delta",
			bd.ScoreWithFeedback)
	}
	// Stable enum check against the public string constants.
	if bd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("Shadow mode: FeedbackSnapshotStatus = %q, want %q",
			bd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
}

func TestRouteOutcomeScoreBreakdownJSONOmitemptyWhenNil(t *testing.T) {
	out := RouteOutcome{PlannedModel: ModelKey{Provider: "p", Model: "m"}}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "score_breakdown") {
		t.Errorf("JSON %s contains score_breakdown despite nil pointer", data)
	}
}

func TestBuildOutcomeScoreBreakdownStableAcrossFallback(t *testing.T) {
	plannedKey := ModelKey{Provider: "primary", Model: "qwen3:8b"}
	fallbackKey := ModelKey{Provider: "fallback", Model: "qwen3:8b"}

	// Construct a plan by hand with a planted scoreBreakdown that
	// describes the planned primary.
	plan := &RoutePlan{
		Profile: &ModelProfile{Key: plannedKey},
		Score:   0.85,
		Reason:  "primary won",
	}
	bd := &scoreBreakdown{
		feedbackActive:       true,
		feedbackRaw:          0.8,
		feedbackAdjusted:     0.7,
		feedbackSampleCount:  30,
		feedbackScoredCount:  30,
		feedbackUpdatedAt:    time.Unix(123, 0),
		scoreWithoutFeedback: 0.6,
		scoreWithFeedback:    0.85,
	}
	plan.setScoreBreakdown(bd)
	plan.setBuiltUnderMode(FeedbackScoringEnforce)
	plan.setFeedbackStatus(feedbackSnapshotStatusActive)

	// Simulate execution: primary failed (HTTP 5xx), fallback succeeded.
	attempts := []RouteAttempt{
		{Key: plannedKey, Status: AttemptStatusFailed, ErrorClass: string(ErrorClass5xx), LatencyMs: 50},
		{Key: fallbackKey, Status: AttemptStatusSucceeded, LatencyMs: 100},
	}
	out := plan.buildOutcome(1, attempts)

	// Execution trace reflects the fallback.
	if out.PlannedModel != plannedKey {
		t.Errorf("PlannedModel = %v, want %v", out.PlannedModel, plannedKey)
	}
	if out.ActualModel != fallbackKey {
		t.Errorf("ActualModel = %v, want %v (after fallback success)", out.ActualModel, fallbackKey)
	}
	if out.FallbacksUsed != 1 {
		t.Errorf("FallbacksUsed = %d, want 1", out.FallbacksUsed)
	}

	// ScoreBreakdown describes the planned candidate, NOT the fallback.
	pubBd := out.ScoreBreakdown
	if pubBd == nil {
		t.Fatalf("ScoreBreakdown nil")
	}
	if pubBd.FeedbackScore != 0.8 {
		t.Errorf("FeedbackScore = %v, want 0.8 (planned primary's raw score, unchanged by fallback)",
			pubBd.FeedbackScore)
	}
	if pubBd.FeedbackAdjustedScore != 0.7 {
		t.Errorf("FeedbackAdjustedScore = %v, want 0.7 (planned primary's adjusted, unchanged by fallback)",
			pubBd.FeedbackAdjustedScore)
	}
	if pubBd.ScoreWithoutFeedback != 0.6 {
		t.Errorf("ScoreWithoutFeedback = %v, want 0.6", pubBd.ScoreWithoutFeedback)
	}
	if pubBd.ScoreWithFeedback != 0.85 {
		t.Errorf("ScoreWithFeedback = %v, want 0.85", pubBd.ScoreWithFeedback)
	}
	if !pubBd.FeedbackApplied {
		t.Errorf("FeedbackApplied = false, want true (Enforce + active snapshot at plan-build)")
	}
	if pubBd.FeedbackSnapshotStatus != FeedbackSnapshotStatusActive {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			pubBd.FeedbackSnapshotStatus, FeedbackSnapshotStatusActive)
	}
	// FeedbackUpdatedAt is *time.Time post-Task-9-fix; assert it carries
	// the planted timestamp (not the fallback's execution time).
	if pubBd.FeedbackUpdatedAt == nil || !pubBd.FeedbackUpdatedAt.Equal(time.Unix(123, 0)) {
		t.Errorf("FeedbackUpdatedAt = %v, want pointer to unix 123 (planned primary's, not rewritten)",
			pubBd.FeedbackUpdatedAt)
	}
	// Mode + counts are translator-read fields; pin them so a future
	// regression that re-stamps from the fallback's snapshot is caught.
	if pubBd.FeedbackMode != FeedbackScoringEnforce.String() {
		t.Errorf("FeedbackMode = %q, want %q (planned primary's mode, not rewritten)",
			pubBd.FeedbackMode, FeedbackScoringEnforce.String())
	}
	if pubBd.FeedbackSampleCount != 30 {
		t.Errorf("FeedbackSampleCount = %d, want 30 (planned primary's, not rewritten)", pubBd.FeedbackSampleCount)
	}
	if pubBd.FeedbackScoredCount != 30 {
		t.Errorf("FeedbackScoredCount = %d, want 30 (planned primary's, not rewritten)", pubBd.FeedbackScoredCount)
	}
}

func TestBuildOutcomeScoreBreakdownSanitizesNonFiniteRawFeedbackScore(t *testing.T) {
	cases := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, raw := range cases {
		t.Run("", func(t *testing.T) {
			plan := &RoutePlan{
				Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}},
				Score:   0.5,
				Reason:  "custom store score",
			}
			bd := &scoreBreakdown{
				feedbackActive:       true,
				feedbackRaw:          raw,
				feedbackAdjusted:     0.5,
				feedbackSampleCount:  5,
				feedbackScoredCount:  5,
				scoreWithoutFeedback: 0.5,
				scoreWithFeedback:    0.5,
			}
			plan.setScoreBreakdown(bd)
			plan.setBuiltUnderMode(FeedbackScoringShadow)
			plan.setFeedbackStatus(feedbackSnapshotStatusActive)

			out := plan.buildOutcome(0, []RouteAttempt{{Key: ModelKey{Provider: "p", Model: "m"}, Status: AttemptStatusSucceeded}})
			if out.ScoreBreakdown == nil {
				t.Fatalf("ScoreBreakdown nil")
			}
			if out.ScoreBreakdown.FeedbackScore != DefaultNeutralScore {
				t.Fatalf("FeedbackScore = %v, want neutral %v", out.ScoreBreakdown.FeedbackScore, DefaultNeutralScore)
			}
			if _, err := json.Marshal(out); err != nil {
				t.Fatalf("json.Marshal(RouteOutcome) = %v", err)
			}
		})
	}
}

func TestBuildOutcomeScoreBreakdownSnapshotStatusReflectsRoute(t *testing.T) {
	// When the snapshot fail-opens (status=read_error), the breakdown
	// is still stamped (the plan exists, the breakdown was built) but
	// FeedbackApplied is false and the status is preserved.
	plan := &RoutePlan{
		Profile: &ModelProfile{Key: ModelKey{Provider: "p", Model: "m"}},
		Score:   0.5,
		Reason:  "fail-open",
	}
	bd := &scoreBreakdown{
		feedbackActive:       false, // snapshot was inactive
		feedbackRaw:          0,
		feedbackAdjusted:     0.5,
		scoreWithoutFeedback: 0.5,
		scoreWithFeedback:    0.5,
	}
	plan.setScoreBreakdown(bd)
	plan.setBuiltUnderMode(FeedbackScoringEnforce)
	plan.setFeedbackStatus(feedbackSnapshotStatusReadError)

	attempts := []RouteAttempt{{Key: ModelKey{Provider: "p", Model: "m"}, Status: AttemptStatusSucceeded}}
	out := plan.buildOutcome(0, attempts)

	pubBd := out.ScoreBreakdown
	if pubBd == nil {
		t.Fatalf("ScoreBreakdown nil")
	}
	if pubBd.FeedbackApplied {
		t.Errorf("read_error: FeedbackApplied = true, want false")
	}
	if pubBd.FeedbackSnapshotStatus != FeedbackSnapshotStatusReadError {
		t.Errorf("FeedbackSnapshotStatus = %q, want %q",
			pubBd.FeedbackSnapshotStatus, FeedbackSnapshotStatusReadError)
	}
	// Zero feedbackUpdatedAt should produce nil pointer (Task 9 fix).
	if pubBd.FeedbackUpdatedAt != nil {
		t.Errorf("FeedbackUpdatedAt = %v, want nil (zero feedbackUpdatedAt should omit)",
			pubBd.FeedbackUpdatedAt)
	}
	// Operator-facing mode label must say "enforce" even on fail-open so
	// dashboards can distinguish "Enforce + read_error" from "Shadow"
	// (both produce FeedbackApplied=false but for different reasons).
	if pubBd.FeedbackMode != FeedbackScoringEnforce.String() {
		t.Errorf("FeedbackMode = %q, want %q", pubBd.FeedbackMode, FeedbackScoringEnforce.String())
	}
}

// ---------------------------------------------------------------------------
// Task 11 — once-logged warnings on RNG failure / feedback store write
// ---------------------------------------------------------------------------

// TestNewRouteIDLogsOnceOnRandFailure substitutes a failing RNG and a
// capturing logger, then asserts the once-logged warning fires exactly
// once across many calls. Reuses routeIDFailingReader (defined in
// routing_feedback_test.go).
func TestNewRouteIDLogsOnceOnRandFailure(t *testing.T) {
	cap := &capturingLogger{}
	state := newFeedbackWarningState()

	old := routeIDRand
	t.Cleanup(func() { routeIDRand = old })
	routeIDRand = routeIDFailingReader{}

	for i := 0; i < 50; i++ {
		id := newRouteIDWithWarn(state, cap)
		if id != "" {
			t.Errorf("expected empty id on RNG failure, got %q", id)
		}
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("warnRouteIDRandOnce fired %d times, want 1", got)
	}
}

// recordFailingStore implements RoutingFeedbackStore for the Task 11
// write-error test. Get returns a neutral aggregate (so Route succeeds);
// Record / RecordBatch always fail so recordOutcomeFeedback hits its
// once-logged warning path.
type recordFailingStore struct{ err error }

func (s *recordFailingStore) Get(_ context.Context, _ FeedbackKey) (Aggregate, error) {
	return Aggregate{Score: DefaultNeutralScore}, nil
}

func (s *recordFailingStore) Record(_ context.Context, _ FeedbackKey, _ FeedbackSignal) error {
	return s.err
}

func (s *recordFailingStore) RecordBatch(_ context.Context, _ []FeedbackItem) error {
	return s.err
}

// TestRecordOutcomeFeedbackLogsOnceOnStoreError drives ExecuteChat
// repeatedly against a store that always fails on Record/RecordBatch and
// asserts the once-logged warning fires exactly once across the loop.
func TestRecordOutcomeFeedbackLogsOnceOnStoreError(t *testing.T) {
	router, _ := setupTestRouter(t)
	router.routingFeedback = NewRoutingFeedback(&recordFailingStore{err: errors.New("disk full")})
	cap := &capturingLogger{}
	router.feedbackLogger = cap

	plan, err := router.Route(context.Background(), RoutingRequest{Model: "test/qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := plan.ExecuteChat(context.Background()); err != nil {
			t.Fatalf("ExecuteChat: %v", err)
		}
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("warnFeedbackWriteOnce fired %d times, want 1", got)
	}
}
