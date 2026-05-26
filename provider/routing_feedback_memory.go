// routing_feedback_memory.go provides an in-memory RoutingFeedbackStore.
// It implements bounded per-key retention with FIFO eviction, all-or-nothing
// RecordBatch semantics, and defensive copies of caller-owned Meta and
// Strength on Record. PR3 replaces it with a SQLite-backed store.
package provider

import (
	"context"
	"fmt"
	"sync"
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

// Get returns the aggregate for key. Empty keys yield a neutral aggregate
// (Score == cfg.NeutralScore, all other fields zero). Full aggregation logic
// is implemented in a subsequent task; this skeleton returns neutral.
func (s *MemoryStore) Get(_ context.Context, _ FeedbackKey) (Aggregate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Aggregate{Score: s.cfg.neutralScore}, nil
}

// Record / RecordBatch are implemented in subsequent tasks.
func (s *MemoryStore) Record(_ context.Context, _ FeedbackKey, _ FeedbackSignal) error {
	return fmt.Errorf("provider: MemoryStore.Record not yet implemented")
}

// RecordBatch is implemented in a subsequent task.
func (s *MemoryStore) RecordBatch(_ context.Context, _ []FeedbackItem) error {
	return fmt.Errorf("provider: MemoryStore.RecordBatch not yet implemented")
}
