package feedback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxWindowAge is the maximum duration an attribution window stays open.
	maxWindowAge = 300 * time.Second

	// sweepInterval is how often the background goroutine checks for
	// expired attribution windows.
	sweepInterval = 60 * time.Second

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
	store  SignalStore
	config CollectorConfig

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
	c := &Collector{
		store:   store,
		config:  cfg,
		windows: make(map[string]*attributionWindow),
		done:    make(chan struct{}),
	}
	c.wg.Add(1)
	go c.sweepLoop()
	return c
}

// Close stops the background goroutine and waits for it to finish.
// Close is safe to call multiple times.
func (c *Collector) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
	c.wg.Wait()
}

// RegisterRetrieval opens an attribution window for the given query and
// chunk keys. It returns a unique retrieval ID.
func (c *Collector) RegisterRetrieval(ctx context.Context, query string, chunkKeys []string) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("feedback: generate retrieval id: %w", err)
	}

	if err := c.store.InsertRetrieval(ctx, id, query, chunkKeys); err != nil {
		return "", err
	}

	if err := c.store.IncrementRetrievalCount(ctx, chunkKeys); err != nil {
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
		createdAt:   time.Now(),
	}
	c.mu.Unlock()

	return id, nil
}

// Record persists a signal against an open attribution window.
func (c *Collector) Record(ctx context.Context, signal Signal) error {
	if signal.RetrievalID == "" {
		return fmt.Errorf("feedback: record: retrieval id is required")
	}

	c.mu.Lock()
	w, ok := c.windows[signal.RetrievalID]
	if ok {
		// Mark interacted chunk keys.
		for _, k := range signal.ChunkKeys {
			w.interacted[k] = true
		}
		// If no explicit chunk keys, mark all.
		if len(signal.ChunkKeys) == 0 {
			for _, k := range w.chunkKeys {
				w.interacted[k] = true
			}
		}
	}
	c.mu.Unlock()

	if !ok {
		return fmt.Errorf("feedback: record: unknown retrieval id %q", signal.RetrievalID)
	}

	strength := signal.effectiveStrength()

	// Determine which chunk keys to record against.
	keys := signal.ChunkKeys
	if len(keys) == 0 {
		keys = w.chunkKeys
	}

	for _, key := range keys {
		if err := c.store.InsertSignal(ctx, signal.RetrievalID, key, signal.Kind, strength, signal.Timestamp); err != nil {
			return err
		}
	}

	oldCount := c.signalCount.Load()
	newCount := c.signalCount.Add(int64(len(keys)))
	// Trigger when the counter crosses an interval boundary, not just on
	// exact multiples. This handles batched signals that skip exact hits.
	if newCount/recomputeInterval > oldCount/recomputeInterval {
		go func() {
			_ = c.store.RecomputeAggregates(context.Background(), c.config.DecayLambda)
		}()
	}

	return nil
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
	result := make(map[string]float64, len(chunkKeys))
	for _, k := range chunkKeys {
		result[k] = 0
	}

	totalSignals, err := c.store.SignalCount(ctx)
	if err != nil {
		return nil, err
	}
	if totalSignals < c.config.WarmupSignals {
		return result, nil
	}

	aggs, err := c.store.GetAggregatesBatch(ctx, chunkKeys)
	if err != nil {
		return nil, err
	}

	for k, agg := range aggs {
		if agg.RetrievalCount >= c.config.MinRetrievals {
			result[k] = agg.WeightedScore
		}
	}

	return result, nil
}

// sweepLoop runs in a goroutine, expiring attribution windows and applying
// weak negatives for non-interacted chunks.
func (c *Collector) sweepLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(sweepInterval)
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

// sweepExpired checks all open windows and expires those past maxWindowAge.
// Weak-negative signals are written for non-interacted chunks, the signal
// counter is bumped, and aggregates are recomputed so the penalties take
// effect immediately.
func (c *Collector) sweepExpired() {
	now := time.Now()
	c.mu.Lock()
	var expired []*attributionWindow
	for id, w := range c.windows {
		if now.Sub(w.createdAt) >= maxWindowAge {
			expired = append(expired, w)
			delete(c.windows, id)
		}
	}
	c.mu.Unlock()

	ctx := context.Background()
	var written int
	for _, w := range expired {
		for _, key := range w.chunkKeys {
			if !w.interacted[key] {
				_ = c.store.InsertSignal(ctx, w.retrievalID, key, "window_expired", weakNegative, time.Time{})
				written++
			}
		}
	}

	if written > 0 {
		c.signalCount.Add(int64(written))
		_ = c.store.RecomputeAggregates(ctx, c.config.DecayLambda)
	}
}

// generateID returns a random 16-byte hex string.
func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("feedback: read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
