package fingerprint

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Mock Store ---

type mockStore struct {
	mu           sync.Mutex
	profiles     map[string]*Profile
	failures     map[string]*FailureInfo
	needsResult  map[string]bool
	saveCalled   int
	saveFailed   int
	saveErr      error
}

func newMockStore() *mockStore {
	return &mockStore{
		profiles:    make(map[string]*Profile),
		failures:    make(map[string]*FailureInfo),
		needsResult: make(map[string]bool),
	}
}

func (s *mockStore) key(backendID, modelName string) string {
	return backendID + "\x00" + modelName
}

func (s *mockStore) Get(_ context.Context, backendID, modelName string) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[s.key(backendID, modelName)]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

func (s *mockStore) GetFailure(_ context.Context, backendID, modelName string) (*FailureInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.failures[s.key(backendID, modelName)]
	if !ok {
		return nil, ErrNotFound
	}
	return f, nil
}

func (s *mockStore) Save(_ context.Context, profile Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalled++
	if s.saveErr != nil {
		return s.saveErr
	}
	p := profile // copy
	s.profiles[s.key(profile.BackendID, profile.ModelName)] = &p
	return nil
}

func (s *mockStore) NeedsFingerprint(_ context.Context, backendID, modelName, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(backendID, modelName)
	needs, ok := s.needsResult[k]
	if ok {
		return needs, nil
	}
	// Default: needs fingerprint if no profile exists.
	_, exists := s.profiles[k]
	return !exists, nil
}

func (s *mockStore) SaveFailure(_ context.Context, backendID, modelName, modelDigest, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveFailed++
	k := s.key(backendID, modelName)
	existing, ok := s.failures[k]
	count := 1
	if ok {
		count = existing.AttemptCount + 1
	}
	s.failures[k] = &FailureInfo{
		ModelDigest:  modelDigest,
		LastError:    errMsg,
		AttemptedAt:  time.Now(),
		AttemptCount: count,
		RetryAfter:   time.Now().Add(time.Hour),
	}
	return nil
}

// --- Mock Prober ---

type mockProber struct {
	detectKindFn    func(ctx context.Context, model string) (*KindDetection, error)
	probeChatFn     func(ctx context.Context, model string, opts interface{}) (*ChatMetrics, error)
	probeEmbedFn    func(ctx context.Context, model string) (*EmbeddingMetrics, error)
	detectCalls     atomic.Int32
	probeChatCalls  atomic.Int32
	probeEmbedCalls atomic.Int32
}

func (m *mockProber) DetectKind(ctx context.Context, model string) (*KindDetection, error) {
	m.detectCalls.Add(1)
	if m.detectKindFn != nil {
		return m.detectKindFn(ctx, model)
	}
	return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
}

func (m *mockProber) ProbeChat(ctx context.Context, model string, opts interface{}) (*ChatMetrics, error) {
	m.probeChatCalls.Add(1)
	if m.probeChatFn != nil {
		return m.probeChatFn(ctx, model, opts)
	}
	return &ChatMetrics{TokensPerSecond: 20.0, PromptLatency: 200 * time.Millisecond}, nil
}

func (m *mockProber) ProbeEmbedding(ctx context.Context, model string) (*EmbeddingMetrics, error) {
	m.probeEmbedCalls.Add(1)
	if m.probeEmbedFn != nil {
		return m.probeEmbedFn(ctx, model)
	}
	return &EmbeddingMetrics{Dim: 768, Latency: 50 * time.Millisecond}, nil
}

// --- Tests ---

const (
	testBackend = "http://localhost:11434"
	testModel   = "qwen2.5:72b"
	testDigest  = "sha256:abc123"
)

func TestProfiler_EnsureProfile_Cached(t *testing.T) {
	store := newMockStore()
	store.profiles[store.key(testBackend, testModel)] = &Profile{
		BackendID:   testBackend,
		ModelName:   testModel,
		ModelDigest: testDigest,
		ModelKind:   ModelKindChat,
	}
	store.needsResult[store.key(testBackend, testModel)] = false

	prober := &mockProber{}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if profile.ModelName != testModel {
		t.Errorf("ModelName = %q, want %q", profile.ModelName, testModel)
	}
	if prober.detectCalls.Load() != 0 {
		t.Errorf("DetectKind called %d times, want 0 (cached)", prober.detectCalls.Load())
	}
}

func TestProfiler_EnsureProfile_Fresh_Chat(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindChat,
				Source:       "capabilities",
				Capabilities: []string{"completion"},
			}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if profile.ModelKind != ModelKindChat {
		t.Errorf("ModelKind = %q, want %q", profile.ModelKind, ModelKindChat)
	}
	if profile.GenerationTokensPerSecond <= 0 {
		t.Errorf("GenerationTokensPerSecond = %f, want > 0", profile.GenerationTokensPerSecond)
	}
	if store.saveCalled != 1 {
		t.Errorf("Save called %d times, want 1", store.saveCalled)
	}
}

func TestProfiler_EnsureProfile_Fresh_Embedding(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindEmbedding,
				Source:       "capabilities",
				Capabilities: []string{"embedding"},
			}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, "nomic-embed", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if profile.ModelKind != ModelKindEmbedding {
		t.Errorf("ModelKind = %q, want %q", profile.ModelKind, ModelKindEmbedding)
	}
	if profile.EmbeddingDim != 768 {
		t.Errorf("EmbeddingDim = %d, want 768", profile.EmbeddingDim)
	}
}

func TestProfiler_EnsureProfile_ProbeFails_SavesFailure(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
		},
		probeChatFn: func(_ context.Context, _ string, _ interface{}) (*ChatMetrics, error) {
			return nil, errors.New("connection refused")
		},
	}
	profiler := NewProfiler(store, prober)

	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err == nil {
		t.Fatal("expected error from failed probe")
	}
	if store.saveFailed != 1 {
		t.Errorf("SaveFailure called %d times, want 1", store.saveFailed)
	}
}

func TestProfiler_EnsureProfile_InBackoff(t *testing.T) {
	store := newMockStore()
	store.needsResult[store.key(testBackend, testModel)] = false
	store.failures[store.key(testBackend, testModel)] = &FailureInfo{
		ModelDigest:  testDigest,
		LastError:    "connection refused",
		RetryAfter:   time.Now().Add(time.Hour),
		AttemptCount: 1,
	}

	prober := &mockProber{}
	profiler := NewProfiler(store, prober)

	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err == nil {
		t.Fatal("expected BackoffError")
	}
	var backoffErr *BackoffError
	if !errors.As(err, &backoffErr) {
		t.Fatalf("expected *BackoffError, got %T: %v", err, err)
	}
}

func TestProfiler_EnsureProfile_VersionUpgrade(t *testing.T) {
	store := newMockStore()
	// Old profile with version 0.
	store.profiles[store.key(testBackend, testModel)] = &Profile{
		BackendID:      testBackend,
		ModelName:      testModel,
		ModelDigest:    testDigest,
		ModelKind:      ModelKindChat,
		ProfileVersion: 0,
	}
	// NeedsFingerprint returns true because version mismatch.
	store.needsResult[store.key(testBackend, testModel)] = true

	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if profile.ProfileVersion != CurrentProfileVersion {
		t.Errorf("ProfileVersion = %d, want %d", profile.ProfileVersion, CurrentProfileVersion)
	}
}

func TestProfiler_EnsureProfile_RaceCondition(t *testing.T) {
	// NeedsFingerprint returns false, Get returns ErrNotFound,
	// GetFailure returns ErrNotFound → falls through to profiling.
	store := newMockStore()
	store.needsResult[store.key(testBackend, testModel)] = false
	// No profile, no failure — race condition path.

	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if profile.ModelKind != ModelKindChat {
		t.Errorf("ModelKind = %q, want %q", profile.ModelKind, ModelKindChat)
	}
}

func TestProfiler_EnsureProfile_DetectKindFails(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return nil, errors.New("model not found")
		},
	}
	profiler := NewProfiler(store, prober)

	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err == nil {
		t.Fatal("expected error from DetectKind failure")
	}
	if store.saveFailed != 1 {
		t.Errorf("SaveFailure called %d times, want 1", store.saveFailed)
	}
}

func TestProfiler_EnsureProfile_BackoffError_ExtractableWithErrorsAs(t *testing.T) {
	store := newMockStore()
	store.needsResult[store.key(testBackend, testModel)] = false
	store.failures[store.key(testBackend, testModel)] = &FailureInfo{
		ModelDigest: testDigest,
		LastError:   "timeout",
		RetryAfter:  time.Now().Add(time.Hour),
	}

	profiler := NewProfiler(store, &mockProber{})

	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err == nil {
		t.Fatal("expected error")
	}
	var backoffErr *BackoffError
	if !errors.As(err, &backoffErr) {
		t.Fatalf("errors.As failed: got %T: %v", err, err)
	}
	if backoffErr.LastError != "timeout" {
		t.Errorf("LastError = %q, want %q", backoffErr.LastError, "timeout")
	}
}

func TestProfiler_EnsureProfile_DualCapability(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindChat,
				Source:       "capabilities",
				Capabilities: []string{"completion", "embedding"},
			}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, "dual-model", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if prober.probeChatCalls.Load() != 1 {
		t.Errorf("ProbeChat called %d times, want 1", prober.probeChatCalls.Load())
	}
	if prober.probeEmbedCalls.Load() != 1 {
		t.Errorf("ProbeEmbedding called %d times, want 1", prober.probeEmbedCalls.Load())
	}
	if profile.GenerationTokensPerSecond <= 0 {
		t.Error("expected chat metrics populated")
	}
	if profile.EmbeddingDim != 768 {
		t.Errorf("EmbeddingDim = %d, want 768", profile.EmbeddingDim)
	}
}

func TestProfiler_EnsureProfile_HeuristicSingleCapability(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:   ModelKindChat,
				Source: "heuristic",
			}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if prober.probeChatCalls.Load() != 1 {
		t.Errorf("ProbeChat called %d times, want 1", prober.probeChatCalls.Load())
	}
	if prober.probeEmbedCalls.Load() != 0 {
		t.Errorf("ProbeEmbedding called %d times, want 0 (heuristic = single)", prober.probeEmbedCalls.Load())
	}
}

func TestProfiler_EnsureProfile_PartialFailure(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindChat,
				Source:       "capabilities",
				Capabilities: []string{"completion", "embedding"},
			}, nil
		},
		probeEmbedFn: func(_ context.Context, _ string) (*EmbeddingMetrics, error) {
			return nil, errors.New("embedding not supported")
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, "partial-model", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v (partial should return profile, not error)", err)
	}
	// Chat metrics should be populated.
	if profile.GenerationTokensPerSecond <= 0 {
		t.Error("expected chat metrics populated in partial profile")
	}
	// Embedding sentinel values.
	if profile.EmbeddingCoherence != -1 {
		t.Errorf("EmbeddingCoherence = %f, want -1 (sentinel)", profile.EmbeddingCoherence)
	}
	// IncompleteCapabilities should record the failed capability.
	if len(profile.IncompleteCapabilities) != 1 || profile.IncompleteCapabilities[0] != "embedding" {
		t.Errorf("IncompleteCapabilities = %v, want [embedding]", profile.IncompleteCapabilities)
	}
	// SaveFailure should have been called for backoff.
	if store.saveFailed != 1 {
		t.Errorf("SaveFailure called %d times, want 1", store.saveFailed)
	}
}

func TestProfiler_EnsureProfile_IncompleteProfile_ActiveBackoff(t *testing.T) {
	store := newMockStore()
	k := store.key(testBackend, "partial-model")
	store.profiles[k] = &Profile{
		BackendID:              testBackend,
		ModelName:              "partial-model",
		ModelDigest:            testDigest,
		ModelKind:              ModelKindChat,
		IncompleteCapabilities: []string{"embedding"},
		GenerationTokensPerSecond: 20.0,
	}
	store.failures[k] = &FailureInfo{
		ModelDigest:  testDigest,
		LastError:    "embedding failed",
		RetryAfter:   time.Now().Add(time.Hour),
		AttemptCount: 1,
	}
	store.needsResult[k] = true // Version upgrade or digest change triggers re-check

	prober := &mockProber{}
	profiler := NewProfiler(store, prober)

	// Should return partial profile, NOT BackoffError.
	profile, err := profiler.EnsureProfile(context.Background(), testBackend, "partial-model", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v, want partial profile returned", err)
	}
	if profile.GenerationTokensPerSecond <= 0 {
		t.Error("expected chat metrics preserved from partial profile")
	}
}

func TestProfiler_EnsureProfile_IncompleteProfile_RetryAfterBackoff(t *testing.T) {
	store := newMockStore()
	k := store.key(testBackend, "partial-model")
	store.profiles[k] = &Profile{
		BackendID:              testBackend,
		ModelName:              "partial-model",
		ModelDigest:            testDigest,
		ModelKind:              ModelKindChat,
		IncompleteCapabilities: []string{"embedding"},
		GenerationTokensPerSecond: 20.0,
		PromptLatency:          200 * time.Millisecond,
	}
	store.failures[k] = &FailureInfo{
		ModelDigest:  testDigest,
		LastError:    "embedding failed",
		RetryAfter:   time.Now().Add(-time.Minute), // Expired backoff
		AttemptCount: 1,
	}
	store.needsResult[k] = true

	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindChat,
				Source:       "capabilities",
				Capabilities: []string{"completion", "embedding"},
			}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	profile, err := profiler.EnsureProfile(context.Background(), testBackend, "partial-model", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	// Only embedding probe should have run (chat already has metrics).
	if prober.probeChatCalls.Load() != 0 {
		t.Errorf("ProbeChat called %d times, want 0 (already has chat metrics)", prober.probeChatCalls.Load())
	}
	if prober.probeEmbedCalls.Load() != 1 {
		t.Errorf("ProbeEmbedding called %d times, want 1 (retry incomplete)", prober.probeEmbedCalls.Load())
	}
	// Verify existing chat metrics preserved.
	if profile.GenerationTokensPerSecond != 20.0 {
		t.Errorf("GenerationTokensPerSecond = %f, want 20.0 (preserved)", profile.GenerationTokensPerSecond)
	}
	// Verify embedding metrics now populated.
	if profile.EmbeddingDim != 768 {
		t.Errorf("EmbeddingDim = %d, want 768", profile.EmbeddingDim)
	}
}

func TestProfiler_EnsureProfile_StaleProfileDuringBackoff(t *testing.T) {
	store := newMockStore()
	k := store.key(testBackend, testModel)
	oldDigest := "sha256:old"
	store.profiles[k] = &Profile{
		BackendID:   testBackend,
		ModelName:   testModel,
		ModelDigest: oldDigest,
		ModelKind:   ModelKindChat,
	}
	store.failures[k] = &FailureInfo{
		ModelDigest:  testDigest, // New digest
		LastError:    "timeout",
		RetryAfter:   time.Now().Add(time.Hour),
		AttemptCount: 1,
	}
	store.needsResult[k] = false

	profiler := NewProfiler(store, &mockProber{})

	// Profile has old digest, request is for new digest → stale.
	// Backoff is active → should return BackoffError.
	_, err := profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
	if err == nil {
		t.Fatal("expected BackoffError for stale profile during backoff")
	}
	var backoffErr *BackoffError
	if !errors.As(err, &backoffErr) {
		t.Fatalf("expected *BackoffError, got %T: %v", err, err)
	}
}

func TestProfiler_EnsureProfile_Singleflight_SameDigest(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
		},
		probeChatFn: func(_ context.Context, _ string, _ interface{}) (*ChatMetrics, error) {
			time.Sleep(50 * time.Millisecond) // Simulate work
			return &ChatMetrics{TokensPerSecond: 20.0, PromptLatency: 200 * time.Millisecond}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	profiles := make([]*Profile, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			profiles[idx], errs[idx] = profiler.EnsureProfile(context.Background(), testBackend, testModel, testDigest)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d error: %v", i, err)
		}
	}
	// Singleflight should deduplicate — prober called once, not twice.
	if prober.detectCalls.Load() != 1 {
		t.Errorf("DetectKind called %d times, want 1 (singleflight)", prober.detectCalls.Load())
	}
}

func TestProfiler_EnsureProfile_Singleflight_DifferentDigest(t *testing.T) {
	store := newMockStore()
	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{Kind: ModelKindChat, Source: "capabilities", Capabilities: []string{"completion"}}, nil
		},
		probeChatFn: func(_ context.Context, _ string, _ interface{}) (*ChatMetrics, error) {
			time.Sleep(50 * time.Millisecond)
			return &ChatMetrics{TokensPerSecond: 20.0}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		profiler.EnsureProfile(context.Background(), testBackend, testModel, "sha256:digest1")
	}()
	go func() {
		defer wg.Done()
		profiler.EnsureProfile(context.Background(), testBackend, testModel, "sha256:digest2")
	}()
	wg.Wait()

	// Different digests → different singleflight keys → both run.
	if prober.detectCalls.Load() != 2 {
		t.Errorf("DetectKind called %d times, want 2 (different digests)", prober.detectCalls.Load())
	}
}

func TestProfiler_EnsureProfile_DualCapability_Order(t *testing.T) {
	store := newMockStore()
	var order []string
	var mu sync.Mutex

	prober := &mockProber{
		detectKindFn: func(_ context.Context, _ string) (*KindDetection, error) {
			return &KindDetection{
				Kind:         ModelKindChat,
				Source:       "capabilities",
				Capabilities: []string{"completion", "embedding"},
			}, nil
		},
		probeChatFn: func(_ context.Context, _ string, _ interface{}) (*ChatMetrics, error) {
			mu.Lock()
			order = append(order, "chat")
			mu.Unlock()
			return &ChatMetrics{TokensPerSecond: 20.0}, nil
		},
		probeEmbedFn: func(_ context.Context, _ string) (*EmbeddingMetrics, error) {
			mu.Lock()
			order = append(order, "embedding")
			mu.Unlock()
			return &EmbeddingMetrics{Dim: 768, Latency: 50 * time.Millisecond}, nil
		},
	}
	profiler := NewProfiler(store, prober)

	_, err := profiler.EnsureProfile(context.Background(), testBackend, "dual-model", testDigest)
	if err != nil {
		t.Fatalf("EnsureProfile() error: %v", err)
	}
	if len(order) != 2 || order[0] != "chat" || order[1] != "embedding" {
		t.Errorf("probe order = %v, want [chat embedding]", order)
	}
}
