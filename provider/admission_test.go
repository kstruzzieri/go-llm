package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// admFakeSource is a settable per-key-capacity SlotSource for gate tests.
// (slots_router_test.go's fakeSlotSource is a seam-traffic recorder with a
// single capacity; the gate tests need per-key dynamic capacities.)
type admFakeSource struct {
	mu   sync.Mutex
	caps map[ModelKey]int // absent key => ungoverned
}

func newAdmFakeSource() *admFakeSource {
	return &admFakeSource{caps: make(map[ModelKey]int)}
}

func (f *admFakeSource) set(key ModelKey, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps[key] = n
}

func (f *admFakeSource) drop(key ModelKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.caps, key)
}

func (f *admFakeSource) Capacity(key ModelKey) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.caps[key]
	return n, ok
}

func (f *admFakeSource) RecordUse(ModelKey) {}
func (f *admFakeSource) Close() error       { return nil }

var admKey = ModelKey{Provider: "local", Model: "m"}

// admWaitTimeout is a liveness bound only — never an elapsed-time assertion.
const admWaitTimeout = 2 * time.Second

func TestAcquireAdmitsUpToCapacityAndQueuesBeyond(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 2)
	done := make(chan struct{})
	sa := newSlotAdmission(src, done)
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	type res struct {
		rel func()
		err error
	}
	third := make(chan res, 1)
	go func() {
		rel, err := sa.acquire(context.Background(), admKey)
		third <- res{rel, err}
	}()

	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("third caller never queued")
	}
	select {
	case r := <-third:
		t.Fatalf("third caller admitted above capacity (err=%v)", r.err)
	default:
	}

	rel1()
	select {
	case r := <-third:
		if r.err != nil {
			t.Fatalf("third caller after release: %v", r.err)
		}
		r.rel()
	case <-time.After(admWaitTimeout):
		t.Fatal("release did not admit the waiter")
	}
	rel2()

	sa.mu.Lock()
	g := sa.gates[admKey]
	if g.inflight != 0 {
		t.Fatalf("inflight = %d after all releases, want 0", g.inflight)
	}
	if g.admitted != 3 || g.queued != 1 || g.rejected != 0 || g.waiting != 0 {
		t.Fatalf("counters admitted=%d queued=%d rejected=%d waiting=%d, want 3/1/0/0",
			g.admitted, g.queued, g.rejected, g.waiting)
	}
	sa.mu.Unlock()
}

func TestQueuedCancelConsumesNoPermit(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errC := make(chan error, 1)
	go func() {
		_, err := sa.acquire(ctx, admKey)
		errC <- err
	}()
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("second caller never queued")
	}
	cancel()
	select {
	case err := <-errC:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("cancel did not release the waiter")
	}

	rel1()
	// The cancelled waiter must not have consumed the freed permit.
	rel3, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire after cancel: %v", err)
	}
	rel3()

	sa.mu.Lock()
	g := sa.gates[admKey]
	if g.inflight != 0 || g.admitted != 2 || g.queued != 1 || g.rejected != 1 || g.waiting != 0 {
		t.Fatalf("inflight=%d admitted=%d queued=%d rejected=%d waiting=%d, want 0/2/1/1/0",
			g.inflight, g.admitted, g.queued, g.rejected, g.waiting)
	}
	sa.mu.Unlock()
}

func TestCloseUnblocksWaitersWithErrRouterClosed(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	done := make(chan struct{})
	sa := newSlotAdmission(src, done)
	queuedC := make(chan ModelKey, 2)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	errC := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := sa.acquire(context.Background(), admKey)
			errC <- err
		}()
	}
	for range 2 {
		select {
		case <-queuedC:
		case <-time.After(admWaitTimeout):
			t.Fatal("waiter never queued")
		}
	}
	close(done)
	for range 2 {
		select {
		case err := <-errC:
			if !errors.Is(err, ErrRouterClosed) {
				t.Fatalf("waiter err = %v, want ErrRouterClosed", err)
			}
		case <-time.After(admWaitTimeout):
			t.Fatal("close did not unblock a waiter")
		}
	}
	rel1() // release after close must be safe
	sa.mu.Lock()
	g := sa.gates[admKey]
	if g.inflight != 0 || g.rejected != 2 || g.waiting != 0 {
		t.Fatalf("inflight=%d rejected=%d waiting=%d after close, want 0/2/0",
			g.inflight, g.rejected, g.waiting)
	}
	sa.mu.Unlock()
}

func TestReleaseIsExactlyOnceAndUnderflowSafe(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	rel, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	rel()              // closure double-call: Once must swallow it
	sa.release(admKey) // raw duplicate: clamp must hold at zero
	sa.mu.Lock()
	if got := sa.gates[admKey].inflight; got != 0 {
		t.Fatalf("inflight = %d after duplicate releases, want 0 (not negative)", got)
	}
	sa.mu.Unlock()

	// Behavior pin (not just state): the gate must not have widened.
	// At capacity 1, one caller admits and the next must QUEUE — an
	// underflowed inflight (-1 < 1 twice over) would admit both.
	rel2, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }
	blocked := make(chan struct{})
	go func() {
		rel3, err := sa.acquire(context.Background(), admKey)
		if err == nil {
			rel3()
		}
		close(blocked)
	}()
	select {
	case <-queuedC: // second caller queued: gate did not widen
	case <-blocked:
		t.Fatal("gate widened by duplicate release: second caller admitted at capacity 1")
	case <-time.After(admWaitTimeout):
		t.Fatal("second caller neither queued nor admitted")
	}
	rel2()
	select {
	case <-blocked:
	case <-time.After(admWaitTimeout):
		t.Fatal("queued caller never admitted after release")
	}
}

func TestPreCancelledContextNeverAdmits(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 4) // capacity free — the check, not the gate, must stop it
	sa := newSlotAdmission(src, make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sa.acquire(ctx, admKey); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	sa.mu.Lock()
	if g := sa.gates[admKey]; g != nil && (g.inflight != 0 || g.admitted != 0) {
		t.Fatalf("pre-cancelled caller touched the gate: inflight=%d admitted=%d",
			g.inflight, g.admitted)
	}
	sa.mu.Unlock()
}

func TestPreCancelledUngovernedStillPassesThrough(t *testing.T) {
	// Bit-identity pin (I2): an ungoverned caller with a dead context
	// must NOT be rejected by the admission layer — today it reaches the
	// provider and the provider handles the cancellation. The governance
	// check must therefore run before the dead-context check on entry.
	src := newAdmFakeSource() // admKey absent => ungoverned
	sa := newSlotAdmission(src, make(chan struct{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rel, err := sa.acquire(ctx, admKey)
	if err != nil {
		t.Fatalf("ungoverned pre-cancelled acquire err = %v, want nil (pass-through)", err)
	}
	rel()
	sa.mu.Lock()
	if len(sa.gates) != 0 {
		t.Fatalf("ungoverned pass-through created %d gate entries, want 0", len(sa.gates))
	}
	sa.mu.Unlock()
}

func TestCloseAndReleaseRaceYieldsErrRouterClosed(t *testing.T) {
	// I5 close/release race: a waiter woken by a release must observe a
	// concurrent Close at its next decision point and get ErrRouterClosed,
	// never a permit. The window (woken, not yet re-checked) cannot be hit
	// reliably by scheduling, so wokeHook parks the waiter inside it
	// deterministically: wake fires -> hook blocks -> test closes done ->
	// hook resumes -> the post-queue done-priority check must reject, even
	// though capacity is now free.
	src := newAdmFakeSource()
	src.set(admKey, 1)
	done := make(chan struct{})
	sa := newSlotAdmission(src, done)
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }
	wokeC := make(chan ModelKey, 1)
	resumeC := make(chan struct{})
	sa.wokeHook = func(k ModelKey) {
		wokeC <- k
		<-resumeC
	}

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	errC := make(chan error, 1)
	go func() {
		_, err := sa.acquire(context.Background(), admKey)
		errC <- err
	}()
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("waiter never queued")
	}
	rel1() // wake the waiter; it parks in wokeHook before re-checking
	select {
	case <-wokeC:
	case <-time.After(admWaitTimeout):
		t.Fatal("waiter never woke")
	}
	close(done)    // Close lands inside the wake-to-recheck window
	close(resumeC) // resume: next decision point must observe done first
	select {
	case err := <-errC:
		if !errors.Is(err, ErrRouterClosed) {
			t.Fatalf("raced waiter err = %v, want ErrRouterClosed (must not admit)", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("raced waiter never returned")
	}
	sa.mu.Lock()
	g := sa.gates[admKey]
	if g.inflight != 0 || g.rejected != 1 || g.waiting != 0 {
		t.Fatalf("inflight=%d rejected=%d waiting=%d, want 0/1/0", g.inflight, g.rejected, g.waiting)
	}
	sa.mu.Unlock()
}

func TestWaitingGaugeTracksBacklog(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	queuedC := make(chan ModelKey, 2)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	got2 := make(chan error, 1)
	go func() {
		_, err := sa.acquire(ctx2, admKey)
		got2 <- err
	}()
	admitted3 := make(chan func(), 1)
	go func() {
		rel, err := sa.acquire(context.Background(), admKey)
		if err == nil {
			admitted3 <- rel
		}
	}()
	for range 2 {
		select {
		case <-queuedC:
		case <-time.After(admWaitTimeout):
			t.Fatal("waiter never queued")
		}
	}
	waiting := func() int {
		sa.mu.Lock()
		defer sa.mu.Unlock()
		return sa.gates[admKey].waiting
	}
	if got := waiting(); got != 2 {
		t.Fatalf("waiting = %d with two queued, want 2", got)
	}
	cancel2()
	select {
	case <-got2:
	case <-time.After(admWaitTimeout):
		t.Fatal("cancelled waiter never returned")
	}
	if got := waiting(); got != 1 {
		t.Fatalf("waiting = %d after cancel, want 1", got)
	}
	rel1()
	var rel3 func()
	select {
	case rel3 = <-admitted3:
	case <-time.After(admWaitTimeout):
		t.Fatal("remaining waiter not admitted")
	}
	if got := waiting(); got != 0 {
		t.Fatalf("waiting = %d after admit, want 0", got)
	}
	rel3()
}

func TestCapacityRampAdmitsAllEligibleWaitersOnRelease(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	queuedC := make(chan ModelKey, 3)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	admitted := make(chan func(), 3)
	for range 3 {
		go func() {
			rel, err := sa.acquire(context.Background(), admKey)
			if err == nil {
				admitted <- rel
			}
		}()
	}
	for range 3 {
		select {
		case <-queuedC:
		case <-time.After(admWaitTimeout):
			t.Fatal("waiter never queued")
		}
	}
	// Raise capacity while all three wait; nothing wakes yet (ramp-up is
	// bounded by one in-flight completion, per the SlotSource contract).
	src.set(admKey, 4)
	rel1()
	// The single release must admit ALL waiters via re-read: 0 in flight,
	// capacity 4, three waiters -> three admissions.
	rels := make([]func(), 0, 3)
	for range 3 {
		select {
		case rel := <-admitted:
			rels = append(rels, rel)
		case <-time.After(admWaitTimeout):
			t.Fatalf("only %d of 3 waiters admitted after ramp release", len(rels))
		}
	}
	for _, rel := range rels {
		rel()
	}
}

func TestCapacityShrinkQueuesNewArrivalsWithoutPreemption(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 3)
	sa := newSlotAdmission(src, make(chan struct{}))
	var hookCount atomic.Int32
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) {
		hookCount.Add(1)
		queuedC <- k
	}

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	src.set(admKey, 1) // shrink under 2 in flight

	admittedC := make(chan struct{}, 1)
	go func() {
		rel, err := sa.acquire(context.Background(), admKey)
		if err == nil {
			admittedC <- struct{}{}
			rel()
		}
	}()
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("new arrival never queued under shrunk capacity")
	}
	rel1() // inflight 1, capacity 1: still full — waiter must NOT admit
	select {
	case <-admittedC:
		t.Fatal("waiter admitted while inflight >= shrunk capacity")
	case <-time.After(50 * time.Millisecond):
		// bounded liveness-negative probe after a sync point (house
		// pattern, dispatch_test), not an elapsed-time assertion
	}
	rel2() // inflight 0 < 1: now it admits
	select {
	case <-admittedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("waiter not admitted once inflight dropped below capacity")
	}
	// The waiter went through at least one wake+re-check loop (rel1)
	// before admitting (rel2). The hook must fire once per caller, on
	// first block only — a re-firing implementation reports 2+ here and
	// can deadlock tests whose hook channel is not drained mid-loop.
	if got := hookCount.Load(); got != 1 {
		t.Fatalf("queuedHook fired %d times for one caller, want exactly 1", got)
	}
}

func TestUngovernedKeyBypassesGateEntirely(t *testing.T) {
	src := newAdmFakeSource() // admKey absent => ungoverned
	sa := newSlotAdmission(src, make(chan struct{}))

	// Concurrency proof: many callers all "in flight" simultaneously with
	// no serialization — a barrier that only opens when all have acquired.
	const n = 8
	var inflight atomic.Int32
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := sa.acquire(context.Background(), admKey)
			if err != nil {
				t.Errorf("ungoverned acquire: %v", err)
				return
			}
			if inflight.Add(1) == n {
				close(barrier)
			}
			<-barrier // holds until ALL n are concurrently admitted
			rel()
		}()
	}
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	select {
	case <-wgDone:
	case <-time.After(admWaitTimeout):
		t.Fatal("ungoverned callers were serialized")
	}
	sa.mu.Lock()
	if len(sa.gates) != 0 {
		t.Fatalf("ungoverned traffic created %d gate entries, want 0", len(sa.gates))
	}
	sa.mu.Unlock()
}

func TestGovernanceFlipOffMidWaitExitsBalanced(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 1)
	sa := newSlotAdmission(src, make(chan struct{}))
	queuedC := make(chan ModelKey, 1)
	sa.queuedHook = func(k ModelKey) { queuedC <- k }

	rel1, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	resC := make(chan error, 1)
	go func() {
		rel, err := sa.acquire(context.Background(), admKey)
		if err == nil {
			rel()
		}
		resC <- err
	}()
	select {
	case <-queuedC:
	case <-time.After(admWaitTimeout):
		t.Fatal("waiter never queued")
	}
	src.drop(admKey) // governance off while queued
	rel1()           // wake -> re-check sees ok=false -> no-permit success
	select {
	case err := <-resC:
		if err != nil {
			t.Fatalf("flipped-ungoverned waiter err = %v, want nil", err)
		}
	case <-time.After(admWaitTimeout):
		t.Fatal("flipped-ungoverned waiter never returned")
	}
	sa.mu.Lock()
	g := sa.gates[admKey]
	if g.inflight != 0 || g.waiting != 0 {
		t.Fatalf("inflight=%d waiting=%d, want 0/0 (waiter must not have incremented inflight, and must have left the waiting gauge)", g.inflight, g.waiting)
	}
	// I7 documented edge: this pass is neither admitted nor rejected.
	if g.admitted != 1 || g.rejected != 0 {
		t.Fatalf("admitted=%d rejected=%d, want 1/0 (governance-flip pass counts as neither)", g.admitted, g.rejected)
	}
	sa.mu.Unlock()
}

func TestContractViolatingCapacityClampsToSerial(t *testing.T) {
	src := newAdmFakeSource()
	src.set(admKey, 0) // ok=true, n=0: violates SlotSource contract
	sa := newSlotAdmission(src, make(chan struct{}))
	rel, err := sa.acquire(context.Background(), admKey)
	if err != nil {
		t.Fatalf("acquire under clamped capacity: %v", err)
	}
	rel()
}
