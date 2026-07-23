// routing_feedback_memory.go provides an in-memory RoutingFeedbackStore.
// It implements bounded per-key retention with FIFO eviction, all-or-nothing
// RecordBatch semantics, and defensive copies of caller-owned Meta and
// Strength on Record. PR3 replaces it with a SQLite-backed store.
package provider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Defaults applied by NewMemoryStore when MemoryStoreConfig fields are
// zero-valued. See MemoryStoreConfig field docs for semantics.
const (
	DefaultMaxRetainedSamples = 1000
	DefaultMaxMetaKeys        = 32
	DefaultMaxMetaValueBytes  = 256
	DefaultNeutralScore       = 0.5
)

// MemoryStoreConfig tunes MemoryStore. All zero-valued fields fall back to
// the documented Default* constants.
type MemoryStoreConfig struct {
	// NeutralScore is returned by Get() when ScoredCount == 0. Must be in
	// [0, 1] after default application. Zero value selects
	// DefaultNeutralScore (0.5).
	NeutralScore float64

	// MaxRetainedSamples bounds per-key signal retention:
	//   0  → use DefaultMaxRetainedSamples (1000)
	//  -1  → unbounded (use only in tests and short-lived processes)
	//  >0  → explicit cap; oldest entries dropped FIFO on overflow
	MaxRetainedSamples int

	// MaxMetaKeys / MaxMetaValueBytes bound Meta size per signal. Zero
	// values select the Default* constants.
	MaxMetaKeys       int
	MaxMetaValueBytes int
}

// resolvedConfig holds post-default values so the hot path does not re-
// resolve defaults on each call.
type resolvedConfig struct {
	neutralScore       float64
	maxRetainedSamples int // -1 means unbounded
	maxMetaKeys        int
	maxMetaValueBytes  int
}

// MemoryStore is the in-memory RoutingFeedbackStore. Safe for concurrent
// use via a single sync.Mutex; SQLite (PR3) will manage its own concurrency.
type MemoryStore struct {
	mu      sync.Mutex
	cfg     resolvedConfig
	signals map[FeedbackKey][]FeedbackSignal
}

// NewMemoryStore validates the config and returns an initialized store.
// Out-of-range NeutralScore (after default application) returns an error
// at construction so misconfiguration cannot silently move Get results.
func NewMemoryStore(cfg MemoryStoreConfig) (*MemoryStore, error) {
	resolved := resolvedConfig{
		neutralScore:       cfg.NeutralScore,
		maxRetainedSamples: cfg.MaxRetainedSamples,
		maxMetaKeys:        cfg.MaxMetaKeys,
		maxMetaValueBytes:  cfg.MaxMetaValueBytes,
	}
	if resolved.neutralScore == 0 {
		resolved.neutralScore = DefaultNeutralScore
	}
	if resolved.maxRetainedSamples == 0 {
		resolved.maxRetainedSamples = DefaultMaxRetainedSamples
	}
	if resolved.maxMetaKeys == 0 {
		resolved.maxMetaKeys = DefaultMaxMetaKeys
	}
	if resolved.maxMetaValueBytes == 0 {
		resolved.maxMetaValueBytes = DefaultMaxMetaValueBytes
	}
	if err := validateNeutralScore(resolved.neutralScore); err != nil {
		return nil, err
	}
	if resolved.maxRetainedSamples < -1 {
		return nil, fmt.Errorf("provider: MaxRetainedSamples %d out of range; use -1 for unbounded", resolved.maxRetainedSamples)
	}
	if resolved.maxMetaKeys < 0 {
		return nil, fmt.Errorf("provider: MaxMetaKeys %d out of range", resolved.maxMetaKeys)
	}
	if resolved.maxMetaValueBytes < 0 {
		return nil, fmt.Errorf("provider: MaxMetaValueBytes %d out of range", resolved.maxMetaValueBytes)
	}
	return &MemoryStore{
		cfg:     resolved,
		signals: make(map[FeedbackKey][]FeedbackSignal),
	}, nil
}

// Signals returns a snapshot copy of the raw signals stored for key, in
// insertion order. Returns nil if no signals are stored. Tests and
// diagnostic tools can use this to inspect individual signals (kind,
// strength, error class) without computing an aggregate. The returned
// slice is independent of the store's internal storage and safe to
// retain across concurrent Record calls.
func (s *MemoryStore) Signals(key FeedbackKey) []FeedbackSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	signals := s.signals[key]
	if len(signals) == 0 {
		return nil
	}
	out := make([]FeedbackSignal, len(signals))
	for i, sig := range signals {
		out[i] = cloneSignal(sig)
	}
	return out
}

// Get returns the aggregate for key. If no signals are stored, returns a
// neutral aggregate (Score == cfg.neutralScore, other fields zero).
// Otherwise computes Score from the score-bearing subset of signals
// (effective strength != 0) using a [-1,+1]-clipped mean mapped to [0,1].
// Latency signals (default strength 0) contribute to SampleCount only.
func (s *MemoryStore) Get(_ context.Context, key FeedbackKey) (Aggregate, error) {
	s.mu.Lock()
	sigs := append([]FeedbackSignal(nil), s.signals[key]...)
	s.mu.Unlock()

	if len(sigs) == 0 {
		return Aggregate{Score: s.cfg.neutralScore}, nil
	}

	var (
		sum      float64
		scored   int
		latestAt time.Time
	)
	for _, sig := range sigs {
		strength := effectiveStrength(sig)
		if sig.At.After(latestAt) {
			latestAt = sig.At
		}
		if strength == 0 {
			continue
		}
		sum += clip(strength, -1, +1)
		scored++
	}
	if scored == 0 {
		return Aggregate{
			Score:       s.cfg.neutralScore,
			SampleCount: len(sigs),
			UpdatedAt:   latestAt,
		}, nil
	}
	mean := sum / float64(scored)
	return Aggregate{
		Score:       0.5 + 0.5*mean,
		SampleCount: len(sigs),
		ScoredCount: scored,
		UpdatedAt:   latestAt,
	}, nil
}

// effectiveStrength returns the signal's strength, falling back to the
// default for its kind when Strength is nil. An unknown kind returns 0.
func effectiveStrength(sig FeedbackSignal) float64 {
	if sig.Strength != nil {
		return *sig.Strength
	}
	return defaultStrength[sig.Kind]
}

// clip returns x clamped to the closed interval [lo, hi].
func clip(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// Record persists a single signal. Validates the key and signal, defaults
// At to time.Now when zero, deep-copies caller-owned Strength and Meta,
// appends under lock, and applies FIFO retention when the per-key bound
// is finite.
func (s *MemoryStore) Record(_ context.Context, key FeedbackKey, sig FeedbackSignal) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateSignal(sig, s.cfg.maxMetaKeys, s.cfg.maxMetaValueBytes); err != nil {
		return err
	}
	stored := cloneSignal(sig)
	if stored.At.IsZero() {
		stored.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals[key] = append(s.signals[key], stored)
	if s.cfg.maxRetainedSamples > 0 && len(s.signals[key]) > s.cfg.maxRetainedSamples {
		drop := len(s.signals[key]) - s.cfg.maxRetainedSamples
		s.signals[key] = s.signals[key][drop:]
	}
	return nil
}

// RecordBatch persists multiple signals as a single atomic state transition.
// All items are validated up front; if any item fails validation, no item
// is persisted. Defaulting of At (when zero) uses a single time.Now() value
// sampled at the start of the call so all items in the batch share a
// timestamp. Per-key FIFO retention is applied inside the same lock so
// readers never observe a partially-applied batch.
func (s *MemoryStore) RecordBatch(_ context.Context, items []FeedbackItem) error {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]FeedbackItem, len(items))
	for i, item := range items {
		if err := validateKey(item.Key); err != nil {
			return err
		}
		if err := validateSignal(item.Signal, s.cfg.maxMetaKeys, s.cfg.maxMetaValueBytes); err != nil {
			return err
		}
		cloned[i] = FeedbackItem{Key: item.Key, Signal: cloneSignal(item.Signal)}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range cloned {
		if cloned[i].Signal.At.IsZero() {
			cloned[i].Signal.At = now
		}
		k := cloned[i].Key
		s.signals[k] = append(s.signals[k], cloned[i].Signal)
		if s.cfg.maxRetainedSamples > 0 && len(s.signals[k]) > s.cfg.maxRetainedSamples {
			drop := len(s.signals[k]) - s.cfg.maxRetainedSamples
			s.signals[k] = s.signals[k][drop:]
		}
	}
	return nil
}
