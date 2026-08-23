// provider/capresolve_parallel_test.go
//
// Serial-equivalence pins and bounded-fan-out tests for
// EnsureToolCallResolved (#401). The pins characterize the observable
// contract the parallel implementation must preserve: candidate order,
// pointer behavior, input immutability, and per-occurrence diagnostic
// ordering. Serial equivalence deliberately EXCLUDES probe timing/counts
// and duplicate transient retries (one resolution per distinct key per
// invocation).
package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// gatedToolCallProber blocks each model's ProbeToolCall on its own gate
// and records start order. A nil gate for a model means no blocking.
type gatedToolCallProber struct {
	*fakeToolCallProber
	gateMu  sync.Mutex
	gates   map[string]chan struct{}
	started []string
}

func newGatedToolCallProber() *gatedToolCallProber {
	return &gatedToolCallProber{
		fakeToolCallProber: newFakeToolCallProber(),
		gates:              make(map[string]chan struct{}),
	}
}

func (p *gatedToolCallProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.gateMu.Lock()
	p.started = append(p.started, model)
	gate := p.gates[model]
	p.gateMu.Unlock()
	if gate != nil {
		<-gate
	}
	return p.fakeToolCallProber.ProbeToolCall(ctx, model)
}

func (p *gatedToolCallProber) startedModels() []string {
	p.gateMu.Lock()
	defer p.gateMu.Unlock()
	return append([]string(nil), p.started...)
}

// ---------------------------------------------------------------------------
// Serial-equivalence pins
// ---------------------------------------------------------------------------

func TestEnsureToolCallResolved_MixedOutcomePointerPins(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["m-yes"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober.outcomes["m-no"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	prober.outcomes["m-inc"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeInconclusive}
	probeErr := errors.New("backend 401")
	prober.errs["m-err"] = probeErr
	models := []string{"m-yes", "m-no", "m-inc", "m-err"}
	mr := newCapResolveRegistry(t, "probe-prov", models, store, prober)

	lookup := func(model string) *ModelProfile {
		t.Helper()
		p, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: model})
		if err != nil {
			t.Fatalf("Lookup(%s) error: %v", model, err)
		}
		return p
	}
	capable := &ModelProfile{Key: ModelKey{Provider: "probe-prov", Model: "m-capable"}, Caps: CapChat | CapToolCall}
	in := []*ModelProfile{lookup("m-yes"), nil, capable, lookup("m-no"), lookup("m-inc"), lookup("m-err")}
	inCopy := append([]*ModelProfile(nil), in...)
	inCaps := make([]Capability, len(in))
	for i, p := range in {
		if p != nil {
			inCaps[i] = p.Caps
		}
	}

	out, diags := mr.EnsureToolCallResolved(ctx, in, CapChat|CapToolCall)

	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	// probe-yes: replaced by the registry's refreshed cached profile --
	// different from the input pointer, identical to a fresh Lookup.
	if out[0] == in[0] {
		t.Fatal("out[0] is the input pointer, want the refreshed profile")
	}
	refreshed := lookup("m-yes")
	if out[0] != refreshed {
		t.Fatalf("out[0] = %p, want the registry's cached refreshed profile %p", out[0], refreshed)
	}
	if !out[0].Caps.Has(CapToolCall) {
		t.Fatalf("out[0].Caps = %v, want tool_call", out[0].Caps)
	}
	// nil, already-capable, no, inconclusive, error: exact original pointer.
	for _, i := range []int{1, 2, 3, 4, 5} {
		if out[i] != in[i] {
			t.Fatalf("out[%d] = %p, want exact input pointer %p", i, out[i], in[i])
		}
	}
	// Input slice and input profile values unchanged.
	for i := range in {
		if in[i] != inCopy[i] {
			t.Fatalf("input slice entry %d mutated", i)
		}
		if in[i] != nil && in[i].Caps != inCaps[i] {
			t.Fatalf("input profile %d Caps mutated: %v -> %v", i, inCaps[i], in[i].Caps)
		}
	}
	// Exactly one diagnostic, labeled with the model key, wrapping the
	// probe error.
	if len(diags) != 1 {
		t.Fatalf("diags = %v, want exactly 1", diags)
	}
	want := fmt.Sprintf("resolve tool_call %s: %s", in[5].Key, probeErr)
	if diags[0].Error() != want {
		t.Fatalf("diag = %q, want %q", diags[0].Error(), want)
	}
	if !errors.Is(diags[0], probeErr) {
		t.Fatalf("diag does not wrap the probe error: %v", diags[0])
	}
}

func TestEnsureToolCallResolved_DiagOrderFollowsCandidateOrder(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["ok"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	for _, m := range []string{"e1", "e2", "e3"} {
		prober.errs[m] = fmt.Errorf("fail %s", m)
	}
	models := []string{"e1", "ok", "e2", "e3"}
	mr := newCapResolveRegistry(t, "probe-prov", models, store, prober)

	var in []*ModelProfile
	for _, m := range models {
		p, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: m})
		if err != nil {
			t.Fatalf("Lookup(%s) error: %v", m, err)
		}
		in = append(in, p)
	}

	_, diags := mr.EnsureToolCallResolved(ctx, in, CapToolCall)
	if len(diags) != 3 {
		t.Fatalf("diags = %v, want 3", diags)
	}
	for i, m := range []string{"e1", "e2", "e3"} {
		want := fmt.Sprintf("resolve tool_call probe-prov/%s: fail %s", m, m)
		if diags[i].Error() != want {
			t.Fatalf("diags[%d] = %q, want %q (candidate index order)", i, diags[i].Error(), want)
		}
	}
}

func TestEnsureToolCallResolved_LostStoreWritePatchesCopy(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	store.saveErr = errors.New("disk full")
	prober := newFakeToolCallProber()
	prober.outcomes["m-yes"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"m-yes"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "m-yes"}

	orig, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	origCaps := orig.Caps

	out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{orig}, CapToolCall)
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none (lost write is non-fatal)", diags)
	}
	if out[0] == orig {
		t.Fatal("out[0] is the input pointer, want a patched copy")
	}
	if !out[0].Caps.Has(CapToolCall) {
		t.Fatalf("out[0].Caps = %v, want tool_call patched in", out[0].Caps)
	}
	if orig.Caps != origCaps {
		t.Fatalf("input profile mutated: %v -> %v", origCaps, orig.Caps)
	}
	// The store had no row to merge, so a fresh Lookup still lacks the
	// bit -- the patch covered this caller only.
	relooked, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("re-Lookup() error: %v", err)
	}
	if relooked.Caps.Has(CapToolCall) {
		t.Fatalf("re-Lookup Caps = %v, want tool_call still absent", relooked.Caps)
	}
}

func TestEnsureToolCallResolved_AliasingPins(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "probe-prov", []string{"m"}, store, prober)
	capable := &ModelProfile{Key: ModelKey{Provider: "probe-prov", Model: "m-capable"}, Caps: CapChat | CapToolCall}

	t.Run("zero unresolved returns clone", func(t *testing.T) {
		in := []*ModelProfile{capable, nil}
		out, diags := mr.EnsureToolCallResolved(ctx, in, CapToolCall)
		if len(diags) != 0 {
			t.Fatalf("diags = %v, want none", diags)
		}
		if out[0] != in[0] || out[1] != in[1] {
			t.Fatal("clone must preserve element pointers")
		}
		if &out[0] == &in[0] {
			t.Fatal("enabled path must return a fresh slice, not the input")
		}
	})

	t.Run("disabled resolution returns input as-is", func(t *testing.T) {
		mrOff := newCapResolveRegistry(t, "probe-prov", []string{"m"}, nil, nil)
		in := []*ModelProfile{capable}
		out, _ := mrOff.EnsureToolCallResolved(ctx, in, CapToolCall)
		if &out[0] != &in[0] {
			t.Fatal("disabled path must return the input slice unchanged")
		}
	})

	t.Run("tool_call not required returns input as-is", func(t *testing.T) {
		in := []*ModelProfile{capable}
		out, _ := mr.EnsureToolCallResolved(ctx, in, CapChat)
		if &out[0] != &in[0] {
			t.Fatal("no-required path must return the input slice unchanged")
		}
	})
}

// ---------------------------------------------------------------------------
// Bounded fan-out
// ---------------------------------------------------------------------------

func TestEnsureToolCallResolved_BoundedFanOutAcrossDistinctKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := newFakeCapProbeStore()
		prober := newGatedToolCallProber()
		models := []string{"m1", "m2", "m3", "m4", "m5"}
		for _, m := range models {
			prober.outcomes[m] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
			prober.gates[m] = make(chan struct{})
		}
		mr := newCapResolveRegistry(t, "probe-prov", models, store, prober)

		var in []*ModelProfile
		for _, m := range models {
			p, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: m})
			if err != nil {
				t.Fatalf("Lookup(%s) error: %v", m, err)
			}
			in = append(in, p)
		}

		type result struct {
			out   []*ModelProfile
			diags []error
		}
		done := make(chan result, 1)
		go func() {
			out, diags := mr.EnsureToolCallResolved(ctx, in, CapToolCall)
			done <- result{out, diags}
		}()

		// With five distinct cold keys, exactly four probes may be in
		// flight (literal bound: an accidental policy change must fail
		// here). The fifth is durably blocked behind the limit.
		synctest.Wait()
		if got := prober.startedModels(); len(got) != 4 {
			t.Fatalf("probes in flight = %d (%v), want exactly 4", len(got), got)
		}

		// Releasing one gate frees a worker slot; the fifth key starts.
		close(prober.gates[prober.startedModels()[0]])
		synctest.Wait()
		if got := prober.startedModels(); len(got) != 5 {
			t.Fatalf("probes started after one release = %d (%v), want 5", len(got), got)
		}

		for _, m := range models {
			gate := prober.gates[m]
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
		res := <-done
		if len(res.diags) != 0 {
			t.Fatalf("diags = %v, want none", res.diags)
		}
		for i := range res.out {
			if !res.out[i].Caps.Has(CapToolCall) {
				t.Fatalf("out[%d].Caps = %v, want tool_call", i, res.out[i].Caps)
			}
		}
	})
}

func TestEnsureToolCallResolved_DuplicatesDoNotConsumeFanOutSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := newFakeCapProbeStore()
		prober := newGatedToolCallProber()
		for _, m := range []string{"dup", "other"} {
			prober.outcomes[m] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
			prober.gates[m] = make(chan struct{})
		}
		mr := newCapResolveRegistry(t, "probe-prov", []string{"dup", "other"}, store, prober)

		dupProfile, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: "dup"})
		if err != nil {
			t.Fatalf("Lookup(dup) error: %v", err)
		}
		otherProfile, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: "other"})
		if err != nil {
			t.Fatalf("Lookup(other) error: %v", err)
		}
		// Five occurrences of dup ahead of other: per-candidate jobs
		// would fill every one of the four worker slots with dup
		// occurrences and leave other unstarted.
		in := []*ModelProfile{dupProfile, dupProfile, dupProfile, dupProfile, dupProfile, otherProfile}

		done := make(chan struct{})
		go func() {
			mr.EnsureToolCallResolved(ctx, in, CapToolCall)
			close(done)
		}()

		synctest.Wait()
		started := prober.startedModels()
		if len(started) != 2 {
			t.Fatalf("probes in flight = %v, want both distinct keys concurrently", started)
		}
		seen := map[string]bool{started[0]: true, started[1]: true}
		if !seen["dup"] || !seen["other"] {
			t.Fatalf("probes in flight = %v, want dup and other", started)
		}

		close(prober.gates["dup"])
		close(prober.gates["other"])
		<-done
		if got := prober.callCount("dup"); got != 1 {
			t.Fatalf("dup probes = %d, want 1 (one resolution per distinct key)", got)
		}
	})
}

func TestEnsureToolCallResolved_DuplicateTransientSharedThenRetriedNextCall(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.errs["a"] = errors.New("transient a")
	prober.errs["b"] = errors.New("transient b")
	mr := newCapResolveRegistry(t, "probe-prov", []string{"a", "b"}, store, prober)

	pa, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: "a"})
	if err != nil {
		t.Fatalf("Lookup(a) error: %v", err)
	}
	pb, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: "b"})
	if err != nil {
		t.Fatalf("Lookup(b) error: %v", err)
	}
	in := []*ModelProfile{pa, pb, pa}

	_, diags := mr.EnsureToolCallResolved(ctx, in, CapToolCall)
	if got := prober.callCount("a"); got != 1 {
		t.Fatalf("first call: a probes = %d, want 1 (duplicates share one resolution)", got)
	}
	if got := prober.callCount("b"); got != 1 {
		t.Fatalf("first call: b probes = %d, want 1", got)
	}
	// Diagnostics per occurrence, input order: a, b, a.
	if len(diags) != 3 {
		t.Fatalf("diags = %v, want 3 (per occurrence)", diags)
	}
	for i, m := range []string{"a", "b", "a"} {
		want := fmt.Sprintf("resolve tool_call probe-prov/%s: transient %s", m, m)
		if diags[i].Error() != want {
			t.Fatalf("diags[%d] = %q, want %q", i, diags[i].Error(), want)
		}
	}

	// Transient failures are not persisted; the NEXT invocation retries
	// each failed key exactly once.
	_, _ = mr.EnsureToolCallResolved(ctx, in, CapToolCall)
	if got := prober.callCount("a"); got != 2 {
		t.Fatalf("second call: a probes = %d, want 2", got)
	}
	if got := prober.callCount("b"); got != 2 {
		t.Fatalf("second call: b probes = %d, want 2", got)
	}
}

func TestEnsureToolCallResolved_DistinctNULKeysDoNotShareFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newFakeCapProbeStore()
		prober := newGatedToolCallProber()
		prober.outcomes["\x00b"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		prober.outcomes["b"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
		prober.gates["\x00b"] = make(chan struct{})
		prober.gates["b"] = make(chan struct{})

		reg := &mrMockProviderRegistry{providers: map[string]Provider{
			"a":     &mrMockProvider{name: "a", models: []ModelInfo{{Name: "\x00b"}}},
			"a\x00": &mrMockProvider{name: "a\x00", models: []ModelInfo{{Name: "b"}}},
		}}
		mr, err := NewModelRegistry(reg, nil,
			WithCapabilityProbeStore(store),
			WithCapabilityProber(capProberFactory(prober)),
		)
		if err != nil {
			t.Fatalf("NewModelRegistry() error: %v", err)
		}

		keys := []ModelKey{
			{Provider: "a", Model: "\x00b"},
			{Provider: "a\x00", Model: "b"},
		}
		profiles := make([]*ModelProfile, len(keys))
		for i, key := range keys {
			profiles[i], err = mr.Lookup(context.Background(), key)
			if err != nil {
				t.Fatalf("Lookup(%q) error: %v", key, err)
			}
		}

		done := make(chan []*ModelProfile, 1)
		go func() {
			out, _ := mr.EnsureToolCallResolved(context.Background(), profiles, CapToolCall)
			done <- out
		}()
		synctest.Wait()
		close(prober.gates["\x00b"])
		close(prober.gates["b"])
		out := <-done

		if got := prober.totalCalls(); got != 2 {
			t.Fatalf("probe calls = %d, want 2 for distinct model keys", got)
		}
		if !out[0].Caps.Has(CapToolCall) {
			t.Fatalf("out[0].Caps = %v, want tool_call", out[0].Caps)
		}
		if out[1].Caps.Has(CapToolCall) {
			t.Fatalf("out[1].Caps = %v, want no tool_call", out[1].Caps)
		}
	})
}
