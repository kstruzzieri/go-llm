package provider

import (
	"context"
	"errors"
	"fmt"
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

// ---------------------------------------------------------------------------
// Streaming brackets (Task 4)
// ---------------------------------------------------------------------------

// scriptedChatStream is a test-driven streaming provider: the test emits
// chunks and the final return value on command, so "mid-stream" is a
// deterministic program state, not a timing accident.
type scriptedChatStream struct {
	*rpMockProvider
	started chan struct{}
	emit    chan ChatResponse
	finish  chan error
}

func (s *scriptedChatStream) ChatStream(ctx context.Context, _ ChatRequest, fn func(ChatResponse) error) error {
	s.started <- struct{}{}
	for {
		select {
		case chunk := <-s.emit:
			if err := fn(chunk); err != nil {
				return err
			}
		case err := <-s.finish:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type scriptedGenStream struct {
	*rpMockProvider
	started chan struct{}
	emit    chan GenerateResponse
	finish  chan error
}

func (s *scriptedGenStream) GenerateStream(ctx context.Context, _ GenerateRequest, fn func(GenerateResponse) error) error {
	s.started <- struct{}{}
	for {
		select {
		case chunk := <-s.emit:
			if err := fn(chunk); err != nil {
				return err
			}
		case err := <-s.finish:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// streamHarness wires a real capacity-1 gate around a scripted stream and
// returns the pieces each release-path test needs.
type streamHarness struct {
	sa      *slotAdmission
	key     ModelKey
	queuedC chan ModelKey
}

func newStreamHarness() *streamHarness {
	key := ModelKey{Provider: "ollama-a", Model: "test-model"}
	src := newAdmFakeSource()
	src.set(key, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	h := &streamHarness{sa: sa, key: key, queuedC: make(chan ModelKey, 1)}
	sa.queuedHook = func(k ModelKey) { h.queuedC <- k }
	return h
}

// assertPermitHeld proves the stream is holding the key's permit RIGHT NOW:
// a probe acquire must queue, not admit. The probe is then cancelled so it
// leaves no residue (rejected+1 is asserted where relevant).
func (h *streamHarness) assertPermitHeld(t *testing.T) {
	t.Helper()
	probeCtx, probeCancel := context.WithCancel(context.Background())
	probeDone := make(chan error, 1)
	go func() {
		_, err := h.sa.acquire(probeCtx, h.key)
		probeDone <- err
	}()
	select {
	case <-h.queuedC:
	case err := <-probeDone:
		t.Fatalf("probe acquire returned (%v) while stream in flight — permit not held", err)
	case <-time.After(admWaitTimeout):
		t.Fatal("probe acquire neither queued nor returned")
	}
	probeCancel()
	select {
	case <-probeDone:
	case <-time.After(admWaitTimeout):
		t.Fatal("cancelled probe never returned")
	}
}

func (h *streamHarness) assertBalanced(t *testing.T, wantAdmitted uint64) {
	t.Helper()
	h.sa.mu.Lock()
	defer h.sa.mu.Unlock()
	g := h.sa.gates[h.key]
	if g == nil {
		t.Fatal("no gate entry for streamed key")
	}
	if g.inflight != 0 {
		t.Fatalf("inflight = %d after stream, want 0 (released exactly once)", g.inflight)
	}
	if g.admitted != wantAdmitted {
		t.Fatalf("admitted = %d, want %d", g.admitted, wantAdmitted)
	}
}

func TestChatStreamHoldsPermitAcrossStreamAndReleasesOnCompletion(t *testing.T) {
	h := newStreamHarness()
	prov := &scriptedChatStream{
		rpMockProvider: healthyMock("ollama-a"),
		started:        make(chan struct{}, 1),
		emit:           make(chan ChatResponse),
		finish:         make(chan error),
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov.rpMockProvider, rec)
	plan.Provider = prov
	plan.setAdmission(&realAdmitter{sa: h.sa})

	errC := make(chan error, 1)
	go func() {
		errC <- plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil })
	}()
	<-prov.started
	prov.emit <- ChatResponse{Content: "chunk", Done: false}
	h.assertPermitHeld(t) // mid-stream: permit still held
	prov.emit <- ChatResponse{Content: "", Done: true}
	prov.finish <- nil
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("ExecuteChatStream: %v", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("stream never completed")
	}
	h.assertBalanced(t, 1)
}

func TestChatStreamReleasesOnMidStreamError(t *testing.T) {
	h := newStreamHarness()
	prov := &scriptedChatStream{
		rpMockProvider: healthyMock("ollama-a"),
		started:        make(chan struct{}, 1),
		emit:           make(chan ChatResponse),
		finish:         make(chan error),
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov.rpMockProvider, rec)
	plan.Provider = prov
	plan.setAdmission(&realAdmitter{sa: h.sa})

	errC := make(chan error, 1)
	go func() {
		errC <- plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil })
	}()
	<-prov.started
	prov.emit <- ChatResponse{Content: "visible", Done: false} // delivered=true suppresses fallback
	h.assertPermitHeld(t)
	prov.finish <- &HTTPStatusError{StatusCode: 503}
	select {
	case err := <-errC:
		if err == nil {
			t.Fatal("expected mid-stream error to propagate")
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("stream never returned after error")
	}
	h.assertBalanced(t, 1)
}

func TestChatStreamReleasesOnCallerAbandonment(t *testing.T) {
	h := newStreamHarness()
	prov := &scriptedChatStream{
		rpMockProvider: healthyMock("ollama-a"),
		started:        make(chan struct{}, 1),
		emit:           make(chan ChatResponse),
		finish:         make(chan error),
	}
	rec := &rpMockRecorder{}
	plan := newTestPlan(prov.rpMockProvider, rec)
	plan.Provider = prov
	plan.setAdmission(&realAdmitter{sa: h.sa})

	ctx, cancel := context.WithCancel(context.Background())
	errC := make(chan error, 1)
	go func() {
		errC <- plan.ExecuteChatStream(ctx, func(ChatResponse) error { return nil })
	}()
	<-prov.started
	prov.emit <- ChatResponse{Content: "partial", Done: false}
	h.assertPermitHeld(t)
	cancel() // abandonment: provider returns ctx.Err()
	select {
	case err := <-errC:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("stream never returned after abandonment")
	}
	h.assertBalanced(t, 1)
}

func TestGenerateStreamPermitLifecycleAllPaths(t *testing.T) {
	// The generate-stream twin of the three chat tests above, table-driven
	// over the release path.
	paths := []struct {
		name  string
		drive func(t *testing.T, prov *scriptedGenStream, cancel context.CancelFunc, h *streamHarness)
		check func(t *testing.T, err error)
	}{
		{
			name: "completion",
			drive: func(t *testing.T, prov *scriptedGenStream, _ context.CancelFunc, h *streamHarness) {
				prov.emit <- GenerateResponse{Response: "chunk"}
				h.assertPermitHeld(t)
				prov.emit <- GenerateResponse{Done: true}
				prov.finish <- nil
			},
			check: func(t *testing.T, err error) {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			},
		},
		{
			name: "mid-stream error",
			drive: func(t *testing.T, prov *scriptedGenStream, _ context.CancelFunc, h *streamHarness) {
				prov.emit <- GenerateResponse{Response: "visible"}
				h.assertPermitHeld(t)
				prov.finish <- &HTTPStatusError{StatusCode: 503}
			},
			check: func(t *testing.T, err error) {
				if err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			name: "abandonment",
			drive: func(t *testing.T, prov *scriptedGenStream, cancel context.CancelFunc, h *streamHarness) {
				prov.emit <- GenerateResponse{Response: "partial"}
				h.assertPermitHeld(t)
				cancel()
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled", err)
				}
			},
		},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			h := newStreamHarness()
			prov := &scriptedGenStream{
				rpMockProvider: healthyMock("ollama-a"),
				started:        make(chan struct{}, 1),
				emit:           make(chan GenerateResponse),
				finish:         make(chan error),
			}
			rec := &rpMockRecorder{}
			plan := newTestPlan(prov.rpMockProvider, rec)
			plan.Provider = prov
			plan.setAdmission(&realAdmitter{sa: h.sa})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errC := make(chan error, 1)
			go func() {
				errC <- plan.ExecuteGenerateStream(ctx, func(GenerateResponse) error { return nil })
			}()
			<-prov.started
			tc.drive(t, prov, cancel, h)
			select {
			case err := <-errC:
				tc.check(t, err)
			case <-time.After(admWaitTimeout):
				t.Fatal("stream never returned")
			}
			h.assertBalanced(t, 1)
		})
	}
}

func TestStreamFallbackReleasesPrimaryBeforeAcquiringFallback(t *testing.T) {
	kinds := []struct {
		name string
		run  func(plan *RoutePlan) error
	}{
		{name: "chat", run: func(plan *RoutePlan) error {
			return plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil })
		}},
		{name: "generate", run: func(plan *RoutePlan) error {
			return plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil })
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			// Primary: zero chunks then infra error (no visible content =>
			// fallback allowed). Fallback: clean Done.
			infra := &HTTPStatusError{StatusCode: 503}
			primary := &rpMockProvider{
				name: "ollama-a", caps: CapChat | CapGenerate | CapStream,
				chatStreamErr: infra, genStreamErr: infra,
			}
			fb := &rpMockProvider{
				name: "ollama-b", caps: CapChat | CapGenerate | CapStream,
				chatStreamChunks: []ChatResponse{{Content: "ok", Done: true}},
				genStreamChunks:  []GenerateResponse{{Response: "ok", Done: true}},
			}
			rec := &rpMockRecorder{}
			plan := admissionFallbackPlan(primary, fb, rec)
			ra := &recordingAdmitter{}
			plan.setAdmission(ra)

			if err := k.run(plan); err != nil {
				t.Fatalf("%s stream: %v", k.name, err)
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

func TestStreamPrimaryAdmissionFailureMakesNoAttempt(t *testing.T) {
	kinds := []struct {
		name string
		run  func(plan *RoutePlan, fn func()) error
	}{
		{name: "chat", run: func(plan *RoutePlan, fn func()) error {
			return plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { fn(); return nil })
		}},
		{name: "generate", run: func(plan *RoutePlan, fn func()) error {
			return plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { fn(); return nil })
		}},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			prov := &rpMockProvider{
				name: "ollama-a", caps: CapChat | CapGenerate | CapStream,
				chatStreamChunks: []ChatResponse{{Content: "ok", Done: true}},
				genStreamChunks:  []GenerateResponse{{Response: "ok", Done: true}},
			}
			rec := &rpMockRecorder{}
			plan := newTestPlan(prov, rec)
			key := ModelKey{Provider: "ollama-a", Model: "test-model"}
			ra := &recordingAdmitter{errFor: map[ModelKey]error{key: ErrRouterClosed}}
			plan.setAdmission(ra)

			callbackRan := false
			err := k.run(plan, func() { callbackRan = true })
			if !errors.Is(err, ErrRouterClosed) {
				t.Fatalf("err = %v, want ErrRouterClosed", err)
			}
			if callbackRan {
				t.Fatal("stream callback ran despite admission failure")
			}
			if calls := prov.getChatCalls() + prov.getGenCalls(); calls != 0 {
				t.Fatalf("provider stream calls = %d, want 0", calls)
			}
			if n := len(rec.getSuccesses()) + len(rec.getFailures()) + len(rec.getWarmthUses()); n != 0 {
				t.Fatalf("recorder signals = %d, want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AdmittedEmbedder seam (Task 5)
// ---------------------------------------------------------------------------

// admittedEmbedFake implements Provider + AdmittedEmbedder, invoking the
// admit func leader-style: acquire, wrap failures with %w (as the exported
// contract requires), release after the underlying embed.
type admittedEmbedFake struct {
	*rpMockProvider
	mu            sync.Mutex
	admittedCalls int
}

func (f *admittedEmbedFake) EmbedAdmitted(ctx context.Context, req EmbedRequest, admit AdmitFunc) (*EmbedResponse, error) {
	f.mu.Lock()
	f.admittedCalls++
	f.mu.Unlock()
	release, err := admit(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider: fake: embed: admission: %w", err)
	}
	defer release()
	return f.Embed(ctx, req)
}

func (f *admittedEmbedFake) getAdmittedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admittedCalls
}

func admittedEmbedPlanPair(primaryErr error, rec *rpMockRecorder) (*RoutePlan, *admittedEmbedFake, *admittedEmbedFake) {
	primary := &admittedEmbedFake{rpMockProvider: healthyMock("ollama-a")}
	if primaryErr != nil {
		primary.embedErr = primaryErr
	}
	fb := &admittedEmbedFake{rpMockProvider: healthyMock("ollama-b")}
	plan := admissionFallbackPlan(primary.rpMockProvider, fb.rpMockProvider, rec)
	plan.Provider = primary
	plan.Fallbacks[0].Provider = fb
	return plan, primary, fb
}

func TestExecuteEmbedDispatchesToAdmittedEmbedderWithPrimaryKey(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, _ := admittedEmbedPlanPair(nil, rec)
	ra := &recordingAdmitter{}
	plan.setAdmission(ra)

	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil || resp == nil {
		t.Fatalf("ExecuteEmbed = (%v, %v)", resp, err)
	}
	if got := primary.getAdmittedCalls(); got != 1 {
		t.Fatalf("EmbedAdmitted calls = %d, want 1 (dispatch must prefer the seam)", got)
	}
	want := []string{"acquire ollama-a/test-model", "release ollama-a/test-model"}
	got := ra.getEvents()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v (admit bound to the PRIMARY key)", got, want)
	}
}

func TestExecuteEmbedFallbackAdmitBoundToFallbackKey(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, _, fb := admittedEmbedPlanPair(&HTTPStatusError{StatusCode: 503}, rec)
	ra := &recordingAdmitter{}
	plan.setAdmission(ra)

	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil || resp == nil {
		t.Fatalf("ExecuteEmbed = (%v, %v)", resp, err)
	}
	if got := fb.getAdmittedCalls(); got != 1 {
		t.Fatalf("fallback EmbedAdmitted calls = %d, want 1", got)
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
			t.Fatalf("events[%d] = %q, want %q (admit bound to each attempt's OWN key)", i, got[i], want[i])
		}
	}
}

func TestExecuteEmbedNilAdmissionStillUsesSeamWithNoopAdmit(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, _ := admittedEmbedPlanPair(nil, rec)
	// no setAdmission: nil seam

	resp, err := plan.ExecuteEmbed(context.Background())
	if err != nil || resp == nil {
		t.Fatalf("ExecuteEmbed = (%v, %v)", resp, err)
	}
	if got := primary.getAdmittedCalls(); got != 1 {
		t.Fatalf("EmbedAdmitted calls = %d, want 1 (uniform path, admit is a no-op)", got)
	}
}

func TestExecuteEmbedAdmissionFailureThroughProviderMintsNoAttempt(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, _ := admittedEmbedPlanPair(nil, rec)
	key := ModelKey{Provider: "ollama-a", Model: "test-model"}
	ra := &recordingAdmitter{errFor: map[ModelKey]error{key: ErrRouterClosed}}
	plan.setAdmission(ra)

	resp, err := plan.ExecuteEmbed(context.Background())
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if !errors.Is(err, ErrRouterClosed) {
		t.Fatalf("err = %v, want ErrRouterClosed", err)
	}
	var ae *admissionError
	if errors.As(err, &ae) {
		t.Fatalf("caller received the internal admissionError wrapper %v; want the unwrapped original", err)
	}
	if got := primary.getAdmittedCalls(); got != 1 {
		t.Fatalf("EmbedAdmitted calls = %d, want 1 (the flight-that-failed-admission ran)", got)
	}
	if got := primary.getEmbedCalls(); got != 0 {
		t.Fatalf("underlying embed calls = %d, want 0", got)
	}
	if n := len(rec.getSuccesses()) + len(rec.getFailures()) + len(rec.getWarmthUses()); n != 0 {
		t.Fatalf("recorder signals = %d, want 0 (admission failure mints no attempt)", n)
	}
}

func TestExecuteEmbedFallbackAdmissionFailureKeepsPriorAttempts(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, fb := admittedEmbedPlanPair(&HTTPStatusError{StatusCode: 503}, rec)
	fbKey := ModelKey{Provider: "ollama-b", Model: "test-model"}
	ra := &recordingAdmitter{errFor: map[ModelKey]error{fbKey: ErrRouterClosed}}
	plan.setAdmission(ra)

	resp, err := plan.ExecuteEmbed(context.Background())
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if !errors.Is(err, ErrRouterClosed) {
		t.Fatalf("err = %v, want ErrRouterClosed", err)
	}
	var ae *admissionError
	if errors.As(err, &ae) {
		t.Fatalf("caller received the internal admissionError wrapper %v", err)
	}
	if got := primary.getAdmittedCalls(); got != 1 {
		t.Fatalf("primary EmbedAdmitted calls = %d, want 1", got)
	}
	if got := fb.getEmbedCalls(); got != 0 {
		t.Fatalf("fallback underlying embed calls = %d, want 0", got)
	}
	failures := rec.getFailures()
	if len(failures) != 1 || failures[0].Provider != "ollama-a" {
		t.Fatalf("failures = %v, want exactly the primary's real infra failure", failures)
	}
}

func TestExecuteEmbedDeadCallerRejectedBeforeFlightWhenGoverned(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, _ := admittedEmbedPlanPair(nil, rec)
	ra := &recordingAdmitter{} // governed by default; rejects dead ctx
	plan.setAdmission(ra)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := plan.ExecuteEmbed(ctx)
	if resp != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteEmbed = (%v, %v), want (nil, context.Canceled)", resp, err)
	}
	if got := primary.getAdmittedCalls(); got != 0 {
		t.Fatalf("EmbedAdmitted calls = %d, want 0 (no flight for a dead caller)", got)
	}
	if n := len(rec.getSuccesses()) + len(rec.getFailures()) + len(rec.getWarmthUses()); n != 0 {
		t.Fatalf("recorder signals = %d, want 0", n)
	}
}

func TestExecuteEmbedDeadCallerUngovernedStillReachesProvider(t *testing.T) {
	rec := &rpMockRecorder{}
	plan, primary, _ := admittedEmbedPlanPair(nil, rec)
	key := ModelKey{Provider: "ollama-a", Model: "test-model"}
	ra := &recordingAdmitter{ungoverned: map[ModelKey]bool{key: true}}
	plan.setAdmission(ra)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Bit-identity: today the provider receives the cancelled ctx and
	// decides for itself; the admission layer must not intercept.
	if _, err := plan.ExecuteEmbed(ctx); err != nil {
		t.Fatalf("ExecuteEmbed = %v, want nil (fake ignores ctx, mirroring provider-owned handling)", err)
	}
	if got := primary.getAdmittedCalls(); got != 1 {
		t.Fatalf("EmbedAdmitted calls = %d, want 1 (ungoverned dead caller passes through)", got)
	}
}

// TestFallbackAdmissionCancelRecordsNoWarmthForPriorProvider pins the
// finalization semantics when the fallback walk ends at an admission
// gate with a cancellation: no provider call was interrupted, so the
// cancel-warmth branch must NOT fire for the previous (infra-failed)
// attempt's key — pre-#400, warmth-on-cancel only ever attributed to a
// key whose call was actually cancelled, and an infra-failed attempt
// never received warmth. The caller still sees the bare context error
// (never the internal admission marker).
func TestFallbackAdmissionCancelRecordsNoWarmthForPriorProvider(t *testing.T) {
	fbKey := ModelKey{Provider: "ollama-b", Model: "test-model"}
	infra := &HTTPStatusError{StatusCode: 503}

	paths := []struct {
		name string
		run  func(ra *recordingAdmitter, rec *rpMockRecorder) error
	}{
		{name: "chat", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			plan := admissionFallbackPlan(failingMock("ollama-a"), healthyMock("ollama-b"), rec)
			plan.setAdmission(ra)
			_, err := plan.ExecuteChat(context.Background())
			return err
		}},
		{name: "generate", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			plan := admissionFallbackPlan(failingMock("ollama-a"), healthyMock("ollama-b"), rec)
			plan.setAdmission(ra)
			_, err := plan.ExecuteGenerate(context.Background())
			return err
		}},
		{name: "embed conservative", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			plan := admissionFallbackPlan(failingMock("ollama-a"), healthyMock("ollama-b"), rec)
			plan.setAdmission(ra)
			_, err := plan.ExecuteEmbed(context.Background())
			return err
		}},
		{name: "embed admitted", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			plan, _, _ := admittedEmbedPlanPair(infra, rec)
			plan.setAdmission(ra)
			_, err := plan.ExecuteEmbed(context.Background())
			return err
		}},
		{name: "chat stream", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			primary := &rpMockProvider{
				name: "ollama-a", caps: CapChat | CapStream, chatStreamErr: infra,
			}
			fb := &rpMockProvider{
				name: "ollama-b", caps: CapChat | CapStream,
				chatStreamChunks: []ChatResponse{{Content: "ok", Done: true}},
			}
			plan := admissionFallbackPlan(primary, fb, rec)
			plan.setAdmission(ra)
			return plan.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil })
		}},
		{name: "generate stream", run: func(ra *recordingAdmitter, rec *rpMockRecorder) error {
			primary := &rpMockProvider{
				name: "ollama-a", caps: CapGenerate | CapStream, genStreamErr: infra,
			}
			fb := &rpMockProvider{
				name: "ollama-b", caps: CapGenerate | CapStream,
				genStreamChunks: []GenerateResponse{{Response: "ok", Done: true}},
			}
			plan := admissionFallbackPlan(primary, fb, rec)
			plan.setAdmission(ra)
			return plan.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil })
		}},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			rec := &rpMockRecorder{}
			ra := &recordingAdmitter{errFor: map[ModelKey]error{fbKey: context.Canceled}}
			err := tc.run(ra, rec)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			var aErr *admissionError
			if errors.As(err, &aErr) {
				t.Fatalf("caller received the internal admission marker %v; want the bare error", err)
			}
			if got := rec.getWarmthUses(); len(got) != 0 {
				t.Fatalf("warmthUses = %v, want NONE (no provider call was interrupted; the failed primary must not be warmed)", got)
			}
			failures := rec.getFailures()
			if len(failures) != 1 || failures[0].Provider != "ollama-a" {
				t.Fatalf("failures = %v, want exactly the primary's real infra failure", failures)
			}
		})
	}
}
