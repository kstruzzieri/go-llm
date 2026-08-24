package configio

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestIntegration_RefreshFiresNoToolCallProbes(t *testing.T) {
	store := newMemCapProbeStore()
	prober := newCountingProber()
	prober.outcomes["mystery"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	reg, mr := newRealRegistry(t, []*fakeProvider{
		{name: "prov", models: []provider.ModelInfo{{Name: "mystery"}}},
	}, store, prober)

	inv, err := RefreshInventory(context.Background(), reg, mr)
	if err != nil {
		t.Fatalf("RefreshInventory() error: %v", err)
	}
	if len(inv.Models) != 1 || inv.Models[0].Key != key("prov", "mystery") {
		t.Fatalf("Models = %+v; want exactly prov/mystery", inv.Models)
	}
	if len(inv.Providers) != 1 || !inv.Providers[0].Reachable {
		t.Fatalf("Providers = %+v; want reachable prov", inv.Providers)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("refresh fired %d tool-call probe calls; refresh must NEVER probe", got)
	}
	if store.count() != 0 {
		t.Fatalf("refresh persisted %d probe rows; want 0", store.count())
	}

	// POSITIVE CONTROL: an explicit ProbeToolCall DOES fire the prober.
	if _, err := ProbeToolCall(context.Background(), mr, key("prov", "mystery")); err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if got := prober.totalCalls(); got == 0 {
		t.Fatal("positive control failed: explicit probe fired no prober calls; the zero-assertion above is blind")
	}
}

func TestIntegration_CancelledProbeCallerReceivesNothing(t *testing.T) {
	// The #456 promise: a cancelled caller receives nothing. This fixture
	// uses a ctx-respecting prober, so the interrupted probe additionally
	// persists nothing; a ctx-IGNORING prober completing after cancel may
	// persist into the shared cache (documented registry semantics) — the
	// caller-receives-nothing guarantee holds regardless.
	store := newMemCapProbeStore()
	prober := newCountingProber()
	prober.block = make(chan struct{})
	defer close(prober.block)
	_, mr := newRealRegistry(t, []*fakeProvider{
		{name: "prov", models: []provider.ModelInfo{{Name: "mystery"}}},
	}, store, prober)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out ProbeOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := ProbeToolCall(ctx, mr, key("prov", "mystery"))
		done <- result{out, err}
	}()

	select {
	case <-prober.started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never started")
	}
	cancel()

	var res result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeToolCall did not return after cancellation")
	}

	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", res.err)
	}
	if code, ok := CodeOf(res.err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if res.out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; a cancelled probe must publish nothing", res.out)
	}
	if store.count() != 0 {
		t.Fatalf("interrupted probe persisted %d rows; want 0", store.count())
	}
}

func TestIntegration_ProbeVerdictSticksAcrossRegistryLifetimes(t *testing.T) {
	// The sticky contract (firn-ide#263): probed verdicts persist via the
	// store and are visible to a FRESH registry's refresh — yes AND no —
	// without re-probing. Bounded by the digestless-no TTL (7 days).
	store := newMemCapProbeStore()
	prober := newCountingProber()
	prober.outcomes["says-yes"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober.outcomes["says-no"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	mkProviders := func() []*fakeProvider {
		return []*fakeProvider{
			{name: "prov", models: []provider.ModelInfo{{Name: "says-no"}, {Name: "says-yes"}}},
		}
	}
	_, mr1 := newRealRegistry(t, mkProviders(), store, prober)

	ctx := context.Background()
	if out, err := ProbeToolCall(ctx, mr1, key("prov", "says-yes")); err != nil || out.State != fingerprint.CapProbeYes || !out.Persisted {
		t.Fatalf("probe says-yes = %+v, %v; want {yes, Persisted:true}, nil", out, err)
	}
	if out, err := ProbeToolCall(ctx, mr1, key("prov", "says-no")); err != nil || out.State != fingerprint.CapProbeNo || !out.Persisted {
		t.Fatalf("probe says-no = %+v, %v; want {no, Persisted:true}, nil", out, err)
	}
	if store.count() != 2 {
		t.Fatalf("store rows = %d; want 2 persisted verdicts", store.count())
	}

	// Session boundary: brand-new registry, provider, AND prober objects
	// sharing only the store. No new probe calls may fire.
	prober2 := newCountingProber()
	reg2, mr2 := newRealRegistry(t, mkProviders(), store, prober2)
	inv, err := RefreshInventory(ctx, reg2, mr2)
	if err != nil {
		t.Fatalf("second-session RefreshInventory() error: %v", err)
	}
	if got := prober2.totalCalls(); got != 0 {
		t.Fatalf("second-session refresh fired %d probe calls; want 0", got)
	}
	if len(inv.Models) != 2 {
		t.Fatalf("second-session Models = %+v; want exactly 2", inv.Models)
	}

	byModel := map[string]int{}
	for i, m := range inv.Models {
		byModel[m.Key.Model] = i
	}
	if len(byModel) != 2 {
		t.Fatalf("distinct models = %d; want 2", len(byModel))
	}
	yesIdx, ok := byModel["says-yes"]
	if !ok {
		t.Fatalf("says-yes missing from second-session inventory: %+v", inv.Models)
	}
	yes := inv.Models[yesIdx]
	if !yes.Caps.Has(provider.CapToolCall) || !yes.KnownMask.Has(provider.CapToolCall) {
		t.Fatalf("says-yes after restart: Caps=%v KnownMask=%v; want tool_call set in both", yes.Caps, yes.KnownMask)
	}
	noIdx, ok := byModel["says-no"]
	if !ok {
		t.Fatalf("says-no missing from second-session inventory: %+v", inv.Models)
	}
	no := inv.Models[noIdx]
	if no.Caps.Has(provider.CapToolCall) {
		t.Fatalf("says-no after restart: Caps=%v; tool_call bit must stay clear", no.Caps)
	}
	if !no.KnownMask.Has(provider.CapToolCall) {
		t.Fatalf("says-no after restart: KnownMask=%v; persisted probe-no must be KNOWN-no, not unknown", no.KnownMask)
	}
}

func TestIntegration_StaleCachedProbeBitIsNotDurable(t *testing.T) {
	store := newMemCapProbeStore()
	prober := newCountingProber()
	prober.outcomes["m"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	p := &fakeProvider{name: "prov", models: []provider.ModelInfo{{Name: "m", Digest: "old"}}}
	_, mr := newRealRegistry(t, []*fakeProvider{p}, store, prober)

	if out, err := ProbeToolCall(context.Background(), mr, key("prov", "m")); err != nil || !out.Persisted {
		t.Fatalf("ProbeToolCall(prov/m) = %+v, %v; want persisted initial verdict", out, err)
	}
	if profile, err := mr.Lookup(context.Background(), key("prov", "m")); err != nil || !profile.Caps.Has(provider.CapToolCall) {
		t.Fatalf("Lookup(prov/m) = %+v, %v; want cached probe bit", profile, err)
	}

	p.models[0].Digest = "new"
	out, err := ProbeToolCall(context.Background(), mr, key("prov", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall(prov/m) error = %v, want nil", err)
	}
	if out.State != fingerprint.CapProbeYes || out.Persisted {
		t.Fatalf("ProbeToolCall(prov/m) = %+v, want {State:yes Persisted:false} after digest change", out)
	}
}

func TestIntegration_StaleCachedRuntimeBitIsNotDurable(t *testing.T) {
	p := &fakeProvider{name: "prov", models: []provider.ModelInfo{{
		Name:         "m",
		Capabilities: []string{"tools"},
	}}}
	_, mr := newRealRegistry(t, []*fakeProvider{p}, nil, nil)

	if out, err := ProbeToolCall(context.Background(), mr, key("prov", "m")); err != nil || !out.Persisted {
		t.Fatalf("ProbeToolCall(prov/m) = %+v, %v; want persisted runtime verdict", out, err)
	}

	p.models[0].Capabilities = nil
	out, err := ProbeToolCall(context.Background(), mr, key("prov", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall(prov/m) error = %v, want nil", err)
	}
	if out.State != fingerprint.CapProbeYes || out.Persisted {
		t.Fatalf("ProbeToolCall(prov/m) = %+v, want {State:yes Persisted:false} after runtime capability removal", out)
	}
}

func TestIntegration_InvalidProbeStateFailsWithoutPersistence(t *testing.T) {
	tests := []struct {
		name  string
		state fingerprint.CapProbeState
	}{
		{name: "empty", state: ""},
		{name: "unknown", state: "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemCapProbeStore()
			prober := newCountingProber()
			prober.outcomes["m"] = fingerprint.CapProbeOutcome{State: tt.state}
			_, mr := newRealRegistry(t, []*fakeProvider{
				{name: "prov", models: []provider.ModelInfo{{Name: "m"}}},
			}, store, prober)

			out, err := ProbeToolCall(context.Background(), mr, key("prov", "m"))
			if code, ok := CodeOf(err); !ok || code != CodeProbeFailed {
				t.Fatalf("CodeOf(ProbeToolCall(prov/m)) = %q, %v; want %q, true", code, ok, CodeProbeFailed)
			}
			if out != (ProbeOutcome{}) {
				t.Fatalf("ProbeToolCall(prov/m) outcome = %+v, want zero", out)
			}
			if store.count() != 0 {
				t.Fatalf("invalid probe state %q persisted %d rows, want 0", tt.state, store.count())
			}
		})
	}
}

func TestIntegration_InvalidCachedProbeStateIsIgnored(t *testing.T) {
	store := newMemCapProbeStore()
	ctx := context.Background()
	if err := store.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID:    "prov",
		ModelName:    "m",
		Capability:   "tool_call",
		State:        "bogus",
		ModelDigest:  key("prov", "m").String(),
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SaveCapProbe(invalid row) error = %v", err)
	}
	prober := newCountingProber()
	prober.outcomes["m"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	_, mr := newRealRegistry(t, []*fakeProvider{
		{name: "prov", models: []provider.ModelInfo{{Name: "m"}}},
	}, store, prober)

	out, err := ProbeToolCall(ctx, mr, key("prov", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall(prov/m) error = %v, want nil", err)
	}
	if out.State != fingerprint.CapProbeYes || !out.Persisted {
		t.Fatalf("ProbeToolCall(prov/m) = %+v, want {State:yes Persisted:true}", out)
	}
	if got := prober.totalCalls(); got != 1 {
		t.Fatalf("ProbeToolCall(prov/m) prober calls = %d, want 1", got)
	}
}

func TestIntegration_LostSaveReportsPersistedFalse(t *testing.T) {
	// The registry swallows SaveCapProbe errors (documented best-effort);
	// D9's outcome surfaces it: verdict returned, Persisted=false, no error.
	store := newMemCapProbeStore()
	store.saveErr = errors.New("disk full")
	prober := newCountingProber()
	prober.outcomes["m"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	_, mr := newRealRegistry(t, []*fakeProvider{
		{name: "prov", models: []provider.ModelInfo{{Name: "m"}}},
	}, store, prober)

	out, err := ProbeToolCall(context.Background(), mr, key("prov", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v; a lost save is a warning, not a failure", err)
	}
	if out.State != fingerprint.CapProbeNo {
		t.Fatalf("state = %q; want no (the verdict stands for this session)", out.State)
	}
	if out.Persisted {
		t.Fatal("Persisted = true despite failing store; the dual outcome must report the lost write")
	}
}
