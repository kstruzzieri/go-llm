// admission.go implements the slot-aware admission gate (#400): a
// Router-owned, per-ModelKey permit counter sized by the SlotSource on
// every decision. See the doc comment on slotAdmission for the
// invariants.
package provider

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

// slotAdmitter is the seam RoutePlan consumes to acquire permits. The
// Router implements it; a dedicated interface (rather than reusing the
// RouteRecorder type-assertion precedent) keeps admission — a correctness
// property — immune to external SetRecorder swaps, and lets plan tests
// inject a recording fake.
type slotAdmitter interface {
	// acquireSlot blocks until a permit for key is available, the ctx is
	// done, or the router closes. On success release is non-nil and must
	// be called exactly once (it is sync.Once-guarded, so a deferred
	// backstop call is safe). Ungoverned keys return (no-op, nil)
	// immediately without touching gate state.
	acquireSlot(ctx context.Context, key ModelKey) (release func(), err error)
}

// noopRelease is the shared release for ungoverned admissions. A
// captureless closure compiles to a static funcval anyway (no per-call
// allocation); the named var exists for call-site clarity — "no permit
// held" is explicit — not for performance.
var noopRelease = func() {}

// slotGateState is one key's admission state. Guarded by slotAdmission.mu.
type slotGateState struct {
	inflight int
	// waiting is the instantaneous queue-backlog gauge: incremented when
	// a caller first blocks, decremented when it leaves the queue for
	// any reason (admit, cancel, close, governance flip).
	waiting int
	// wake is closed and replaced on every release (broadcast). Waiters
	// capture it under mu, then select; a closed channel selects
	// immediately, so a release between unlock and select is never lost.
	wake chan struct{}

	admitted uint64
	queued   uint64
	rejected uint64
}

// slotAdmission is the Router-owned admission gate. Capacity is re-read
// from src on every decision (I1); permits are the inflight count under
// mu (I3 — no hand-off). done is the Router's done channel: it unblocks
// queued waiters with ErrRouterClosed (I5) but does not reject callers
// that admit against free capacity, preserving today's execute-after-
// Close behavior.
type slotAdmission struct {
	mu    sync.Mutex
	gates map[ModelKey]*slotGateState
	src   SlotSource
	done  <-chan struct{}

	// queuedHook, when non-nil, fires once per caller — on the first
	// block only — after the caller has incremented queued/waiting and
	// captured its wake channel; from that point a release is guaranteed
	// to wake it. Never re-fired on wake/re-check iterations (a test
	// holding an undrained hook channel would deadlock). Test seam
	// (precedent: the launch func on OpenAICompatSlotSource); called
	// without holding mu. Both hooks are read unguarded: tests MUST set
	// them before the first concurrent acquire and never mutate after.
	queuedHook func(ModelKey)
	// wokeHook, when non-nil, fires each time a waiter is woken by a
	// release broadcast, before it loops back to re-check. Test seam:
	// it is the only way to deterministically park a waiter inside the
	// wake-to-recheck window (e.g. to close the router there and prove
	// the post-queue done-priority check, I5). Called without holding mu.
	wokeHook func(ModelKey)
}

func newSlotAdmission(src SlotSource, done <-chan struct{}) *slotAdmission {
	return &slotAdmission{
		gates: make(map[ModelKey]*slotGateState),
		src:   src,
		done:  done,
	}
}

// acquire implements the slotAdmitter contract for the Router.
func (sa *slotAdmission) acquire(ctx context.Context, key ModelKey) (func(), error) {
	first := true
	for {
		if !first {
			// Post-queue iterations: terminal signals take priority over
			// EVERYTHING, including the governance read — a queued
			// waiter's exit must be deterministic on cancel (I4) and
			// Close (I5 close/release race: wake and done can be ready
			// together and select picks nondeterministically) even when
			// a custom source concurrently flips governance.
			if err := ctx.Err(); err != nil {
				sa.exitWait(key, true)
				return nil, err
			}
			select {
			case <-sa.done:
				sa.exitWait(key, true)
				return nil, ErrRouterClosed
			default:
			}
		}
		n, ok := sa.src.Capacity(key)
		if !ok {
			// Ungoverned (I2): no gate state, no serialization. On the
			// ENTRY iteration this runs before the dead-context check, so
			// ungoverned callers — pre-cancelled ones included — behave
			// bit-identically to today all the way into the provider.
			// Also the exit path if a custom source flips governance off
			// mid-wait — inflight was never incremented, so accounting
			// stays balanced (waiting is decremented; the pass counts as
			// neither admitted nor rejected — no permit was involved, I7).
			if !first {
				sa.exitWait(key, false)
			}
			return noopRelease, nil
		}
		if first {
			// I4: a dead context never admits a governed call. Entry
			// iteration only — post-queue iterations checked above,
			// before the governance read.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if n < 1 {
			n = 1 // contract violation fail-safe: serial, never stuck
		}
		sa.mu.Lock()
		g := sa.gates[key]
		if g == nil {
			g = &slotGateState{wake: make(chan struct{})}
			sa.gates[key] = g
		}
		if g.inflight < n {
			g.inflight++
			g.admitted++
			if !first {
				g.waiting--
			}
			sa.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { sa.release(key) }) }, nil
		}
		justQueued := false
		if first {
			g.queued++
			g.waiting++
			first = false
			justQueued = true
		}
		wake := g.wake
		sa.mu.Unlock()
		if justQueued && sa.queuedHook != nil {
			sa.queuedHook(key)
		}
		select {
		case <-wake:
			// re-check under fresh Capacity (I1)
			if sa.wokeHook != nil {
				sa.wokeHook(key)
			}
		case <-ctx.Done():
			sa.exitWait(key, true)
			return nil, ctx.Err()
		case <-sa.done:
			sa.exitWait(key, true)
			return nil, ErrRouterClosed
		}
	}
}

// release decrements inflight and broadcasts. Safe after Router close.
// The decrement is clamped at zero (I6): the release closure is already
// Once-guarded, so underflow requires a further bug — the clamp keeps
// such a bug from driving inflight negative and permanently widening
// the gate. Deliberate defense-in-depth with the Once.
func (sa *slotAdmission) release(key ModelKey) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	g := sa.gates[key]
	if g == nil {
		return
	}
	if g.inflight > 0 {
		g.inflight--
	}
	close(g.wake)
	g.wake = make(chan struct{})
}

// exitWait records a waiter leaving the queue without a permit.
// rejected=true for cancel/close (I4/I5); false for the governance-
// flip-off pass, which counts as neither admitted nor rejected (I7).
func (sa *slotAdmission) exitWait(key ModelKey, rejected bool) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	g := sa.gates[key]
	if g == nil {
		return
	}
	g.waiting--
	if rejected {
		g.rejected++
	}
}

// SlotAdmissionInfo is one key's admission telemetry for operator
// surfaces (Router.SlotAdmissionSnapshot).
type SlotAdmissionInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Capacity int    `json:"capacity"`
	InFlight int    `json:"in_flight"`
	Waiting  int    `json:"waiting"`
	Admitted uint64 `json:"admitted"`
	Queued   uint64 `json:"queued"`
	Rejected uint64 `json:"rejected"`
}

func (sa *slotAdmission) snapshot() []SlotAdmissionInfo {
	// Two-phase on purpose: copy gate state under mu, resolve capacities
	// AFTER unlocking. Capacity is contractually a pure cache read, but a
	// custom SlotSource may take arbitrary locks or time, and sa.mu is on
	// the hot admit path — foreign calls never run under it.
	sa.mu.Lock()
	out := make([]SlotAdmissionInfo, 0, len(sa.gates))
	for key, g := range sa.gates {
		out = append(out, SlotAdmissionInfo{
			Provider: key.Provider,
			Model:    key.Model,
			InFlight: g.inflight,
			Waiting:  g.waiting,
			Admitted: g.admitted,
			Queued:   g.queued,
			Rejected: g.rejected,
		})
	}
	sa.mu.Unlock()
	for i := range out {
		n, _ := sa.src.Capacity(ModelKey{Provider: out[i].Provider, Model: out[i].Model})
		out[i].Capacity = n
	}
	slices.SortFunc(out, func(a, b SlotAdmissionInfo) int {
		if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})
	return out
}
