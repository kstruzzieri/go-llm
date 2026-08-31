package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSlotSource records seam traffic — keys on BOTH methods, so a
// wrong-key mutation in Router.SlotCapacity or RecordSlotUse cannot
// survive.
type fakeSlotSource struct {
	mu           sync.Mutex
	used         []ModelKey
	capacityKeys []ModelKey
	capacity     int
	governed     bool
	closed       int
	closeErr     error
}

func (f *fakeSlotSource) Capacity(key ModelKey) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacityKeys = append(f.capacityKeys, key)
	return f.capacity, f.governed
}

func (f *fakeSlotSource) RecordUse(key ModelKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used = append(f.used, key)
}

func (f *fakeSlotSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func TestRouterSlotCapacityWithoutSource(t *testing.T) {
	router, _ := setupTestRouter(t)
	defer func() { _ = router.Close() }()
	if n, ok := router.SlotCapacity(ModelKey{Provider: "test", Model: "qwen3:8b"}); ok || n != 0 {
		t.Fatalf("no-source SlotCapacity = (%d, %v), want (0, false)", n, ok)
	}
	router.RecordSlotUse(ModelKey{Provider: "test", Model: "qwen3:8b"}) // must not panic
}

func TestRouterSlotSourceWiring(t *testing.T) {
	fake := &fakeSlotSource{capacity: 4, governed: true}
	router, _ := setupTestRouter(t, WithSlotSource(fake))
	want := ModelKey{Provider: "test", Model: "qwen3:8b"}

	if n, ok := router.SlotCapacity(want); !ok || n != 4 {
		t.Fatalf("SlotCapacity = (%d, %v), want (4, true)", n, ok)
	}
	router.RecordSlotUse(want)

	fake.mu.Lock()
	if len(fake.capacityKeys) != 1 || fake.capacityKeys[0] != want {
		fake.mu.Unlock()
		t.Fatalf("Capacity saw keys %v, want exactly [%v]", fake.capacityKeys, want)
	}
	if len(fake.used) != 1 || fake.used[0] != want {
		fake.mu.Unlock()
		t.Fatalf("RecordUse saw keys %v, want exactly [%v]", fake.used, want)
	}
	fake.mu.Unlock()

	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.closed != 1 {
		t.Fatalf("source Close calls = %d, want 1", fake.closed)
	}
}

// slotMockRecorder is rpMockRecorder plus the optional RecordSlotUse — a
// slot-aware recorder for the optional-interface path.
type slotMockRecorder struct {
	rpMockRecorder
	slotUses []ModelKey
}

func (r *slotMockRecorder) RecordSlotUse(key ModelKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slotUses = append(r.slotUses, key)
}

func (r *slotMockRecorder) getSlotUses() []ModelKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelKey, len(r.slotUses))
	copy(out, r.slotUses)
	return out
}

// Success fires the slot signal for the SERVED model — the last attempt's
// key, not the planned Profile key. Failed-primary + succeeded-fallback
// attempts make those two keys differ, so an implementation that signals
// rp.Profile.Key on every success (or drops the model) fails here.
// newTestPlan takes *rpMockRecorder concretely and Go embedding is not
// subtyping, so the plan is built with the embedded recorder and
// re-pointed at the slot-aware dynamic type via the public SetRecorder.
func TestHandleResultSuccessRecordsSlotUse(t *testing.T) {
	rec := &slotMockRecorder{}
	rp := newTestPlan(&rpMockProvider{name: "ollama-a", caps: CapChat}, &rec.rpMockRecorder)
	rp.SetRecorder(rec)
	attempts := []RouteAttempt{
		{Key: ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "network"},
		{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}, Status: AttemptStatusSucceeded},
	}
	_ = rp.handleResult(nil, 0, attempts)

	want := attempts[1].Key
	slots := rec.getSlotUses()
	if len(slots) != 1 || slots[0] != want {
		t.Fatalf("slotUses = %v, want exactly [%v] (the served fallback key)", slots, want)
	}
}

// Cancellation records warmth (existing semantics, deliberately kept) but
// NEVER slot use — a slot probe on behalf of a cancelled request could
// make llama-swap load a model nobody is using. If a future edit moves
// the slot call to the shared warmth site, this goes red. Attempt
// fabrication mirrors TestHandleResultUsesLastAttemptKeyForCancellationWarmth.
func TestHandleResultCancellationRecordsNoSlotUse(t *testing.T) {
	rec := &slotMockRecorder{}
	rp := newTestPlan(&rpMockProvider{name: "ollama-a", caps: CapChat}, &rec.rpMockRecorder)
	rp.SetRecorder(rec)
	attempts := []RouteAttempt{
		{Key: ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "network"},
		{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}, Status: AttemptStatusUnknown},
	}
	_ = rp.handleResult(context.Canceled, 0, attempts)

	if got := rec.getWarmthUses(); len(got) != 1 {
		t.Fatalf("warmthUses = %d, want 1 (existing cancellation semantics)", len(got))
	}
	if got := rec.getSlotUses(); len(got) != 0 {
		t.Fatalf("slotUses = %v, want none on cancellation", got)
	}
}

// A recorder implementing ONLY the existing three RouteRecorder methods
// (rpMockRecorder itself, unmodified) still works on the success path:
// runtime proof the interface did not grow, so external recorders
// installed via the public SetRecorder keep compiling and running.
func TestHandleResultSuccessWithLegacyRecorder(t *testing.T) {
	rec := &rpMockRecorder{}
	rp := newTestPlan(&rpMockProvider{name: "ollama", caps: CapChat}, rec)
	_ = rp.handleResult(nil, 0, nil)
	if got := rec.getSuccesses(); len(got) != 1 {
		t.Fatalf("successes = %d, want 1", len(got))
	}
}

// "Success only" means ONLY success: ordinary failures and deadline
// expiry must produce zero slot signals too, not just context.Canceled —
// a branch condition weakened to "anything but Canceled" would slip past
// the cancellation test alone.
func TestHandleResultNonSuccessRecordsNoSlotUse(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "infrastructure failure", err: &HTTPStatusError{StatusCode: 500}},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "plain error", err: errors.New("model exploded")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &slotMockRecorder{}
			rp := newTestPlan(&rpMockProvider{name: "ollama", caps: CapChat}, &rec.rpMockRecorder)
			rp.SetRecorder(rec)
			attempts := []RouteAttempt{
				{Key: ModelKey{Provider: "ollama", Model: "qwen3:8b"}, Status: AttemptStatusFailed, ErrorClass: "network"},
			}
			_ = rp.handleResult(tt.err, 0, attempts)
			if got := rec.getSlotUses(); len(got) != 0 {
				t.Fatalf("slotUses = %v, want none for %v", got, tt.err)
			}
		})
	}
}

// fakeWarmthSource lets the Router close-error path be pinned for BOTH
// subsystems: dropping either Close call, or losing either error in the
// join, goes red here.
type fakeWarmthSource struct {
	mu       sync.Mutex
	closed   int
	closeErr error
}

func (f *fakeWarmthSource) IsWarm(ModelKey) bool             { return false }
func (f *fakeWarmthSource) WarmthState(ModelKey) *WarmthInfo { return nil }
func (f *fakeWarmthSource) RecordUse(ModelKey)               {}
func (f *fakeWarmthSource) Snapshot() []WarmModel            { return nil }
func (f *fakeWarmthSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func TestRouterCloseJoinsWarmthAndSlotErrors(t *testing.T) {
	warmthErr := errors.New("warmth close failed")
	slotErr := errors.New("slot close failed")
	warmth := &fakeWarmthSource{closeErr: warmthErr}
	slots := &fakeSlotSource{governed: true, capacity: 1, closeErr: slotErr}
	router, _ := setupTestRouter(t, WithWarmthSource(warmth), WithSlotSource(slots))

	err := router.Close()
	if !errors.Is(err, warmthErr) {
		t.Fatalf("Close error %v does not wrap the warmth close error", err)
	}
	if !errors.Is(err, slotErr) {
		t.Fatalf("Close error %v does not wrap the slot close error", err)
	}
	warmth.mu.Lock()
	if warmth.closed != 1 {
		warmth.mu.Unlock()
		t.Fatalf("warmth Close calls = %d, want 1", warmth.closed)
	}
	warmth.mu.Unlock()
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if slots.closed != 1 {
		t.Fatalf("slot Close calls = %d, want 1", slots.closed)
	}
}

// ---------------------------------------------------------------------------
// Admission wiring (#400)
// ---------------------------------------------------------------------------

func TestRouterWithSlotSourceStampsAdmissionOnPlans(t *testing.T) {
	fake := &fakeSlotSource{capacity: 2, governed: true}
	router, _ := setupTestRouter(t, WithSlotSource(fake))
	defer func() { _ = router.Close() }()
	plan, err := router.Route(context.Background(), RoutingRequest{Model: "qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.admission == nil {
		t.Fatal("plan built by a slot-sourced Router has nil admission seam")
	}

	bare, _ := setupTestRouter(t)
	defer func() { _ = bare.Close() }()
	barePlan, err := bare.Route(context.Background(), RoutingRequest{Model: "qwen3:8b", UseCase: "chat"})
	if err != nil {
		t.Fatalf("Route (no source): %v", err)
	}
	if barePlan.admission != nil {
		t.Fatal("plan built by a source-less Router must have nil admission seam")
	}
}

func TestSlotAdmissionSnapshotDeterministicOrderAndNilWithoutSource(t *testing.T) {
	bare, _ := setupTestRouter(t)
	defer func() { _ = bare.Close() }()
	if got := bare.SlotAdmissionSnapshot(); got != nil {
		t.Fatalf("no-source snapshot = %v, want nil", got)
	}

	src := newAdmFakeSource()
	keys := []ModelKey{
		{Provider: "p", Model: "b"},
		{Provider: "p", Model: "a"},
		{Provider: "o", Model: "z"},
	}
	for _, k := range keys {
		src.set(k, 3)
	}
	router, _ := setupTestRouter(t, WithSlotSource(src))
	defer func() { _ = router.Close() }()
	// Create gate entries in deliberately unsorted order.
	for _, k := range keys {
		rel, err := router.acquireSlot(context.Background(), k)
		if err != nil {
			t.Fatalf("acquireSlot(%v): %v", k, err)
		}
		rel()
	}
	snap := router.SlotAdmissionSnapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	wantOrder := []ModelKey{
		{Provider: "o", Model: "z"},
		{Provider: "p", Model: "a"},
		{Provider: "p", Model: "b"},
	}
	for i, w := range wantOrder {
		got := snap[i]
		if got.Provider != w.Provider || got.Model != w.Model {
			t.Fatalf("snapshot[%d] = %s/%s, want %s/%s", i, got.Provider, got.Model, w.Provider, w.Model)
		}
		if got.Capacity != 3 || got.InFlight != 0 || got.Waiting != 0 ||
			got.Admitted != 1 || got.Queued != 0 || got.Rejected != 0 {
			t.Fatalf("snapshot[%d] = %+v, want capacity 3, admitted 1, all else 0", i, got)
		}
	}
}

func TestRouterCloseUnblocksAdmissionWaiters(t *testing.T) {
	src := newAdmFakeSource()
	key := ModelKey{Provider: "p", Model: "m"}
	src.set(key, 1)
	router, _ := setupTestRouter(t, WithSlotSource(src))
	queuedC := make(chan ModelKey, 1)
	router.admission.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := router.acquireSlot(context.Background(), key)
	if err != nil {
		t.Fatalf("acquireSlot: %v", err)
	}
	errC := make(chan error, 1)
	go func() {
		_, err := router.acquireSlot(context.Background(), key)
		errC <- err
	}()
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("waiter never queued")
	}
	if err := router.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errC:
		if !errors.Is(err, ErrRouterClosed) {
			t.Fatalf("waiter err = %v, want ErrRouterClosed", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("Router.Close did not unblock the admission waiter")
	}
	rel1() // release after close must be safe
}

// acquireSlot on a Router with no slot source must be a safe no-op, not a
// nil dereference — the seam is real Router API surface even though the
// route-plan bracket short-circuits before reaching it.
func TestAcquireSlotWithoutSourceIsNoop(t *testing.T) {
	bare, _ := setupTestRouter(t)
	defer func() { _ = bare.Close() }()
	rel, err := bare.acquireSlot(context.Background(), ModelKey{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatalf("acquireSlot on source-less router: %v", err)
	}
	rel()
}
