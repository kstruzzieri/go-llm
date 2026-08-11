package feedback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// SweepInterval is how often expired attribution windows should be swept.
	SweepInterval = 60 * time.Second

	// maxWindowAge is the maximum duration an attribution window stays open.
	maxWindowAge = 300 * time.Second

	// recomputeInterval is the number of signals between automatic
	// aggregate recomputation.
	recomputeInterval = 100

	// weakNegative is the strength applied to chunks in an expired window
	// that received no interaction.
	weakNegative = -0.1
)

// attributionWindow tracks an open retrieval for signal attribution.
type attributionWindow struct {
	retrievalID string
	chunkKeys   []string
	interacted  map[string]bool
	createdAt   time.Time
}

// Collector orchestrates behavioral feedback collection. It manages
// attribution windows, delegates persistence to a SignalStore, and
// runs background maintenance.
type Collector struct {
	store       SignalStore
	atomicStore AtomicSignalStore
	config      CollectorConfig

	mu      sync.Mutex
	windows map[string]*attributionWindow

	signalCount atomic.Int64
	closeOnce   sync.Once
	done        chan struct{}
	wg          sync.WaitGroup
}

// NewCollector creates a Collector backed by the given store and config.
// A background goroutine is started to sweep expired attribution windows;
// call Close to stop it.
func NewCollector(store SignalStore, config CollectorConfig) *Collector {
	cfg := config.withDefaults()
	atomicStore, _ := store.(AtomicSignalStore)
	c := &Collector{
		store:       store,
		atomicStore: atomicStore,
		config:      cfg,
		windows:     make(map[string]*attributionWindow),
		done:        make(chan struct{}),
	}
	c.wg.Add(1)
	go c.sweepLoop()
	return c
}

// NewManualCollector creates a Collector whose expiry and shutdown are driven
// explicitly by SweepExpired and DiscardOpen. It starts no background sweeper.
func NewManualCollector(store AtomicSignalStore, config CollectorConfig) *Collector {
	return &Collector{
		store:       store,
		atomicStore: store,
		config:      config.withDefaults(),
		windows:     make(map[string]*attributionWindow),
	}
}

// Close stops the background goroutine and waits for it to finish.
// Close is safe to call multiple times.
func (c *Collector) Close() {
	if c.done == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.done)
	})
	c.wg.Wait()
}

// RegisterRetrieval opens an attribution window for the given query and
// chunk keys. It returns a unique retrieval ID.
func (c *Collector) RegisterRetrieval(ctx context.Context, query string, chunkKeys []string) (string, error) {
	return c.RegisterRetrievalAt(ctx, query, chunkKeys, time.Now())
}

// RegisterRetrievalAt opens an attribution window at presentedAt.
// It rejects a zero presentedAt.
func (c *Collector) RegisterRetrievalAt(ctx context.Context, query string, chunkKeys []string, presentedAt time.Time) (string, error) {
	if presentedAt.IsZero() {
		return "", fmt.Errorf("feedback: register retrieval: presented time is required")
	}

	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("feedback: generate retrieval id: %w", err)
	}

	if err := c.insertRetrieval(ctx, id, query, chunkKeys, presentedAt); err != nil {
		return "", err
	}

	// Defensive copy so callers cannot mutate the window's key set.
	keysCopy := make([]string, len(chunkKeys))
	copy(keysCopy, chunkKeys)

	c.mu.Lock()
	c.windows[id] = &attributionWindow{
		retrievalID: id,
		chunkKeys:   keysCopy,
		interacted:  make(map[string]bool),
		createdAt:   presentedAt,
	}
	c.mu.Unlock()

	return id, nil
}

// Record persists a signal against an open attribution window.
func (c *Collector) Record(ctx context.Context, signal Signal) error {
	_, err := c.RecordAt(ctx, signal, time.Now())
	return err
}

// RecordAt persists a signal against an open attribution window, using
// observedAt to decide whether the window is still open. It rejects a zero
// observedAt.
//
// Its full completion contract is: `(true, nil)` means the signal batch committed;
// `(true, err)` means it committed but synchronous maintenance recomputation
// failed; `(false, nil)` is a semantic no-op; `(false, err)` means no signal
// committed.
func (c *Collector) RecordAt(ctx context.Context, signal Signal, observedAt time.Time) (bool, error) {
	if observedAt.IsZero() {
		return false, fmt.Errorf("feedback: record: observed time is required")
	}
	if signal.RetrievalID == "" {
		return false, fmt.Errorf("feedback: record: retrieval id is required")
	}

	// ponytail: serialize persistence per collector; split per-window locks only
	// if recording throughput becomes a measured bottleneck.
	c.mu.Lock()
	w, ok := c.windows[signal.RetrievalID]
	if !ok {
		c.mu.Unlock()
		return false, fmt.Errorf("feedback: record: unknown retrieval id %q", signal.RetrievalID)
	}
	if observedAt.Sub(w.createdAt) >= maxWindowAge {
		c.mu.Unlock()
		return false, nil
	}

	strength := signal.effectiveStrength()
	keys := signal.ChunkKeys
	if len(keys) == 0 {
		keys = w.chunkKeys
	}
	if signal.Timestamp.IsZero() {
		signal.Timestamp = observedAt
	}
	if err := c.insertSignals(ctx, signal.RetrievalID, keys, signal.Kind, strength, signal.Timestamp); err != nil {
		c.mu.Unlock()
		return false, err
	}

	for _, key := range keys {
		w.interacted[key] = true
	}
	oldCount := c.signalCount.Load()
	newCount := c.signalCount.Add(int64(len(keys)))
	recompute := newCount/recomputeInterval > oldCount/recomputeInterval
	c.mu.Unlock()

	if recompute {
		if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
			return true, err
		}
	}

	return true, nil
}

// Weights returns the behavioral weight for a single chunk key.
// Returns 0.0 if the system is in cold-start or the chunk is below
// MinRetrievals.
func (c *Collector) Weights(ctx context.Context, chunkKey string) (float64, error) {
	totalSignals, err := c.store.SignalCount(ctx)
	if err != nil {
		return 0, err
	}
	if totalSignals < c.config.WarmupSignals {
		return 0, nil
	}

	agg, err := c.store.GetAggregate(ctx, chunkKey)
	if err != nil {
		return 0, err
	}
	if agg.RetrievalCount < c.config.MinRetrievals {
		return 0, nil
	}

	return agg.WeightedScore, nil
}

// WeightsBatch returns behavioral weights for multiple chunk keys in one
// query. Keys that are below MinRetrievals or that have no aggregate return
// 0.0.
func (c *Collector) WeightsBatch(ctx context.Context, chunkKeys []string) (map[string]float64, error) {
	return weightsBatch(ctx, c.store, c.config, chunkKeys)
}

// sweepLoop runs in a goroutine, expiring attribution windows and applying
// weak negatives for non-interacted chunks.
func (c *Collector) sweepLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			// Final sweep on shutdown.
			c.sweepExpired()
			return
		case <-ticker.C:
			c.sweepExpired()
		}
	}
}

// SweepExpired expires windows at least maxWindowAge old and returns each ID
// durably committed and removed before any error. It rejects a zero now.
func (c *Collector) SweepExpired(ctx context.Context, now time.Time) ([]string, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("feedback: sweep expired: current time is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var expired []*attributionWindow
	for _, w := range c.windows {
		if now.Sub(w.createdAt) >= maxWindowAge {
			expired = append(expired, w)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].createdAt.Equal(expired[j].createdAt) {
			return expired[i].retrievalID < expired[j].retrievalID
		}
		return expired[i].createdAt.Before(expired[j].createdAt)
	})

	removed := make([]string, 0, len(expired))
	for _, w := range expired {
		keys := make([]string, 0, len(w.chunkKeys))
		for _, key := range w.chunkKeys {
			if !w.interacted[key] {
				keys = append(keys, key)
			}
		}
		if err := c.insertSignals(ctx, w.retrievalID, keys, SignalWindowExpired, weakNegative, now); err != nil {
			return removed, err
		}

		delete(c.windows, w.retrievalID)
		removed = append(removed, w.retrievalID)
		if len(keys) == 0 {
			continue
		}
		c.signalCount.Add(int64(len(keys)))
		if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
			return removed, err
		}
	}

	return removed, nil
}

// DiscardOpen removes every open attribution window without writing weak
// negatives.
func (c *Collector) DiscardOpen() {
	c.mu.Lock()
	c.windows = make(map[string]*attributionWindow)
	c.mu.Unlock()
}

// sweepExpired preserves the legacy background sweep entry point.
func (c *Collector) sweepExpired() {
	_, _ = c.SweepExpired(context.Background(), time.Now())
}

func (c *Collector) insertRetrieval(ctx context.Context, id, query string, chunkKeys []string, createdAt time.Time) error {
	if c.atomicStore != nil {
		return c.atomicStore.InsertRetrievalWithCounts(ctx, id, query, chunkKeys, createdAt)
	}
	if err := c.store.InsertRetrieval(ctx, id, query, chunkKeys); err != nil {
		return err
	}
	return c.store.IncrementRetrievalCount(ctx, chunkKeys)
}

func (c *Collector) insertSignals(ctx context.Context, retrievalID string, chunkKeys []string, kind SignalKind, strength float64, createdAt time.Time) error {
	if c.atomicStore != nil {
		return c.atomicStore.InsertSignals(ctx, retrievalID, chunkKeys, kind, strength, createdAt)
	}
	for _, key := range chunkKeys {
		if err := c.store.InsertSignal(ctx, retrievalID, key, kind, strength, createdAt); err != nil {
			return err
		}
	}
	return nil
}

// generateID returns a random 16-byte hex string.
func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("feedback: read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
