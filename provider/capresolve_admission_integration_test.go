// provider/capresolve_admission_integration_test.go
//
// End-to-end #401 tests through a REAL Router-owned slotAdmission gate
// (fake SlotSource, real NewRouter wiring): queued-then-admitted
// counters, cancel-while-queued, Router-close-while-queued, and the
// cross-invocation same-key proof that permit acquisition lives inside
// the singleflight leader.
package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// admissionRow finds key's row in a SlotAdmissionSnapshot.
func admissionRow(t *testing.T, r *Router, key ModelKey) SlotAdmissionInfo {
	t.Helper()
	for _, row := range r.SlotAdmissionSnapshot() {
		if row.Provider == key.Provider && row.Model == key.Model {
			return row
		}
	}
	t.Fatalf("no admission row for %v in %+v", key, r.SlotAdmissionSnapshot())
	return SlotAdmissionInfo{}
}

func TestResolveToolCall_QueuedThenAdmittedThroughRealGate(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	src := &fakeSlotSource{capacity: 1, governed: true}
	router := NewRouter(mr, NewRegistry(), WithSlotSource(src))
	cleanupRouter(t, router)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	queued := make(chan ModelKey, 1)
	router.admission.queuedHook = func(k ModelKey) { queued <- k }

	release, err := router.acquireSlot(ctx, key) // hold the only permit
	if err != nil {
		t.Fatalf("manual acquireSlot() error: %v", err)
	}

	type result struct {
		state fingerprint.CapProbeState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		state, rErr := mr.ResolveToolCall(ctx, key)
		done <- result{state, rErr}
	}()

	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not queue behind the held permit")
	}
	release()

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ResolveToolCall did not finish after release")
	}
	if got.err != nil || got.state != fingerprint.CapProbeYes {
		t.Fatalf("ResolveToolCall() = %q, %v; want yes, nil", got.state, got.err)
	}

	row := admissionRow(t, router, key)
	if row.Admitted != 2 || row.Queued != 1 || row.Rejected != 0 || row.InFlight != 0 || row.Waiting != 0 {
		t.Fatalf("admission row = %+v, want Admitted=2 Queued=1 Rejected=0 InFlight=0 Waiting=0", row)
	}
}

func TestEnsureToolCallResolved_CancelWhileQueuedProbesNothing(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	src := &fakeSlotSource{capacity: 1, governed: true}
	router := NewRouter(mr, NewRegistry(), WithSlotSource(src))
	cleanupRouter(t, router)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	queued := make(chan ModelKey, 1)
	router.admission.queuedHook = func(k ModelKey) { queued <- k }

	release, err := router.acquireSlot(ctx, key)
	if err != nil {
		t.Fatalf("manual acquireSlot() error: %v", err)
	}
	defer release()

	orig, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	type result struct {
		out   []*ModelProfile
		diags []error
	}
	done := make(chan result, 1)
	go func() {
		out, diags := mr.EnsureToolCallResolved(cctx, []*ModelProfile{orig}, CapToolCall)
		done <- result{out, diags}
	}()

	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not queue behind the held permit")
	}
	cancel()

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureToolCallResolved did not finish after cancel")
	}
	if got.out[0] != orig {
		t.Fatalf("out[0] = %p, want unchanged input pointer %p", got.out[0], orig)
	}
	if len(got.diags) != 1 || !errors.Is(got.diags[0], context.Canceled) {
		t.Fatalf("diags = %v, want one diagnostic wrapping context.Canceled", got.diags)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0 (canceled before admission)", got)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("store rows = %d, want 0", got)
	}
	row := admissionRow(t, router, key)
	if row.Rejected != 1 || row.Waiting != 0 {
		t.Fatalf("admission row = %+v, want Rejected=1 Waiting=0", row)
	}
}

func TestEnsureToolCallResolved_RouterCloseWhileQueued(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	src := &fakeSlotSource{capacity: 1, governed: true}
	router := NewRouter(mr, NewRegistry(), WithSlotSource(src))
	cleanupRouter(t, router)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	queued := make(chan ModelKey, 1)
	router.admission.queuedHook = func(k ModelKey) { queued <- k }

	release, err := router.acquireSlot(ctx, key)
	if err != nil {
		t.Fatalf("manual acquireSlot() error: %v", err)
	}
	defer release()

	orig, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	type result struct {
		out   []*ModelProfile
		diags []error
	}
	done := make(chan result, 1)
	go func() {
		out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{orig}, CapToolCall)
		done <- result{out, diags}
	}()

	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not queue behind the held permit")
	}
	if err := router.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureToolCallResolved did not finish after close")
	}
	if got.out[0] != orig {
		t.Fatalf("out[0] = %p, want unchanged input pointer %p", got.out[0], orig)
	}
	if len(got.diags) != 1 || !errors.Is(got.diags[0], ErrRouterClosed) {
		t.Fatalf("diags = %v, want one diagnostic wrapping ErrRouterClosed", got.diags)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("store rows = %d, want 0", got)
	}
}

func TestRoute_CloseMidProbeClassifiesNoViableWithRouterClosedText(t *testing.T) {
	ctx := context.Background()
	provReg := NewRegistry()
	if err := provReg.Register(&fakeChainProvider{name: "p", models: []string{"a"}}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["a"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr, err := NewModelRegistry(provReg, nil,
		WithCapabilityProbeStore(store),
		WithCapabilityProber(capProberFactory(prober)),
	)
	if err != nil {
		t.Fatalf("new model registry: %v", err)
	}
	seedChainProfile(t, mr, &ModelProfile{
		Key:           ModelKey{Provider: "p", Model: "a"},
		Caps:          CapChat | CapGenerate | CapStream,
		Quality:       TierGood,
		Speed:         TierGood,
		ContextWindow: 8192,
	})
	if err := provReg.RefreshModels(ctx, "p"); err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	src := &fakeSlotSource{capacity: 1, governed: true}
	router := NewRouter(mr, provReg, WithSlotSource(src))
	cleanupRouter(t, router)
	key := ModelKey{Provider: "p", Model: "a"}

	queued := make(chan ModelKey, 1)
	router.admission.queuedHook = func(k ModelKey) { queued <- k }

	release, aErr := router.acquireSlot(ctx, key)
	if aErr != nil {
		t.Fatalf("manual acquireSlot() error: %v", aErr)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, rErr := router.Route(ctx, RoutingRequest{
			UseCase:        "chat",
			RequiredCaps:   CapChat | CapToolCall,
			PreferredChain: []string{"p/a"},
			StrictChain:    true,
			Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
		})
		done <- rErr
	}()

	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not queue behind the held permit")
	}
	if err := router.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	var routeErr error
	select {
	case routeErr = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Route did not finish after close")
	}
	// The caller's ctx is alive, so close-mid-probe stays a routing
	// verdict: no-viable-candidate whose stringified diagnostics carry
	// the router-closed text. Diagnostics remain %s-joined, so the
	// route error deliberately does NOT errors.Is-match ErrRouterClosed.
	if !errors.Is(routeErr, ErrNoViableCandidate) {
		t.Fatalf("Route() error = %v, want ErrNoViableCandidate", routeErr)
	}
	if !strings.Contains(routeErr.Error(), ErrRouterClosed.Error()) {
		t.Fatalf("Route() error %q does not carry the router-closed diagnostic", routeErr)
	}
	if errors.Is(routeErr, ErrRouterClosed) {
		t.Fatalf("Route() error = %v, diagnostics must stay stringified", routeErr)
	}
}

func TestResolveToolCall_CrossInvocationSameKeyLeaderHoldsOnePermit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := newFakeCapProbeStore()
		prober := newGatedToolCallProber()
		prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		prober.gates["mystery-model"] = make(chan struct{})
		mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
		src := &fakeSlotSource{capacity: 1, governed: true}
		router := NewRouter(mr, NewRegistry(), WithSlotSource(src))
		cleanupRouter(t, router)
		key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

		orig, err := mr.Lookup(ctx, key)
		if err != nil {
			t.Fatalf("Lookup() error: %v", err)
		}

		type result struct {
			out   []*ModelProfile
			diags []error
		}
		results := make(chan result, 2)
		invoke := func() {
			out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{orig}, CapToolCall)
			results <- result{out, diags}
		}

		go invoke()
		synctest.Wait() // leader holds the permit, blocked in the probe gate
		go invoke()
		synctest.Wait() // second invocation durably blocked -- must be a
		// singleflight follower, NOT an admission waiter

		row := admissionRow(t, router, key)
		if row.Admitted != 1 || row.Queued != 0 || row.Waiting != 0 || row.InFlight != 1 {
			t.Fatalf("mid-flight admission row = %+v, want Admitted=1 Queued=0 Waiting=0 InFlight=1 (follower must not touch the gate)", row)
		}

		close(prober.gates["mystery-model"])
		for range 2 {
			res := <-results
			if len(res.diags) != 0 {
				t.Fatalf("diags = %v, want none", res.diags)
			}
			if !res.out[0].Caps.Has(CapToolCall) {
				t.Fatalf("out[0].Caps = %v, want tool_call", res.out[0].Caps)
			}
		}
		if got := prober.callCount("mystery-model"); got != 1 {
			t.Fatalf("probe count = %d, want 1 (one leader)", got)
		}
		row = admissionRow(t, router, key)
		if row.Admitted != 1 || row.InFlight != 0 {
			t.Fatalf("final admission row = %+v, want Admitted=1 InFlight=0", row)
		}
	})
}
