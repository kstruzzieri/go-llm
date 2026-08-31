// provider/router_chain_parallel_test.go
//
// Chain batching tests (#401): routeChain resolves every chain entry's
// candidates through ONE bounded-parallel EnsureToolCallResolved
// invocation before the first feedback read, while the joined
// route-failure error preserves the per-selector interleaved order
// (each selector contributes its lookup error, or -- on successful
// lookup -- its candidate probe diagnostics, in selector order).
package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// newChainProbeRouter builds a Router over one fake provider ("p") whose
// models are seeded WITHOUT CapToolCall and whose registry has live
// capability resolution wired to the given prober.
func newChainProbeRouter(t *testing.T, prober fingerprint.ModelProber, models ...string) *Router {
	t.Helper()
	provReg := NewRegistry()
	if err := provReg.Register(&fakeChainProvider{name: "p", models: models}); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	mr, err := NewModelRegistry(provReg, nil,
		WithCapabilityProbeStore(newFakeCapProbeStore()),
		WithCapabilityProber(capProberFactory(prober)),
	)
	if err != nil {
		t.Fatalf("new model registry: %v", err)
	}
	for _, m := range models {
		seedChainProfile(t, mr, &ModelProfile{
			Key:           ModelKey{Provider: "p", Model: m},
			Caps:          CapChat | CapGenerate | CapStream,
			Quality:       TierGood,
			Speed:         TierGood,
			ContextWindow: 8192,
		})
	}
	if err := provReg.RefreshModels(context.Background(), "p"); err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	r := NewRouter(mr, provReg)
	cleanupRouter(t, r)
	return r
}

func TestRouteChain_EntriesResolveInOneBoundedWave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prober := newGatedToolCallProber()
		for _, m := range []string{"a", "b"} {
			prober.errs[m] = errors.New("transient " + m)
			prober.gates[m] = make(chan struct{})
		}
		r := newChainProbeRouter(t, prober, "a", "b")

		done := make(chan error, 1)
		go func() {
			_, err := r.Route(context.Background(), RoutingRequest{
				UseCase:        "chat",
				RequiredCaps:   CapChat | CapToolCall,
				PreferredChain: []string{"p/a", "p/b"},
				StrictChain:    true,
				Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
			})
			done <- err
		}()

		// Both qualified entries' probes must reach the wave together:
		// per-entry resolution would hold entry b behind entry a's gate.
		synctest.Wait()
		started := prober.startedModels()
		if len(started) != 2 {
			t.Fatalf("probes in flight = %v, want both chain entries concurrently", started)
		}

		close(prober.gates["a"])
		close(prober.gates["b"])
		if err := <-done; err == nil {
			t.Fatal("Route() = nil error, want failure (both probes transient, strict chain)")
		}
	})
}

func TestRouteChain_JoinedErrorKeepsPerSelectorOrder(t *testing.T) {
	ctx := context.Background()
	prober := newFakeToolCallProber()
	prober.errs["a"] = errors.New("probe boom")
	r := newChainProbeRouter(t, prober, "a")

	// Entry 1 resolves and its probe errors; entry 2 fails lookup
	// (provider never registered). A naive "collect lookup errors during
	// resolution, append probe diagnostics later" implementation reverses
	// them in the joined route error.
	_, err := r.Route(ctx, RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat | CapToolCall,
		PreferredChain: []string{"p/a", "missing/x:1"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Route() = nil error, want chain exhaustion")
	}
	msg := err.Error()
	probeDiag := "resolve tool_call p/a: probe boom"
	lookupDiag := `chain entry "missing/x:1"`
	pi := strings.Index(msg, probeDiag)
	li := strings.Index(msg, lookupDiag)
	if pi < 0 || li < 0 {
		t.Fatalf("route error missing diagnostics:\n%s", msg)
	}
	if pi > li {
		t.Fatalf("diagnostic order reversed: probe diag at %d after lookup diag at %d in:\n%s", pi, li, msg)
	}
}
