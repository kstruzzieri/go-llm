package provider

import (
	"context"
	"sync"
	"testing"
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
	return nil
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
