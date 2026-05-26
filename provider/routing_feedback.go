// routing_feedback.go defines the RoutingFeedback seam: a provider/model/
// use-case keyed signal store distinct from the chunk-key oriented
// feedback.SignalStore. PR1 (Epic #48 Phase 5) ships types, interface, and
// the in-memory wiring; subsequent PRs persist signals and read scores into
// routing decisions.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// FeedbackKey identifies a routing-feedback aggregation bucket. Provider is
// the configured instance name (e.g. "ollama-local-a", "vllm-prod-1") —
// matching ModelKey.Provider after #117/#118 — not the API kind.
type FeedbackKey struct {
	Provider string
	Model    string
	UseCase  string
}

// RoutingSignalKind enumerates the routing-owned signal kinds emitted in PR1.
// FallbackUsed/CircuitOpened/Canceled were considered and deferred; see the
// design spec for the rationale.
type RoutingSignalKind string

const (
	RoutingSignalSuccess RoutingSignalKind = "success"
	RoutingSignalFailure RoutingSignalKind = "failure"
	RoutingSignalLatency RoutingSignalKind = "latency"
)

// defaultStrength maps each kind to its built-in strength contribution.
// Latency defaults to 0.0 so latency signals count toward SampleCount but
// not toward Score in PR1 (PR3 supplies latency-to-score policy).
var defaultStrength = map[RoutingSignalKind]float64{
	RoutingSignalSuccess: +0.5,
	RoutingSignalFailure: -0.7,
	RoutingSignalLatency: 0.0,
}

// DefaultStrength returns the default strength for kind, or 0 when kind is
// not a known RoutingSignalKind.
func DefaultStrength(kind RoutingSignalKind) float64 {
	return defaultStrength[kind]
}

// FeedbackSignal is the typed routing-feedback payload. Strength uses
// pointer-nil semantics so callers can distinguish "use the kind default"
// (nil) from "explicit, including zero" (non-nil pointer).
type FeedbackSignal struct {
	Kind         RoutingSignalKind
	Strength     *float64
	At           time.Time
	LatencyMs    int64
	ErrorClass   string
	RouteID      string
	CompletionID string
	Meta         map[string]string
}

// Aggregate is the read-side view of accumulated routing feedback. ScoredCount
// is the subset of SampleCount with non-zero effective strength.
type Aggregate struct {
	// Score in [0, 1]; neutral is store-configured (default 0.5). Computed
	// from signals whose effective strength is non-zero. Returns the
	// configured neutral value when ScoredCount == 0.
	Score       float64
	SampleCount int
	ScoredCount int
	UpdatedAt   time.Time
}

// FeedbackItem pairs a key with a signal for batch ingestion.
type FeedbackItem struct {
	Key    FeedbackKey
	Signal FeedbackSignal
}

// RoutingFeedbackStore is the persistence boundary for the seam. PR3 swaps
// the in-memory implementation for a SQLite-backed one without touching the
// interface.
type RoutingFeedbackStore interface {
	// Get returns the current aggregate for key, or a neutral aggregate
	// when no signals are stored under it.
	Get(ctx context.Context, key FeedbackKey) (Aggregate, error)

	// Record persists a single signal. Implementations must validate the
	// key and signal against the same rules enforced by RoutingFeedback.
	Record(ctx context.Context, key FeedbackKey, sig FeedbackSignal) error

	// RecordBatch persists multiple signals as a single atomic state
	// transition: if any item fails validation, no item is persisted.
	RecordBatch(ctx context.Context, items []FeedbackItem) error
}

// RoutingFeedback is the ergonomic public wrapper around a
// RoutingFeedbackStore. RecordOutcome decomposes per-attempt route results
// into typed signals; producers can't supply a FeedbackKey directly to
// avoid misattribution.
type RoutingFeedback struct {
	store RoutingFeedbackStore
}

// NewRoutingFeedback wraps a store; the name avoids the too-vague
// provider.New(...) that conflicts with other constructible types in this
// package.
func NewRoutingFeedback(store RoutingFeedbackStore) *RoutingFeedback {
	return &RoutingFeedback{store: store}
}

// Score returns the aggregate for key. If the underlying store has no
// signals for key, returns the store's configured neutral aggregate.
// Store read errors are returned to the caller; the PR3 scoring path is
// expected to wrap this call with fail-open behavior so a store failure
// never blocks routing decisions.
func (rf *RoutingFeedback) Score(ctx context.Context, key FeedbackKey) (Aggregate, error) {
	return rf.store.Get(ctx, key)
}

// Record persists a single signal via the underlying store. Validation
// happens inside the store implementation, so error sentinels match
// (ErrInvalidFeedbackKey, ErrUnknownSignalKind, ...).
func (rf *RoutingFeedback) Record(ctx context.Context, key FeedbackKey, sig FeedbackSignal) error {
	return rf.store.Record(ctx, key, sig)
}

// RecordOutcome decomposes out.Attempts into typed per-attempt signals and
// persists them atomically via the underlying store's RecordBatch.
//
// Decomposition rules (see spec § "Decomposition rules — worked examples"):
//   - One signal pass per attempt in out.Attempts (in order).
//   - AttemptStatusSucceeded: emit Success at key derived from
//     attempt.Key + useCase.
//   - AttemptStatusFailed:    emit Failure (carrying ErrorClass; empty
//     normalizes to "unknown") at the same key.
//   - AttemptStatusUnknown:   skip (no signal emitted).
//   - Any other AttemptStatus value: return ErrUnknownAttemptStatus and
//     persist no signals from this outcome.
//   - LatencyMs > 0: additionally emit a Latency signal at the same key.
//   - LatencyMs < 0: return ErrInvalidSignalPayload (no signals persisted).
//
// All emitted signals share a single sampled-once-up-front timestamp and
// the same RouteID copied from out.RouteID, so decomposition order has no
// effect on UpdatedAt or route correlation.
//
// useCase must be non-empty; an empty useCase cannot form a FeedbackKey
// and returns ErrInvalidFeedbackKey.
//
// If out.Attempts is empty or nil, RecordOutcome is a no-op and returns
// nil — the seam's pre-PR2 zero-value-safe property.
func (rf *RoutingFeedback) RecordOutcome(ctx context.Context, useCase string, out RouteOutcome) error {
	if len(out.Attempts) == 0 {
		return nil
	}
	if useCase == "" {
		return fmt.Errorf("%w: useCase is required", ErrInvalidFeedbackKey)
	}
	now := time.Now()
	items := make([]FeedbackItem, 0, len(out.Attempts)*2)
	for i, attempt := range out.Attempts {
		key := FeedbackKey{
			Provider: attempt.Key.Provider,
			Model:    attempt.Key.Model,
			UseCase:  useCase,
		}
		if attempt.LatencyMs < 0 {
			return fmt.Errorf("%w: attempt[%d].LatencyMs=%d", ErrInvalidSignalPayload, i, attempt.LatencyMs)
		}
		switch attempt.Status {
		case AttemptStatusUnknown:
			continue
		case AttemptStatusSucceeded:
			items = append(items, FeedbackItem{
				Key: key,
				Signal: FeedbackSignal{
					Kind:    RoutingSignalSuccess,
					At:      now,
					RouteID: out.RouteID,
				},
			})
		case AttemptStatusFailed:
			errClass := attempt.ErrorClass
			if errClass == "" {
				errClass = "unknown"
			}
			items = append(items, FeedbackItem{
				Key: key,
				Signal: FeedbackSignal{
					Kind:       RoutingSignalFailure,
					ErrorClass: errClass,
					At:         now,
					RouteID:    out.RouteID,
				},
			})
		default:
			return fmt.Errorf("%w: attempt[%d].Status=%d", ErrUnknownAttemptStatus, i, attempt.Status)
		}
		if attempt.LatencyMs > 0 {
			items = append(items, FeedbackItem{
				Key: key,
				Signal: FeedbackSignal{
					Kind:      RoutingSignalLatency,
					LatencyMs: attempt.LatencyMs,
					At:        now,
					RouteID:   out.RouteID,
				},
			})
		}
	}
	if len(items) == 0 {
		// All attempts were AttemptStatusUnknown.
		return nil
	}
	return rf.store.RecordBatch(ctx, items)
}

// ErrInvalidFeedbackKey is returned when a FeedbackKey has an empty
// Provider, Model, or UseCase field, or when RoutingFeedback.RecordOutcome
// is called with an empty useCase argument.
var ErrInvalidFeedbackKey = errors.New("provider: invalid feedback key")

// ErrUnknownSignalKind is returned when a FeedbackSignal carries a Kind
// that is not one of the declared RoutingSignalKind constants.
var ErrUnknownSignalKind = errors.New("provider: unknown routing signal kind")

// ErrInvalidSignalStrength is returned when a non-nil FeedbackSignal.Strength
// holds a NaN or infinite value.
var ErrInvalidSignalStrength = errors.New("provider: invalid signal strength")

// ErrInvalidSignalPayload is returned when a FeedbackSignal's payload
// fields violate kind-specific invariants — e.g. a Latency signal without
// a positive LatencyMs, or a non-Failure signal carrying an ErrorClass.
var ErrInvalidSignalPayload = errors.New("provider: invalid signal payload")

// ErrMetaTooLarge is returned when a FeedbackSignal.Meta exceeds the
// store's configured key count or per-value byte limits.
var ErrMetaTooLarge = errors.New("provider: signal meta too large")

// ErrUnknownAttemptStatus is returned by RoutingFeedback.RecordOutcome
// when a RouteAttempt.Status (introduced in Task 2 / provider/types.go)
// holds a value outside the declared AttemptStatus constants.
var ErrUnknownAttemptStatus = errors.New("provider: unknown attempt status")

// validateKey enforces non-empty key triple semantics.
func validateKey(k FeedbackKey) error {
	if k.Provider == "" || k.Model == "" || k.UseCase == "" {
		return fmt.Errorf("%w: %+v", ErrInvalidFeedbackKey, k)
	}
	return nil
}

// validateSignal enforces shape/payload invariants:
//   - Kind is one of the known constants.
//   - Strength, if non-nil, is finite (no NaN/Inf).
//   - LatencyMs is positive iff Kind is Latency.
//   - ErrorClass is set iff Kind is Failure.
//   - Meta does not exceed maxKeys keys or maxValBytes bytes per value.
func validateSignal(sig FeedbackSignal, maxKeys, maxValBytes int) error {
	switch sig.Kind {
	case RoutingSignalSuccess, RoutingSignalFailure, RoutingSignalLatency:
		// ok
	default:
		return fmt.Errorf("%w: %q", ErrUnknownSignalKind, sig.Kind)
	}
	if sig.Strength != nil {
		v := *sig.Strength
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: %v", ErrInvalidSignalStrength, v)
		}
	}
	switch sig.Kind {
	case RoutingSignalLatency:
		if sig.LatencyMs <= 0 {
			return fmt.Errorf("%w: latency signal requires LatencyMs > 0", ErrInvalidSignalPayload)
		}
	default:
		if sig.LatencyMs != 0 {
			return fmt.Errorf("%w: non-latency signal must not carry LatencyMs", ErrInvalidSignalPayload)
		}
	}
	switch sig.Kind {
	case RoutingSignalFailure:
		if sig.ErrorClass == "" {
			return fmt.Errorf("%w: failure signal requires ErrorClass", ErrInvalidSignalPayload)
		}
	default:
		if sig.ErrorClass != "" {
			return fmt.Errorf("%w: non-failure signal must not carry ErrorClass", ErrInvalidSignalPayload)
		}
	}
	if maxKeys > 0 && len(sig.Meta) > maxKeys {
		return fmt.Errorf("%w: %d meta keys exceeds limit %d", ErrMetaTooLarge, len(sig.Meta), maxKeys)
	}
	if maxValBytes > 0 {
		for k, v := range sig.Meta {
			if len(v) > maxValBytes {
				return fmt.Errorf("%w: meta[%q] %d bytes exceeds limit %d",
					ErrMetaTooLarge, k, len(v), maxValBytes)
			}
		}
	}
	return nil
}

// cloneSignal returns a deep copy with store-owned Strength and Meta
// memory so callers cannot retroactively mutate stored signals.
func cloneSignal(sig FeedbackSignal) FeedbackSignal {
	out := sig
	if sig.Strength != nil {
		v := *sig.Strength
		out.Strength = &v
	}
	if sig.Meta != nil {
		m := make(map[string]string, len(sig.Meta))
		for k, v := range sig.Meta {
			m[k] = v
		}
		out.Meta = m
	}
	return out
}
