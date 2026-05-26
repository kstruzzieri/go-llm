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
	if resolved.neutralScore < 0 || resolved.neutralScore > 1 {
		return nil, fmt.Errorf("provider: NeutralScore %v out of range [0,1]", resolved.neutralScore)
	}
	return &MemoryStore{
		cfg:     resolved,
		signals: make(map[FeedbackKey][]FeedbackSignal),
	}, nil
}

// Get returns the aggregate for key. Until Task 6 lands the full formula,
// this returns SampleCount = len(signals[key]), UpdatedAt = latest At, and
// Score = configured neutral. The Get-side formula upgrade lands in Task 6.
func (s *MemoryStore) Get(_ context.Context, key FeedbackKey) (Aggregate, error) {
	s.mu.Lock()
	sigs := s.signals[key]
	count := len(sigs)
	var latestAt time.Time
	for _, sig := range sigs {
		if sig.At.After(latestAt) {
			latestAt = sig.At
		}
	}
	s.mu.Unlock()
	if count == 0 {
		return Aggregate{Score: s.cfg.neutralScore}, nil
	}
	return Aggregate{
		Score:       s.cfg.neutralScore,
		SampleCount: count,
		UpdatedAt:   latestAt,
	}, nil
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

// RecordBatch is implemented in a subsequent task.
func (s *MemoryStore) RecordBatch(_ context.Context, _ []FeedbackItem) error {
	return fmt.Errorf("provider: MemoryStore.RecordBatch not yet implemented")
}
