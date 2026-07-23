package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeChainProvider implements Provider just enough to exercise routeChain.
// Generation methods return fixed responses; everything else is a no-op.
type fakeChainProvider struct {
	name   string
	models []string
}

func (f *fakeChainProvider) Name() string { return f.name }
func (f *fakeChainProvider) Capabilities() Capability {
	return CapChat | CapGenerate | CapEmbed | CapStream
}
func (f *fakeChainProvider) Health(ctx context.Context) error { return nil }
func (f *fakeChainProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	models := make([]ModelInfo, 0, len(f.models))
	for _, name := range f.models {
		models = append(models, ModelInfo{Name: name})
	}
	return models, nil
}
func (f *fakeChainProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{Provider: f.name, Model: req.Model, Content: "ok", Done: true}, nil
}
func (f *fakeChainProvider) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	return fn(ChatResponse{Provider: f.name, Model: req.Model, Content: "ok", Done: true})
}
func (f *fakeChainProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	return &GenerateResponse{Provider: f.name, Model: req.Model, Response: "ok", Done: true}, nil
}
func (f *fakeChainProvider) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	return fn(GenerateResponse{Provider: f.name, Model: req.Model, Response: "ok", Done: true})
}
func (f *fakeChainProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	return &EmbedResponse{Provider: f.name, Model: req.Model, Embeddings: [][]float64{{1}}}, nil
}

type keyedChainFeedbackStore struct {
	aggregates map[FeedbackKey]Aggregate
	errors     map[FeedbackKey]error
}

func (s *keyedChainFeedbackStore) Get(_ context.Context, key FeedbackKey) (Aggregate, error) {
	if err := s.errors[key]; err != nil {
		return Aggregate{}, err
	}
	if agg, ok := s.aggregates[key]; ok {
		return agg, nil
	}
	return Aggregate{Score: DefaultNeutralScore}, nil
}

func (s *keyedChainFeedbackStore) Record(_ context.Context, _ FeedbackKey, _ FeedbackSignal) error {
	return nil
}

func (s *keyedChainFeedbackStore) RecordBatch(_ context.Context, _ []FeedbackItem) error {
	return nil
}

// newChainTestRegistry builds a ModelRegistry pre-seeded with profiles
// matching the given (provider, model) pairs.
func newChainTestRegistry(t *testing.T, pairs ...ModelKey) (*ModelRegistry, *Registry) {
	t.Helper()
	provReg := NewRegistry()
	byProvider := map[string][]string{}
	for _, k := range pairs {
		byProvider[k.Provider] = append(byProvider[k.Provider], k.Model)
	}
	for providerName, models := range byProvider {
		p := &fakeChainProvider{name: providerName, models: models}
		if err := provReg.Register(p); err != nil {
			t.Fatalf("register provider %q: %v", providerName, err)
		}
	}
	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("new model registry: %v", err)
	}
	// Pre-seed cache so Lookup does not hit /api/show.
	for _, k := range pairs {
		profile := &ModelProfile{
			Key:           k,
			Caps:          CapChat | CapGenerate | CapEmbed | CapStream,
			Quality:       TierGood,
			Speed:         TierGood,
			ContextWindow: 8192,
		}
		seedChainProfile(t, mr, profile)
	}
	for providerName := range byProvider {
		if err := provReg.RefreshModels(context.Background(), providerName); err != nil {
			t.Fatalf("refresh models %q: %v", providerName, err)
		}
	}
	return mr, provReg
}

func seedChainProfile(t *testing.T, mr *ModelRegistry, profile *ModelProfile) {
	t.Helper()
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if mr.profiles == nil {
		mr.profiles = make(map[ModelKey]*ModelProfile)
	}
	mr.profiles[profile.Key] = profile
}

func cleanupRouter(t *testing.T, r *Router) {
	t.Helper()
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close router: %v", err)
		}
	})
}

func TestRouter_routeChain_SingleStepHappyPath(t *testing.T) {
	mr, pr := newChainTestRegistry(t, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	r := NewRouter(mr, pr)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/qwen3:8b"},
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama" || plan.Profile.Key.Model != "qwen3:8b" {
		t.Errorf("plan.Profile.Key = %v, want ollama/qwen3:8b", plan.Profile.Key)
	}
	if got := len(plan.Fallbacks); got != 0 {
		t.Errorf("Fallbacks length = %d, want 0", got)
	}
}

func TestRouter_routeChain_MultiStepOrdering(t *testing.T) {
	mr, pr := newChainTestRegistry(t,
		ModelKey{Provider: "ollama", Model: "primary:8b"},
		ModelKey{Provider: "ollama", Model: "fallback1:8b"},
		ModelKey{Provider: "ollama", Model: "fallback2:8b"},
	)
	r := NewRouter(mr, pr)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat,
		PreferredChain: []string{
			"ollama/primary:8b",
			"ollama/fallback1:8b",
			"ollama/fallback2:8b",
		},
		StrictChain: true, // skip Recommend tail for clarity
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Model != "primary:8b" {
		t.Errorf("primary = %q, want primary:8b", plan.Profile.Key.Model)
	}
	if len(plan.Fallbacks) != 2 {
		t.Fatalf("fallback count = %d, want 2", len(plan.Fallbacks))
	}
	if plan.Fallbacks[0].Profile.Key.Model != "fallback1:8b" {
		t.Errorf("fallbacks[0] = %q, want fallback1:8b", plan.Fallbacks[0].Profile.Key.Model)
	}
	if plan.Fallbacks[1].Profile.Key.Model != "fallback2:8b" {
		t.Errorf("fallbacks[1] = %q, want fallback2:8b", plan.Fallbacks[1].Profile.Key.Model)
	}
}

func TestRouter_routeChain_WithinStepScoring(t *testing.T) {
	// Two providers, both serving the same unqualified model name.
	// Provider "fast" has Speed=TierBest; provider "slow" has Speed=TierBasic.
	// Expectation: "fast" wins the within-step ordering.
	mr, provReg := newChainTestRegistry(t,
		ModelKey{Provider: "fast", Model: "shared:8b"},
		ModelKey{Provider: "slow", Model: "shared:8b"},
	)
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "fast", Model: "shared:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierBest, ContextWindow: 8192,
	})
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "slow", Model: "shared:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierBasic, ContextWindow: 8192,
	})

	r := NewRouter(mr, provReg)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"shared:8b"}, // unqualified — resolves to both
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "fast" {
		t.Errorf("primary provider = %q, want fast", plan.Profile.Key.Provider)
	}
	if len(plan.Fallbacks) != 1 || plan.Fallbacks[0].Profile.Key.Provider != "slow" {
		t.Errorf("fallback should be slow/shared:8b, got %v", plan.Fallbacks)
	}
}

func TestRouter_routeChain_LaterFeedbackReadErrorRescoresEarlierSteps(t *testing.T) {
	mr, provReg := newChainTestRegistry(t,
		ModelKey{Provider: "fast", Model: "shared:8b"},
		ModelKey{Provider: "slow", Model: "shared:8b"},
		ModelKey{Provider: "failer", Model: "tail:8b"},
	)
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "fast", Model: "shared:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierBest, ContextWindow: 8192,
	})
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "slow", Model: "shared:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierBasic, ContextWindow: 8192,
	})
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "failer", Model: "tail:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierGood, ContextWindow: 8192,
	})

	store := &keyedChainFeedbackStore{
		aggregates: map[FeedbackKey]Aggregate{
			{Provider: "fast", Model: "shared:8b", UseCase: "chat"}: {
				Score: 0, SampleCount: 100, ScoredCount: 100,
			},
			{Provider: "slow", Model: "shared:8b", UseCase: "chat"}: {
				Score: 1, SampleCount: 100, ScoredCount: 100,
			},
		},
		errors: map[FeedbackKey]error{
			{Provider: "failer", Model: "tail:8b", UseCase: "chat"}: errors.New("disk full"),
		},
	}

	r := NewRouter(mr, provReg,
		WithRoutingFeedback(NewRoutingFeedback(store)),
		WithFeedbackScoringMode(FeedbackScoringEnforce),
		WithWeightOverrides(map[string]*WeightProfile{
			"chat": {Feedback: 100, Speed: 1},
		}),
	)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"shared:8b", "failer/tail:8b"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "fast" {
		t.Fatalf("primary provider = %q, want fast (later read_error must neutralize earlier feedback)",
			plan.Profile.Key.Provider)
	}
	if plan.feedbackStatus != feedbackSnapshotStatusReadError {
		t.Fatalf("feedbackStatus = %q, want %q", plan.feedbackStatus, feedbackSnapshotStatusReadError)
	}
	if plan.scoreBreakdown == nil {
		t.Fatalf("scoreBreakdown nil, want read_error-stamped breakdown")
	}
	if plan.scoreBreakdown.feedbackActive {
		t.Errorf("feedbackActive = true after read_error; want false")
	}
	if plan.Score != plan.scoreBreakdown.scoreWithoutFeedback {
		t.Errorf("plan.Score = %v, scoreWithoutFeedback = %v; fail-open selection used stale feedback",
			plan.Score, plan.scoreBreakdown.scoreWithoutFeedback)
	}
}

func TestRouter_routeChain_RecommendTailFires(t *testing.T) {
	// Chain entry is budget-rejected (ContextWindow: 1); tail should pick the
	// only viable other model. Do not open the provider breaker, since that
	// would also gate the tail candidate.
	mr, pr := newChainTestRegistry(t,
		ModelKey{Provider: "ollama", Model: "broken:8b"},
		ModelKey{Provider: "ollama", Model: "alternate:8b"},
	)

	r := NewRouter(mr, pr)
	cleanupRouter(t, r)

	// Re-seed the chain entry with ContextWindow: 1 to force budget rejection.
	seedChainProfile(t, mr, &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "broken:8b"},
		Caps:          CapChat,
		Quality:       TierGood,
		Speed:         TierGood,
		ContextWindow: 1,
	})

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/broken:8b"},
		StrictChain:    false, // allow tail
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Model != "alternate:8b" {
		t.Errorf("primary = %q, want alternate:8b (Recommend tail)", plan.Profile.Key.Model)
	}
}

func TestRouter_routeChain_StrictChainSuppressesTail(t *testing.T) {
	mr, pr := newChainTestRegistry(t,
		ModelKey{Provider: "ollama", Model: "broken:8b"},
		ModelKey{Provider: "ollama", Model: "alternate:8b"},
	)
	r := NewRouter(mr, pr)
	cleanupRouter(t, r)

	seedChainProfile(t, mr, &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "broken:8b"},
		Caps:          CapChat,
		Quality:       TierGood,
		Speed:         TierGood,
		ContextWindow: 1,
	})

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/broken:8b"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	_, err := r.Route(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when StrictChain=true and chain exhausted")
	}
	if !errors.Is(err, ErrBudgetExceeded) && !errors.Is(err, ErrNoViableCandidate) && !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("err = %v, want ErrBudgetExceeded, ErrBudgetAdaptationRequired, or ErrNoViableCandidate", err)
	}
}

func TestRouter_routeChain_SuppressesStickyEvenWithAffinityKey(t *testing.T) {
	mr, pr := newChainTestRegistry(t,
		ModelKey{Provider: "ollama", Model: "primary:8b"},
		ModelKey{Provider: "ollama", Model: "fallback:8b"},
	)
	r := NewRouter(mr, pr)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/primary:8b", "ollama/fallback:8b"},
		StrictChain:    true,
		AffinityKey:    "anything-non-empty",
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}

	// Pre-seed sticky cache with fallback as the incumbent for the actual
	// key the request would compute. If routeChain ever consulted sticky,
	// it would reorder to put fallback first; we assert it does not.
	now := time.Now()
	r.sticky.put(&routeSticky{
		key:         StickyKey(req),
		providerKey: ModelKey{Provider: "ollama", Model: "fallback:8b"},
		score:       100,
		reason:      "test",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Hour),
	})

	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Model != "primary:8b" {
		t.Fatalf("primary = %q, want primary:8b — sticky must not reorder chain",
			plan.Profile.Key.Model)
	}
	if plan.WasSticky() {
		t.Error("plan.WasSticky() = true, want false for chain routes")
	}
}

// flakyProvider returns 5xx HTTPStatusError for any Models() call,
// which propagates through Lookup as IsInfrastructureError == true.
type flakyProvider struct {
	fakeChainProvider
}

func (f *flakyProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	return nil, &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
}

func TestRouter_routeChain_LookupFailureRecordsBreaker(t *testing.T) {
	provReg := NewRegistry()
	if err := provReg.Register(&flakyProvider{fakeChainProvider: fakeChainProvider{name: "flaky"}}); err != nil {
		t.Fatalf("register flaky: %v", err)
	}
	if err := provReg.Register(&fakeChainProvider{name: "stable"}); err != nil {
		t.Fatalf("register stable: %v", err)
	}
	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("model registry: %v", err)
	}
	// Pre-seed only the stable profile so flaky's Lookup is forced to fall
	// through to the runtime query (which 503s).
	seedChainProfile(t, mr, &ModelProfile{
		Key: ModelKey{Provider: "stable", Model: "m:8b"}, Caps: CapChat,
		Quality: TierGood, Speed: TierGood, ContextWindow: 8192,
	})

	r := NewRouter(mr, provReg)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"flaky/missing:8b", "stable/m:8b"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v (chain should fall through to stable)", err)
	}
	if plan.Profile.Key.Provider != "stable" {
		t.Errorf("primary provider = %q, want stable", plan.Profile.Key.Provider)
	}

	// Breaker for "flaky" should have recorded the lookup failure.
	info, ok := r.BreakerInfo("flaky")
	if !ok {
		t.Fatal("expected breaker for \"flaky\" to exist after lookup failure")
	}
	if info.Failures == 0 {
		t.Errorf("flaky breaker failures = %d, want >= 1", info.Failures)
	}
}

func TestRouter_routeChain_AllBudgetRejected(t *testing.T) {
	mr, pr := newChainTestRegistry(t,
		ModelKey{Provider: "ollama", Model: "small:8b"},
	)
	r := NewRouter(mr, pr)
	cleanupRouter(t, r)
	seedChainProfile(t, mr, &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "small:8b"},
		Caps:          CapChat,
		Quality:       TierGood,
		Speed:         TierGood,
		ContextWindow: 1,
	})

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/small:8b"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	_, err := r.Route(context.Background(), req)
	// Sole truncatable candidate becomes degraded; otherwise classified
	// as ErrBudgetExceeded. Both are acceptable per the design.
	if !errors.Is(err, ErrBudgetExceeded) && !errors.Is(err, ErrBudgetAdaptationRequired) {
		t.Errorf("err = %v, want ErrBudgetExceeded or ErrBudgetAdaptationRequired", err)
	}
}

func TestRouter_routeChain_AllLookupsFailedWrapsErrors(t *testing.T) {
	provReg := NewRegistry()
	if err := provReg.Register(&flakyProvider{fakeChainProvider: fakeChainProvider{name: "flaky"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r := NewRouter(mr, provReg)
	cleanupRouter(t, r)

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"flaky/a:8b", "flaky/b:8b"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	_, err = r.Route(context.Background(), req)
	if !errors.Is(err, ErrNoViableCandidate) {
		t.Errorf("err = %v, want wrapped ErrNoViableCandidate", err)
	}
	// Diagnostics should mention both selectors.
	msg := err.Error()
	if !strings.Contains(msg, "flaky/a:8b") || !strings.Contains(msg, "flaky/b:8b") {
		t.Errorf("error message %q should reference both failed selectors", msg)
	}
}
