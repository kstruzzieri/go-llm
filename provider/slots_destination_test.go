package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// Slot probes are repository-owned metadata traffic (I14): they must run
// through a per-backend guarded client and bind a slot-probe capability per
// probe, freshly from the gate — a cached context would die at the first
// generation change and never recover after re-admission.

func slotGateFor(t *testing.T, baseURL string) (*DestinationGate, Destination) {
	t.Helper()
	dest := mustDest(t, "lc", baseURL)
	gate := installTestGate(t, DestinationEdge{Purpose: DestinationPurposeSlotProbe, Destination: dest})
	return gate, dest
}

// The wired path: per-backend guarded client plus per-probe binder. The probe
// reaches the backend and the request that arrives carried the capability
// (the guarded client would have denied it otherwise).
func TestSlotProbeThroughGuardedClientAndBinder(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	gate, dest := slotGateFor(t, srv.URL)
	guarded, err := GuardHTTPClient(gate, dest, nil)
	if err != nil {
		t.Fatal(err)
	}

	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotClientFor(func(providerName string) *http.Client {
			if providerName != "lc" {
				t.Errorf("clientFor called for %q", providerName)
			}
			return guarded
		}),
		WithSlotProbeBinder(func(ctx context.Context, providerName string) (context.Context, error) {
			return gate.Bind(ctx, DestinationPurposeSlotProbe, providerName)
		}),
	)

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()

	mu.Lock()
	got := count
	mu.Unlock()
	if got != 1 {
		t.Fatalf("guarded slot probe reached the server %d times, want 1", got)
	}
	if n, ok := ss.Capacity(key); !ok || n != 4 {
		t.Fatalf("Capacity = (%d, %v), want probed (4, true)", n, ok)
	}
}

// A binder denial is a zero-request outcome: no dial, and the entry degrades
// to the existing fail-safe serial capacity — never a crash, never a bypass.
func TestSlotProbeBinderDenialProducesZeroRequests(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotProbeBinder(func(ctx context.Context, providerName string) (context.Context, error) {
			return nil, &DestinationDeniedError{Provider: providerName, Purpose: DestinationPurposeSlotProbe}
		}),
	)

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()

	mu.Lock()
	got := count
	mu.Unlock()
	if got != 0 {
		t.Fatalf("denied slot probe reached the server %d times, want 0", got)
	}
	if n, ok := ss.Capacity(key); !ok || n != 1 {
		t.Fatalf("Capacity after denial = (%d, %v), want fail-safe (1, true)", n, ok)
	}
}

// The binder runs per probe, not once: after Clear plus re-Install, the next
// probe binds against the NEW generation and succeeds. A cached context
// would be permanently dead here.
func TestSlotProbeBinderIsFreshPerProbe(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	dest := mustDest(t, "lc", srv.URL)
	edge := DestinationEdge{Purpose: DestinationPurposeSlotProbe, Destination: dest}
	m, err := NewDestinationManifest(edge)
	if err != nil {
		t.Fatal(err)
	}
	pol := NewDestinationPolicy(dest)
	gate := NewDestinationGate()
	if err := gate.Install(pol, m); err != nil {
		t.Fatal(err)
	}
	guarded, err := GuardHTTPClient(gate, dest, nil)
	if err != nil {
		t.Fatal(err)
	}

	ss, cl, now := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotClientFor(func(string) *http.Client { return guarded }),
		WithSlotProbeBinder(func(ctx context.Context, providerName string) (context.Context, error) {
			return gate.Bind(ctx, DestinationPurposeSlotProbe, providerName)
		}),
	)

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()
	mu.Lock()
	first := count
	mu.Unlock()
	if first != 1 {
		t.Fatalf("first probe count = %d, want 1", first)
	}

	// Revoke, re-admit, expire the cache, probe again: the fresh bind must
	// authorize against the NEW generation.
	gate.Clear()
	if err := gate.Install(pol, m); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(defaultSlotTTL + 1)
	ss.RecordUse(key)
	cl.drain()

	mu.Lock()
	second := count
	mu.Unlock()
	if second != 2 {
		t.Fatalf("post-re-admission probe count = %d, want 2 (stale-context wiring would stay denied)", second)
	}
	if n, ok := ss.Capacity(key); !ok || n != 4 {
		t.Fatalf("Capacity after re-admission = (%d, %v), want (4, true)", n, ok)
	}
}

// nil options keep the pre-#477 behavior byte-identical.
func TestSlotProbeWithoutSeamsUnchanged(t *testing.T) {
	var mu sync.Mutex
	slots, count := 3, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotClientFor(nil), WithSlotProbeBinder(nil))

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()
	if n, ok := ss.Capacity(key); !ok || n != 3 {
		t.Fatalf("Capacity = (%d, %v), want (3, true)", n, ok)
	}
}

// A clientFor returning nil falls back to the shared client rather than
// dialing with a nil client and panicking mid-goroutine.
func TestSlotProbeClientForNilFallsBack(t *testing.T) {
	var mu sync.Mutex
	slots, count := 2, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotClientFor(func(string) *http.Client { return nil }))

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()
	if n, ok := ss.Capacity(key); !ok || n != 2 {
		t.Fatalf("Capacity = (%d, %v), want (2, true)", n, ok)
	}
}

// Binder errors surface the typed denial to nothing — but they must not be
// distinguishable from probe failure at the capacity level, and they must
// not panic the goroutine. This pins the error-shape tolerance.
func TestSlotProbeBinderErrorShapeTolerated(t *testing.T) {
	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: "http://127.0.0.1:1"},
		WithSlotProbeBinder(func(context.Context, string) (context.Context, error) {
			return nil, fmt.Errorf("wrapped: %w", errors.New("opaque failure"))
		}))
	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	cl.drain()
	if n, ok := ss.Capacity(key); !ok || n != 1 {
		t.Fatalf("Capacity = (%d, %v), want fail-safe (1, true)", n, ok)
	}
}
