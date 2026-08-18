package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// recordingAdmitter — plan-level admission fake
// ---------------------------------------------------------------------------

// recordingAdmitter records acquire/release ordering per key and can
// force errors. Injected via plan.setAdmission — plan-level tests do not
// need a full Router.
type recordingAdmitter struct {
	mu         sync.Mutex
	events     []string // "acquire p/m", "release p/m"
	errFor     map[ModelKey]error
	ungoverned map[ModelKey]bool // default: everything governed
}

// acquireSlot mirrors the real gate's decision order: governance first
// (ungoverned keys get a silent no-op, no events), then forced errors,
// then the governed dead-context rejection.
func (ra *recordingAdmitter) acquireSlot(ctx context.Context, key ModelKey) (func(), error) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if ra.ungoverned[key] {
		return func() {}, nil
	}
	if err := ra.errFor[key]; err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ra.events = append(ra.events, "acquire "+key.String())
	var once sync.Once
	return func() {
		once.Do(func() {
			ra.mu.Lock()
			ra.events = append(ra.events, "release "+key.String())
			ra.mu.Unlock()
		})
	}, nil
}

func (ra *recordingAdmitter) getEvents() []string {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	out := make([]string, len(ra.events))
	copy(out, ra.events)
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// admissionFallbackPlan wires prov as primary and fbProv as the single
// fallback, with the recorder shared, mirroring the existing fallback
// fixtures in route_plan_test.go.
func admissionFallbackPlan(prov, fbProv *rpMockProvider, rec *rpMockRecorder) *RoutePlan {
	plan := newTestPlan(prov, rec)
	plan.Fallbacks = []RoutePlan{
		{
			Kind:     plan.Kind,
			Provider: fbProv,
			Model:    "test-model",
			Profile: &ModelProfile{
				Key:           ModelKey{Provider: fbProv.name, Model: "test-model"},
				Name:          "test-model",
				Family:        "test",
				Provider:      fbProv.name,
				ContextWindow: 32768,
			},
			Request: plan.Request,
			Score:   0.70,
			Budget:  BudgetResult{Decision: BudgetOK},
			Reason:  "fallback",
		},
	}
	return plan
}

func healthyMock(name string) *rpMockProvider {
	return &rpMockProvider{
		name:      name,
		caps:      CapChat | CapGenerate | CapEmbed,
		chatResp:  &ChatResponse{Model: "test-model", Content: "ok", Done: true},
		genResp:   &GenerateResponse{Model: "test-model", Response: "ok", Done: true},
		embedResp: &EmbedResponse{Model: "test-model", Embeddings: [][]float64{{0.1}}},
	}
}

func failingMock(name string) *rpMockProvider {
	infra := &HTTPStatusError{StatusCode: 503}
	return &rpMockProvider{
		name: name, caps: CapChat | CapGenerate | CapEmbed,
		chatErr: infra, genErr: infra, embedErr: infra,
	}
}

// executeKind runs one Execute method by name; the admission brackets must
// behave identically across all three non-streaming paths.
func executeKind(t *testing.T, kind string, plan *RoutePlan) error {
	t.Helper()
	var err error
	switch kind {
	case "chat":
		_, err = plan.ExecuteChat(context.Background())
	case "generate":
		_, err = plan.ExecuteGenerate(context.Background())
	case "embed":
		_, err = plan.ExecuteEmbed(context.Background())
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	return err
}

var nonStreamingKinds = []string{"chat", "generate", "embed"}

// ---------------------------------------------------------------------------
// Bracket ordering
// ---------------------------------------------------------------------------

func TestExecuteBracketOrderHappyPath(t *testing.T) {
	for _, kind := range nonStreamingKinds {
		t.Run(kind, func(t *testing.T) {
			prov := healthyMock("ollama-a")
			rec := &rpMockRecorder{}
			plan := newTestPlan(prov, rec)
			ra := &recordingAdmitter{}
			plan.setAdmission(ra)

			if err := executeKind(t, kind, plan); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			want := []string{"acquire ollama-a/test-model", "release ollama-a/test-model"}
			got := ra.getEvents()
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

func TestExecuteFallbackReleasesPrimaryBeforeAcquiringFallback(t *testing.T) {
	for _, kind := range nonStreamingKinds {
		t.Run(kind, func(t *testing.T) {
			rec := &rpMockRecorder{}
			plan := admissionFallbackPlan(failingMock("ollama-a"), healthyMock("ollama-b"), rec)
			ra := &recordingAdmitter{}
			plan.setAdmission(ra)

			if err := executeKind(t, kind, plan); err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			want := []string{
				"acquire ollama-a/test-model", "release ollama-a/test-model",
				"acquire ollama-b/test-model", "release ollama-b/test-model",
			}
			got := ra.getEvents()
			if len(got) != len(want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("events[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Admission-failure semantics (§4)
// ---------------------------------------------------------------------------

func TestPrimaryAdmissionFailureMakesNoAttemptAndNoSignals(t *testing.T) {
	for _, kind := range nonStreamingKinds {
		t.Run(kind, func(t *testing.T) {
			prov := healthyMock("ollama-a")
			rec := &rpMockRecorder{}
			plan := newTestPlan(prov, rec)
			key := ModelKey{Provider: "ollama-a", Model: "test-model"}
			ra := &recordingAdmitter{errFor: map[ModelKey]error{key: context.Canceled}}
			plan.setAdmission(ra)

			err := executeKind(t, kind, plan)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			calls := prov.getChatCalls() + prov.getGenCalls() + prov.getEmbedCalls()
			if calls != 0 {
				t.Fatalf("provider calls = %d, want 0 (no attempt on admission failure)", calls)
			}
			if n := len(rec.getSuccesses()) + len(rec.getFailures()) + len(rec.getWarmthUses()); n != 0 {
				t.Fatalf("recorder signals = %d, want 0", n)
			}
		})
	}
}

func TestFallbackAdmissionFailureIsTerminalButKeepsPriorAttempts(t *testing.T) {
	for _, kind := range nonStreamingKinds {
		t.Run(kind, func(t *testing.T) {
			primary := failingMock("ollama-a")
			fb := healthyMock("ollama-b")
			rec := &rpMockRecorder{}
			plan := admissionFallbackPlan(primary, fb, rec)
			fbKey := ModelKey{Provider: "ollama-b", Model: "test-model"}
			ra := &recordingAdmitter{errFor: map[ModelKey]error{fbKey: ErrRouterClosed}}
			plan.setAdmission(ra)

			err := executeKind(t, kind, plan)
			if !errors.Is(err, ErrRouterClosed) {
				t.Fatalf("err = %v, want ErrRouterClosed", err)
			}
			if calls := fb.getChatCalls() + fb.getGenCalls() + fb.getEmbedCalls(); calls != 0 {
				t.Fatalf("fallback provider calls = %d, want 0", calls)
			}
			// The primary's real infrastructure failure was recorded.
			failures := rec.getFailures()
			if len(failures) != 1 || failures[0].Provider != "ollama-a" {
				t.Fatalf("failures = %v, want exactly the primary's", failures)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Crossed-fallback deadlock (release-before-acquire, real gate)
// ---------------------------------------------------------------------------

func TestCrossedFallbacksResolveWithoutDeadlock(t *testing.T) {
	keyA := ModelKey{Provider: "prov-a", Model: "test-model"}
	keyB := ModelKey{Provider: "prov-b", Model: "test-model"}
	src := newAdmFakeSource()
	src.set(keyA, 1)
	src.set(keyB, 1)
	sa := newSlotAdmission(src, make(chan struct{}))

	// plan1: primary A fails infra -> fallback B succeeds
	// plan2: primary B fails infra -> fallback A succeeds
	rec := &rpMockRecorder{}
	plan1 := admissionFallbackPlan(failingMock("prov-a"), healthyMock("prov-b"), rec)
	plan2 := admissionFallbackPlan(failingMock("prov-b"), healthyMock("prov-a"), rec)
	adm := &realAdmitter{sa: sa}
	plan1.setAdmission(adm)
	plan2.setAdmission(adm)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range []*RoutePlan{plan1, plan2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.ExecuteChat(context.Background())
		}()
	}
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(admWaitTimeout):
		t.Fatal("crossed fallbacks deadlocked (permit held across fallback acquire?)")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("plan%d err = %v, want nil", i+1, err)
		}
	}
	sa.mu.Lock()
	defer sa.mu.Unlock()
	for key, g := range sa.gates {
		if g.inflight != 0 {
			t.Fatalf("gate %v inflight = %d after both turns, want 0", key, g.inflight)
		}
	}
}

// realAdmitter adapts a bare slotAdmission to the plan seam without a
// Router, so gate-integration plan tests stay self-contained.
type realAdmitter struct{ sa *slotAdmission }

func (r *realAdmitter) acquireSlot(ctx context.Context, key ModelKey) (func(), error) {
	return r.sa.acquire(ctx, key)
}

// ---------------------------------------------------------------------------
// Execute-level capacity AC + ungoverned parallelism
// ---------------------------------------------------------------------------

// gatedChatProvider blocks Chat until the test opens its gate.
type gatedChatProvider struct {
	*rpMockProvider
	started chan struct{}
	gate    chan struct{}
}

func (g *gatedChatProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	g.started <- struct{}{}
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.rpMockProvider.Chat(ctx, req)
}

func TestExecuteChatBoundsInFlightToCapacity(t *testing.T) {
	key := ModelKey{Provider: "ollama-a", Model: "test-model"}
	src := newAdmFakeSource()
	src.set(key, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	prov := &gatedChatProvider{
		rpMockProvider: healthyMock("ollama-a"),
		started:        make(chan struct{}, 2),
		gate:           make(chan struct{}),
	}
	adm := &realAdmitter{sa: sa}
	mkPlan := func() *RoutePlan {
		rec := &rpMockRecorder{}
		p := newTestPlan(prov.rpMockProvider, rec)
		p.Provider = prov
		p.setAdmission(adm)
		return p
	}

	errC := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := mkPlan().ExecuteChat(context.Background())
			errC <- err
		}()
	}
	// Exactly one provider call in flight; the second caller queues at the
	// admission gate, NOT at the provider.
	select {
	case <-prov.started:
	case <-time.After(admWaitTimeout):
		t.Fatal("first caller never reached the provider")
	}
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("second caller never queued at the gate")
	}
	select {
	case <-prov.started:
		t.Fatal("second provider call started above capacity 1")
	default:
	}

	close(prov.gate) // finish the first call; waiter admits and completes
	for range 2 {
		select {
		case err := <-errC:
			if err != nil {
				t.Fatalf("ExecuteChat: %v", err)
			}
		case <-time.After(admWaitTimeout):
			t.Fatal("caller never completed after gate opened")
		}
	}
	sa.mu.Lock()
	defer sa.mu.Unlock()
	if g := sa.gates[key]; g.inflight != 0 || g.admitted != 2 || g.queued != 1 {
		t.Fatalf("gate = %+v, want inflight 0, admitted 2, queued 1", g)
	}
}

// barrierChatProvider proves N callers are inside Chat simultaneously.
type barrierChatProvider struct {
	*rpMockProvider
	arrived atomic.Int32
	n       int32
	barrier chan struct{}
}

func (b *barrierChatProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if b.arrived.Add(1) == b.n {
		close(b.barrier)
	}
	select {
	case <-b.barrier:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.rpMockProvider.Chat(ctx, req)
}

func TestUngovernedExecuteChatRunsFullyParallel(t *testing.T) {
	// Real gate, source that does NOT govern the key: two concurrent
	// ExecuteChat must be inside the provider at the same time — proof of
	// no serialization, not merely "both completed".
	src := newAdmFakeSource() // key absent => ungoverned
	sa := newSlotAdmission(src, make(chan struct{}))
	prov := &barrierChatProvider{
		rpMockProvider: healthyMock("ollama-a"),
		n:              2,
		barrier:        make(chan struct{}),
	}
	adm := &realAdmitter{sa: sa}

	errC := make(chan error, 2)
	for range 2 {
		go func() {
			rec := &rpMockRecorder{}
			p := newTestPlan(prov.rpMockProvider, rec)
			p.Provider = prov
			p.setAdmission(adm)
			_, err := p.ExecuteChat(context.Background())
			errC <- err
		}()
	}
	for range 2 {
		select {
		case err := <-errC:
			if err != nil {
				t.Fatalf("ExecuteChat: %v", err)
			}
		case <-time.After(admWaitTimeout):
			t.Fatal("ungoverned callers were serialized (barrier never opened)")
		}
	}
	sa.mu.Lock()
	defer sa.mu.Unlock()
	if len(sa.gates) != 0 {
		t.Fatalf("ungoverned execute created %d gate entries, want 0", len(sa.gates))
	}
}
