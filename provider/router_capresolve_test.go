// provider/router_capresolve_test.go
//
// Task 7 (#219): route-time lazy tool_call resolution. Verifies that the
// Router probes unknown candidates via EnsureToolCallResolved in the chain
// path, the Recommend (empty-Model) path, and the recommend tail — with
// probe I/O strictly BEFORE feedback-snapshot reads, and the capability
// gate remaining the single rejection point.
//
// The disabled-resolver path (no prober/store wired) is covered by the
// pre-existing router suite, which runs entirely without cap resolution.
package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ---------------------------------------------------------------------------
// Ordering fakes
// ---------------------------------------------------------------------------

// rcOrderLog records probe/feedback events so tests can assert that probe
// I/O happens before feedback-snapshot reads.
type rcOrderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *rcOrderLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *rcOrderLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

// rcLoggingProber wraps fakeToolCallProber and records each probe.
type rcLoggingProber struct {
	*fakeToolCallProber
	log *rcOrderLog
}

func (p *rcLoggingProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.log.add("probe:" + model)
	return p.fakeToolCallProber.ProbeToolCall(ctx, model)
}

// rcLoggingFeedbackStore is a RoutingFeedbackStore that records reads.
type rcLoggingFeedbackStore struct {
	log *rcOrderLog
}

func (s *rcLoggingFeedbackStore) Get(_ context.Context, key FeedbackKey) (Aggregate, error) {
	s.log.add("feedback:" + key.Model)
	return Aggregate{}, nil
}

func (s *rcLoggingFeedbackStore) Record(context.Context, FeedbackKey, FeedbackSignal) error {
	return nil
}

func (s *rcLoggingFeedbackStore) RecordBatch(context.Context, []FeedbackItem) error {
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// newCapRouteRouter builds a Router over a single "llamacpp" mock provider
// whose models advertise chat+stream but NOT tool_call, with the cap-probe
// store and prober wired into the ModelRegistry.
func newCapRouteRouter(t *testing.T, models []string, store fingerprint.CapProbeStore, prober fingerprint.ModelProber, ropts ...RouterOption) *Router {
	t.Helper()

	infos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		infos = append(infos, ModelInfo{
			Name:          m,
			ContextWindow: 32768,
			Capabilities:  []string{"chat", "stream"},
		})
	}
	prov := &rtMockProvider{
		name:   "llamacpp",
		caps:   CapChat | CapStream,
		models: infos,
	}
	provReg := NewRegistry()
	if err := provReg.Register(prov); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), "llamacpp"); err != nil {
		t.Fatalf("RefreshModels failed: %v", err)
	}

	var mrOpts []ModelRegistryOption
	if store != nil {
		mrOpts = append(mrOpts, WithCapabilityProbeStore(store))
	}
	if prober != nil {
		mrOpts = append(mrOpts, WithCapabilityProber(capProberFactory(prober)))
	}
	modelReg, err := NewModelRegistry(provReg, nil, mrOpts...)
	if err != nil {
		t.Fatalf("NewModelRegistry failed: %v", err)
	}

	router := NewRouter(modelReg, provReg, ropts...)
	t.Cleanup(func() { _ = router.Close() })
	return router
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRouteChain_UnknownCandidateProbedBeforeGate(t *testing.T) {
	ctx := context.Background()
	log := &rcOrderLog{}
	store := newFakeCapProbeStore()
	inner := newFakeToolCallProber()
	inner.outcomes["mystery-byo"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober := &rcLoggingProber{fakeToolCallProber: inner, log: log}

	router := newCapRouteRouter(t, []string{"mystery-byo"}, store, prober,
		WithRoutingFeedback(NewRoutingFeedback(&rcLoggingFeedbackStore{log: log})),
		WithFeedbackScoringMode(FeedbackScoringShadow),
	)

	plan, err := router.Route(ctx, RoutingRequest{
		PreferredChain: []string{"llamacpp/mystery-byo"},
		StrictChain:    true,
		UseCase:        "chat",
		RequiredCaps:   CapChat | CapStream | CapToolCall,
	})
	if err != nil {
		t.Fatalf("Route() error: %v (probe-resolvable candidate must not be rejected)", err)
	}
	if plan.Profile.Key.Model != "mystery-byo" {
		t.Fatalf("winner = %q, want mystery-byo", plan.Profile.Key.Model)
	}
	if got := inner.callCount("mystery-byo"); got != 1 {
		t.Fatalf("prober calls = %d, want 1", got)
	}

	// Design lock: probe I/O happens BEFORE any feedback-snapshot read.
	events := log.list()
	probeIdx, feedbackIdx := -1, -1
	for i, e := range events {
		if probeIdx == -1 && strings.HasPrefix(e, "probe:") {
			probeIdx = i
		}
		if feedbackIdx == -1 && strings.HasPrefix(e, "feedback:") {
			feedbackIdx = i
		}
	}
	if probeIdx == -1 {
		t.Fatalf("no probe event recorded; events = %v", events)
	}
	if feedbackIdx == -1 {
		t.Fatalf("no feedback read recorded; events = %v", events)
	}
	if probeIdx > feedbackIdx {
		t.Fatalf("probe at %d AFTER feedback read at %d; events = %v", probeIdx, feedbackIdx, events)
	}
}

func TestRouteChain_ConfirmedNoSkippedWithoutProbe(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()

	// Pre-seed a valid "no" row keyed by the digestless fallback digest.
	now := time.Now()
	if err := store.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID:    "llamacpp",
		ModelName:    "mystery-byo",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeNo,
		ModelDigest:  "llamacpp/mystery-byo",
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveCapProbe failed: %v", err)
	}

	router := newCapRouteRouter(t, []string{"mystery-byo"}, store, prober)

	_, err := router.Route(ctx, RoutingRequest{
		PreferredChain: []string{"llamacpp/mystery-byo"},
		StrictChain:    true,
		UseCase:        "chat",
		RequiredCaps:   CapChat | CapStream | CapToolCall,
	})
	if !errors.Is(err, ErrNoViableCandidate) {
		t.Fatalf("Route() error = %v, want ErrNoViableCandidate", err)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0 (confirmed-no must not re-probe)", got)
	}
}

func TestResolveCandidates_RecommendStripsThenFilters(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-a"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober.outcomes["mystery-b"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}

	router := newCapRouteRouter(t, []string{"mystery-a", "mystery-b"}, store, prober)

	candidates, _, err := router.resolveCandidates(ctx, RoutingRequest{
		Model:        "",
		UseCase:      "chat",
		RequiredCaps: CapChat | CapStream | CapToolCall,
	})
	if err != nil {
		t.Fatalf("resolveCandidates() error: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Key.Model != "mystery-a" {
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			names = append(names, c.Key.Model)
		}
		t.Fatalf("candidates = %v, want exactly [mystery-a]", names)
	}
	// Both models were probed: proof that Recommend ran WITHOUT the
	// tool_call bit (the old code would have filtered both out before the
	// router ever saw them, and the prober would never run).
	if got := prober.callCount("mystery-a"); got != 1 {
		t.Fatalf("prober calls for mystery-a = %d, want 1", got)
	}
	if got := prober.callCount("mystery-b"); got != 1 {
		t.Fatalf("prober calls for mystery-b = %d, want 1", got)
	}
}

func TestRecommendTail_AppliesSameResolution(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-chain"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	prober.outcomes["mystery-a"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}

	router := newCapRouteRouter(t, []string{"mystery-chain", "mystery-a"}, store, prober)

	// Non-strict chain: the chain entry probes "no" and is gated out; the
	// recommend tail must apply the same strip-resolve-filter treatment so
	// mystery-a (probe-yes) can win.
	plan, err := router.Route(ctx, RoutingRequest{
		PreferredChain: []string{"llamacpp/mystery-chain"},
		StrictChain:    false,
		UseCase:        "chat",
		RequiredCaps:   CapChat | CapStream | CapToolCall,
	})
	if err != nil {
		t.Fatalf("Route() error: %v (tail must resolve probe-yes candidates)", err)
	}
	if plan.Profile.Key.Model != "mystery-a" {
		t.Fatalf("winner = %q, want mystery-a", plan.Profile.Key.Model)
	}
	if got := prober.callCount("mystery-a"); got != 1 {
		t.Fatalf("prober calls for mystery-a = %d, want 1", got)
	}
	// mystery-chain's "no" verdict was persisted by the chain-step probe;
	// the tail resolution must reuse the cached row, not probe again.
	if got := prober.callCount("mystery-chain"); got != 1 {
		t.Fatalf("prober calls for mystery-chain = %d, want 1", got)
	}
	for _, fb := range plan.Fallbacks {
		if fb.Profile.Key.Model == "mystery-chain" {
			t.Fatalf("probe-no candidate mystery-chain leaked into fallbacks")
		}
	}
}

// TestRoute_DirectRoutesResolved pins the EnsureToolCallResolved wiring on
// the three direct-route branches of resolveCandidates: qualified selector,
// provider-restricted unqualified, and LookupAny. Each would fail with
// ErrNoViableCandidate if its wiring line were reverted (the merged profile
// lacks tool_call until the probe resolves it).
func TestRoute_DirectRoutesResolved(t *testing.T) {
	cases := []struct {
		name string
		req  RoutingRequest
	}{
		{
			name: "qualified selector",
			req: RoutingRequest{
				Model:        "llamacpp/mystery-byo",
				UseCase:      "chat",
				RequiredCaps: CapChat | CapStream | CapToolCall,
			},
		},
		{
			name: "provider-restricted unqualified",
			req: RoutingRequest{
				Model:        "mystery-byo",
				Provider:     "llamacpp",
				UseCase:      "chat",
				RequiredCaps: CapChat | CapStream | CapToolCall,
			},
		},
		{
			name: "lookup-any unqualified",
			req: RoutingRequest{
				Model:        "mystery-byo",
				UseCase:      "chat",
				RequiredCaps: CapChat | CapStream | CapToolCall,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeCapProbeStore()
			prober := newFakeToolCallProber()
			prober.outcomes["mystery-byo"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
			router := newCapRouteRouter(t, []string{"mystery-byo"}, store, prober)

			plan, err := router.Route(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Route() error: %v (direct route must resolve probe-yes candidate)", err)
			}
			if plan.Profile.Key.Model != "mystery-byo" {
				t.Fatalf("winner = %q, want mystery-byo", plan.Profile.Key.Model)
			}
			if got := prober.callCount("mystery-byo"); got != 1 {
				t.Fatalf("prober calls = %d, want 1", got)
			}
		})
	}
}

// TestRoute_ProbeErrorSurfacedInDiagnostics pins the diagnostics surface:
// a transient probe failure (e.g. 401) must be readable from the route
// error so operators can distinguish it from "genuinely not tool-capable".
func TestRoute_ProbeErrorSurfacedInDiagnostics(t *testing.T) {
	newRouter := func(t *testing.T) (*Router, *fakeToolCallProber) {
		store := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		prober.errs["mystery-byo"] = errors.New("unexpected status 401 Unauthorized")
		return newCapRouteRouter(t, []string{"mystery-byo"}, store, prober), prober
	}

	t.Run("direct qualified route", func(t *testing.T) {
		router, _ := newRouter(t)
		_, err := router.Route(context.Background(), RoutingRequest{
			Model:        "llamacpp/mystery-byo",
			UseCase:      "chat",
			RequiredCaps: CapChat | CapStream | CapToolCall,
		})
		if !errors.Is(err, ErrNoViableCandidate) {
			t.Fatalf("Route() error = %v, want ErrNoViableCandidate", err)
		}
		if !strings.Contains(err.Error(), "resolve tool_call") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("error %q must contain the probe diagnostic (resolve tool_call ... 401)", err)
		}
	})

	t.Run("strict chain", func(t *testing.T) {
		router, _ := newRouter(t)
		_, err := router.Route(context.Background(), RoutingRequest{
			PreferredChain: []string{"llamacpp/mystery-byo"},
			StrictChain:    true,
			UseCase:        "chat",
			RequiredCaps:   CapChat | CapStream | CapToolCall,
		})
		if !errors.Is(err, ErrNoViableCandidate) {
			t.Fatalf("Route() error = %v, want ErrNoViableCandidate", err)
		}
		if !strings.Contains(err.Error(), "resolve tool_call") || !strings.Contains(err.Error(), "401") {
			t.Fatalf("chain exhaustion error %q must contain the probe diagnostic (resolve tool_call ... 401)", err)
		}
	})
}
