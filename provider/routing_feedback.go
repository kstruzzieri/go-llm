// Package provider — routing_feedback.go defines the RoutingFeedback seam:
// a provider/model/use-case keyed signal store distinct from the chunk-key
// oriented feedback.SignalStore. PR1 (Epic #48 Phase 5) ships types,
// interface, in-memory wiring, and the RouteAttempt model; subsequent PRs
// wire outcomes, persist signals, and read scores into routing decisions.
package provider

import (
	"context"
	"errors"
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

// Aggregate is the read-side view of accumulated routing feedback. Score is
// in [0, 1] with 0.5 neutral. ScoredCount is the subset of SampleCount with
// non-zero effective strength.
type Aggregate struct {
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
	Get(ctx context.Context, key FeedbackKey) (Aggregate, error)
	Record(ctx context.Context, key FeedbackKey, sig FeedbackSignal) error
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

// Sentinel errors returned by the seam.
var (
	ErrInvalidFeedbackKey    = errors.New("provider: invalid feedback key")
	ErrUnknownSignalKind     = errors.New("provider: unknown routing signal kind")
	ErrInvalidSignalStrength = errors.New("provider: invalid signal strength")
	ErrInvalidSignalPayload  = errors.New("provider: invalid signal payload")
	ErrMetaTooLarge          = errors.New("provider: signal meta too large")
	ErrUnknownAttemptStatus  = errors.New("provider: unknown attempt status")
)
