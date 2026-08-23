// provider/capresolve_admission_test.go
//
// Tests for the #401 admission seam on capability resolution: the
// registry-held slotAdmitter, permit scope (acquire after a valid-row
// miss, release immediately after the probe returns, before
// persistence), and the post-probe timestamp contract.
package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeSlotAdmitter records acquireSlot traffic. attempts counts every
// call; acquires records keys for successful admissions only; releases
// counts raw release-func invocations so a double-release bug is visible.
type fakeSlotAdmitter struct {
	mu       sync.Mutex
	attempts int
	acquires []ModelKey
	releases int
	err      error // when non-nil, acquireSlot fails with it
}

func (a *fakeSlotAdmitter) acquireSlot(_ context.Context, key ModelKey) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts++
	if a.err != nil {
		return nil, a.err
	}
	a.acquires = append(a.acquires, key)
	return func() {
		a.mu.Lock()
		a.releases++
		a.mu.Unlock()
	}, nil
}

func (a *fakeSlotAdmitter) stats() (attempts int, acquires []ModelKey, releases int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempts, append([]ModelKey(nil), a.acquires...), a.releases
}

// hookedCapProbeStore runs onSave before delegating SaveCapProbe, so a
// test can observe admission state at the moment of persistence.
type hookedCapProbeStore struct {
	*fakeCapProbeStore
	onSave func()
}

func (s *hookedCapProbeStore) SaveCapProbe(ctx context.Context, probe fingerprint.CapProbe) error {
	if s.onSave != nil {
		s.onSave()
	}
	return s.fakeCapProbeStore.SaveCapProbe(ctx, probe)
}

// timedToolCallProber blocks each ProbeToolCall on gate, signals started
// once, and records the time it resumed. entryAt is strictly after the
// test closes gate, so asserting !row.TestedAt.Before(entryAt) proves the
// persisted timestamp was captured after the probe ran -- deterministic
// monotonic ordering, no clock abstraction, no elapsed-time assertions.
type timedToolCallProber struct {
	*fakeToolCallProber
	started   chan struct{}
	startOnce sync.Once
	gate      chan struct{}
	timedMu   sync.Mutex
	entryAt   time.Time
}

func newTimedToolCallProber() *timedToolCallProber {
	return &timedToolCallProber{
		fakeToolCallProber: newFakeToolCallProber(),
		started:            make(chan struct{}),
		gate:               make(chan struct{}),
	}
}

func (p *timedToolCallProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.startOnce.Do(func() { close(p.started) })
	<-p.gate
	p.timedMu.Lock()
	p.entryAt = time.Now()
	p.timedMu.Unlock()
	return p.fakeToolCallProber.ProbeToolCall(ctx, model)
}

func (p *timedToolCallProber) entry() time.Time {
	p.timedMu.Lock()
	defer p.timedMu.Unlock()
	return p.entryAt
}

// ---------------------------------------------------------------------------
// Acquire/release placement
// ---------------------------------------------------------------------------

func TestResolveToolCall_AdmissionAcquireReleaseAroundProbe(t *testing.T) {
	ctx := context.Background()
	admitter := &fakeSlotAdmitter{}
	base := newFakeCapProbeStore()
	releasedAtSave := -1
	store := &hookedCapProbeStore{fakeCapProbeStore: base, onSave: func() {
		_, _, releasedAtSave = admitter.stats()
	}}
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	mr.setSlotAdmitter(admitter)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	attempts, acquires, releases := admitter.stats()
	if attempts != 1 || len(acquires) != 1 || acquires[0] != key {
		t.Fatalf("acquires = %d/%v, want exactly one for %v", attempts, acquires, key)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want exactly 1", releases)
	}
	if releasedAtSave != 1 {
		t.Fatalf("releases at SaveCapProbe time = %d, want 1 (permit must not span persistence)", releasedAtSave)
	}
}

func TestResolveToolCall_AdmissionZeroAcquireFastPaths(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	seedRow := func(state fingerprint.CapProbeState) fingerprint.CapProbe {
		return fingerprint.CapProbe{
			BackendID:    "probe-prov",
			ModelName:    "mystery-model",
			Capability:   "tool_call",
			State:        state,
			ModelDigest:  key.String(),
			ProbeVersion: fingerprint.CurrentToolProbeVersion,
			TestedAt:     time.Now(),
		}
	}

	cases := []struct {
		name      string
		setup     func(t *testing.T, mr *ModelRegistry, store *fakeCapProbeStore)
		key       ModelKey
		wantState fingerprint.CapProbeState
	}{
		{
			name: "explicit override yes",
			setup: func(_ *testing.T, mr *ModelRegistry, _ *fakeCapProbeStore) {
				mr.SetCapabilityOverride(func(ModelKey) []string { return []string{"tool_call"} })
			},
			key:       key,
			wantState: fingerprint.CapProbeYes,
		},
		{
			name: "explicit override no",
			setup: func(_ *testing.T, mr *ModelRegistry, _ *fakeCapProbeStore) {
				mr.SetCapabilityOverride(func(ModelKey) []string { return []string{"chat"} })
			},
			key:       key,
			wantState: fingerprint.CapProbeNo,
		},
		{
			name: "valid cached no row",
			setup: func(t *testing.T, _ *ModelRegistry, store *fakeCapProbeStore) {
				seedCapRow(t, store, seedRow(fingerprint.CapProbeNo))
			},
			key:       key,
			wantState: fingerprint.CapProbeNo,
		},
		{
			name: "valid cached inconclusive row",
			setup: func(t *testing.T, _ *ModelRegistry, store *fakeCapProbeStore) {
				seedCapRow(t, store, seedRow(fingerprint.CapProbeInconclusive))
			},
			key:       key,
			wantState: fingerprint.CapProbeInconclusive,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admitter := &fakeSlotAdmitter{}
			store := newFakeCapProbeStore()
			prober := newFakeToolCallProber()
			mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
			mr.setSlotAdmitter(admitter)
			tc.setup(t, mr, store)

			state, err := mr.ResolveToolCall(ctx, tc.key)
			if err != nil {
				t.Fatalf("ResolveToolCall() error: %v", err)
			}
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if attempts, _, _ := admitter.stats(); attempts != 0 {
				t.Fatalf("acquire attempts = %d, want 0", attempts)
			}
			if got := prober.totalCalls(); got != 0 {
				t.Fatalf("prober calls = %d, want 0", got)
			}
		})
	}

	t.Run("merged catalog caps", func(t *testing.T) {
		admitter := &fakeSlotAdmitter{}
		store := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		mr := newCapResolveRegistry(t, "ollama-local", []string{"qwen3:8b"}, store, prober)
		mr.setSlotAdmitter(admitter)

		state, err := mr.ResolveToolCall(ctx, ModelKey{Provider: "ollama-local", Model: "qwen3:8b"})
		if err != nil {
			t.Fatalf("ResolveToolCall() error: %v", err)
		}
		if state != fingerprint.CapProbeYes {
			t.Fatalf("state = %q, want yes (catalog declares tools)", state)
		}
		if attempts, _, _ := admitter.stats(); attempts != 0 {
			t.Fatalf("acquire attempts = %d, want 0", attempts)
		}
	})

	t.Run("resolution disabled", func(t *testing.T) {
		admitter := &fakeSlotAdmitter{}
		mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, nil, nil)
		mr.setSlotAdmitter(admitter)

		state, err := mr.ResolveToolCall(ctx, key)
		if err != nil {
			t.Fatalf("ResolveToolCall() error: %v", err)
		}
		if state != "" {
			t.Fatalf("state = %q, want unknown", state)
		}
		if attempts, _, _ := admitter.stats(); attempts != 0 {
			t.Fatalf("acquire attempts = %d, want 0", attempts)
		}
	})
}

func TestResolveToolCall_AdmissionExpiredRowStillAcquires(t *testing.T) {
	ctx := context.Background()
	admitter := &fakeSlotAdmitter{}
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	mr.setSlotAdmitter(admitter)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	seedCapRow(t, store, fingerprint.CapProbe{
		BackendID:    "probe-prov",
		ModelName:    "mystery-model",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeNo,
		ModelDigest:  key.String(),
		ProbeVersion: fingerprint.CurrentToolProbeVersion - 1, // invalid: stale probe version
		TestedAt:     time.Now().Add(-time.Hour),
	})

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	if attempts, _, _ := admitter.stats(); attempts != 1 {
		t.Fatalf("acquire attempts = %d, want 1 (invalid row must not skip admission)", attempts)
	}
	if got := prober.callCount("mystery-model"); got != 1 {
		t.Fatalf("prober calls = %d, want 1", got)
	}
}

func TestResolveToolCall_AdmissionReleasedOnProbeError(t *testing.T) {
	ctx := context.Background()
	admitter := &fakeSlotAdmitter{}
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	probeErr := errors.New("backend 401")
	prober.errs["mystery-model"] = probeErr
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	mr.setSlotAdmitter(admitter)

	_, err := mr.ResolveToolCall(ctx, ModelKey{Provider: "probe-prov", Model: "mystery-model"})
	if !errors.Is(err, probeErr) {
		t.Fatalf("ResolveToolCall() error = %v, want %v", err, probeErr)
	}
	if _, _, releases := admitter.stats(); releases != 1 {
		t.Fatalf("releases = %d, want exactly 1 on probe error", releases)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("store rows = %d, want 0 (transient errors never persist)", got)
	}
}

func TestResolveToolCall_AdmissionAcquireFailureSkipsProbe(t *testing.T) {
	ctx := context.Background()
	admitErr := errors.New("router closed")
	admitter := &fakeSlotAdmitter{err: admitErr}
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	mr.setSlotAdmitter(admitter)

	state, err := mr.ResolveToolCall(ctx, ModelKey{Provider: "probe-prov", Model: "mystery-model"})
	if !errors.Is(err, admitErr) {
		t.Fatalf("ResolveToolCall() error = %v, want %v", err, admitErr)
	}
	if state != "" {
		t.Fatalf("state = %q, want unknown", state)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0 (denied admission must not probe)", got)
	}
	if got := store.count(); got != 0 {
		t.Fatalf("store rows = %d, want 0", got)
	}
	if _, _, releases := admitter.stats(); releases != 0 {
		t.Fatalf("releases = %d, want 0 (no permit was granted)", releases)
	}
}

// ---------------------------------------------------------------------------
// Post-probe timestamps
// ---------------------------------------------------------------------------

func TestResolveToolCall_TimestampsCapturedAfterProbe(t *testing.T) {
	cases := []struct {
		name       string
		outcome    fingerprint.CapProbeOutcome
		wantExpiry func(testedAt time.Time) time.Time
	}{
		{
			name:       "digestless no TTL",
			outcome:    fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo},
			wantExpiry: func(testedAt time.Time) time.Time { return testedAt.Add(fingerprint.CapProbeDigestlessNoTTL) },
		},
		{
			name:       "ordinary outcome TTL",
			outcome:    fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes, TTL: time.Hour},
			wantExpiry: func(testedAt time.Time) time.Time { return testedAt.Add(time.Hour) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newFakeCapProbeStore()
			prober := newTimedToolCallProber()
			prober.outcomes["mystery-model"] = tc.outcome
			mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
			key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

			type result struct {
				state fingerprint.CapProbeState
				err   error
			}
			done := make(chan result, 1)
			go func() {
				state, err := mr.ResolveToolCall(ctx, key)
				done <- result{state, err}
			}()

			select {
			case <-prober.started:
			case <-time.After(2 * time.Second):
				t.Fatal("probe did not start")
			}
			close(prober.gate)

			var got result
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("ResolveToolCall did not finish")
			}
			if got.err != nil {
				t.Fatalf("ResolveToolCall() error: %v", got.err)
			}

			row, ok := store.row("probe-prov", "mystery-model", "tool_call")
			if !ok {
				t.Fatal("expected persisted row")
			}
			entry := prober.entry()
			if row.TestedAt.Before(entry) {
				t.Fatalf("TestedAt = %v is before probe entry %v; must be captured after the probe", row.TestedAt, entry)
			}
			if want := tc.wantExpiry(row.TestedAt); !row.ExpiresAt.Equal(want) {
				t.Fatalf("ExpiresAt = %v, want %v (derived from post-probe TestedAt)", row.ExpiresAt, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Router wiring
// ---------------------------------------------------------------------------

func TestNewRouter_WiresRegistryAdmitter(t *testing.T) {
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	provReg := NewRegistry()

	router := NewRouter(mr, provReg)
	defer func() { _ = router.Close() }()

	mr.mu.RLock()
	got := mr.admitter
	mr.mu.RUnlock()
	if got == nil {
		t.Fatal("registry admitter = nil, want the router")
	}
	if r, ok := got.(*Router); !ok || r != router {
		t.Fatalf("registry admitter = %T(%p), want the constructing router %p", got, got, router)
	}
}

// seedCapRow saves a row and fails the test on error.
func seedCapRow(t *testing.T, store fingerprint.CapProbeStore, row fingerprint.CapProbe) {
	t.Helper()
	if err := store.SaveCapProbe(context.Background(), row); err != nil {
		t.Fatalf("seed SaveCapProbe() error: %v", err)
	}
}
