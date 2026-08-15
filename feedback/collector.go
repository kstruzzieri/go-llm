package feedback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
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

	signalCount   int64
	signalCountOK bool
	closeOnce     sync.Once
	done          chan struct{}
	wg            sync.WaitGroup
	lifecycleMu   sync.Mutex
	maintenanceMu sync.Mutex
	closed        bool
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
		c.lifecycleMu.Lock()
		c.closed = true
		close(c.done)
		c.lifecycleMu.Unlock()
	})
	c.wg.Wait()
}

// RegisterRetrieval opens an attribution window for the given query and
// chunk keys. It returns a unique retrieval ID.
func (c *Collector) RegisterRetrieval(ctx context.Context, query string, chunkKeys []string) (string, error) {
	return c.RegisterRetrievalAt(ctx, query, chunkKeys, time.Now())
}

// RegisterRetrievalAt opens an attribution window at presentedAt. It rejects a
// zero presentedAt. With an atomic store, the retrieval and count updates
// commit together. Its legacy two-call fallback can leave the retrieval row
// committed when count updates fail. Neither path installs an in-memory window
// on error.
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

// Record persists a signal against an open attribution window. Background
// collectors schedule maintenance asynchronously and ignore its error; manual
// collectors preserve RecordAt's synchronous, error-reporting behavior.
func (c *Collector) Record(ctx context.Context, signal Signal) error {
	if c.done == nil {
		_, err := c.RecordAt(ctx, signal, time.Now())
		return err
	}
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		return fmt.Errorf("feedback: record: collector is closed")
	}
	c.wg.Add(1)
	c.lifecycleMu.Unlock()
	committed, recompute, err := c.recordBatchAt(ctx, []Signal{signal}, time.Now())
	if err != nil || !recompute {
		c.wg.Done()
		if len(committed) > 0 {
			return nil
		}
		return err
	}
	go func() {
		defer c.wg.Done()
		_ = c.recompute(context.Background())
	}()
	return nil
}

// RecordAt persists a signal against an open attribution window, using
// observedAt to decide whether the window is still open. It rejects a zero
// observedAt.
//
// With an atomic store (always true for `NewManualCollector` and every current
// production `SignalStore`), its completion contract is: `(true, nil)` means the
// signal batch committed; `(true, err)` means it committed but synchronous
// maintenance recomputation failed; `(false, nil)` is a semantic no-op;
// `(false, err)` means no signal committed. With a legacy non-atomic store,
// persistence is best-effort per key: `(false, err)` may leave earlier keys in
// the batch committed, but interaction state is never marked on any error.
func (c *Collector) RecordAt(ctx context.Context, signal Signal, observedAt time.Time) (bool, error) {
	committed, recompute, err := c.recordBatchAt(ctx, []Signal{signal}, observedAt)
	if err != nil || !recompute {
		return len(committed) > 0, err
	}
	return true, c.recompute(ctx)
}

// RecordBatchAt atomically records one observer event against all matching
// attribution windows. Returned signals are exactly those durably committed.
// It rejects collectors constructed without an AtomicSignalStore.
func (c *Collector) RecordBatchAt(ctx context.Context, signals []Signal, observedAt time.Time) ([]Signal, error) {
	if c.atomicStore == nil {
		return nil, fmt.Errorf("feedback: record batch: atomic store is required")
	}
	committed, recompute, err := c.recordBatchAt(ctx, signals, observedAt)
	if err != nil || !recompute {
		return committed, err
	}
	return committed, c.recompute(ctx)
}

func (c *Collector) recordBatchAt(ctx context.Context, signals []Signal, observedAt time.Time) ([]Signal, bool, error) {
	if observedAt.IsZero() {
		return nil, false, fmt.Errorf("feedback: record: observed time is required")
	}

	// ponytail: serialize persistence per collector; split per-window locks only
	// if recording throughput becomes a measured bottleneck.
	c.mu.Lock()
	defer c.mu.Unlock()

	committed := make([]Signal, 0, len(signals))
	windows := make([]*attributionWindow, 0, len(signals))
	for _, signal := range signals {
		if signal.RetrievalID == "" {
			return nil, false, fmt.Errorf("feedback: record: retrieval id is required")
		}
		w, ok := c.windows[signal.RetrievalID]
		if !ok {
			return nil, false, fmt.Errorf("feedback: record: unknown retrieval id %q", signal.RetrievalID)
		}
		if observedAt.Sub(w.createdAt) >= maxWindowAge {
			continue
		}
		keys := signal.ChunkKeys
		if len(keys) == 0 {
			keys = w.chunkKeys
		}
		signal.ChunkKeys = append([]string(nil), keys...)
		if signal.Timestamp.IsZero() {
			signal.Timestamp = observedAt
		}
		signal.Strength = signal.effectiveStrength()
		committed = append(committed, signal)
		windows = append(windows, w)
	}
	if len(committed) == 0 {
		return nil, false, nil
	}
	if !c.signalCountOK {
		count, err := c.store.SignalCount(ctx)
		if err != nil {
			return nil, false, err
		}
		c.signalCount = int64(count)
		c.signalCountOK = true
	}
	var err error
	if len(committed) == 1 {
		signal := committed[0]
		err = c.insertSignals(ctx, signal.RetrievalID, signal.ChunkKeys, signal.Kind, signal.Strength, signal.Timestamp)
	} else if c.atomicStore == nil {
		err = fmt.Errorf("feedback: record batch: atomic store is required")
	} else {
		err = c.atomicStore.InsertSignalBatch(ctx, committed)
	}
	if err != nil {
		return nil, false, err
	}

	for i, signal := range committed {
		for _, key := range signal.ChunkKeys {
			windows[i].interacted[key] = true
		}
	}
	oldCount := c.signalCount
	count, err := c.store.SignalCount(ctx)
	if err != nil {
		return committed, false, err
	}
	newCount := int64(count)
	c.signalCount = newCount
	recompute := newCount/recomputeInterval > oldCount/recomputeInterval

	return committed, recompute, nil
}

func (c *Collector) recompute(ctx context.Context) error {
	c.maintenanceMu.Lock()
	defer c.maintenanceMu.Unlock()
	return c.store.RecomputeAggregates(ctx, c.config.DecayLambda)
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

// SweepExpired manually expires windows at least maxWindowAge old. It rejects
// collectors that own a background sweeper and rejects a zero now. With an
// atomic store, returned IDs are exactly the windows durably committed and
// removed before any error; the failed window remains open with no weak
// negatives committed. With a legacy non-atomic store, persistence is
// best-effort per key: an error may leave earlier weak negatives committed,
// while the failed window remains open and absent from the returned IDs.
func (c *Collector) SweepExpired(ctx context.Context, now time.Time) ([]string, error) {
	if c.done != nil {
		return nil, fmt.Errorf("feedback: sweep expired: sweep owned by background loop")
	}
	return c.sweepExpiredAt(ctx, now)
}

func (c *Collector) sweepExpiredAt(ctx context.Context, now time.Time) ([]string, error) {
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
		c.signalCount += int64(len(keys))
		if err := c.recompute(ctx); err != nil {
			return removed, err
		}
	}

	return removed, nil
}

// DiscardOpen is for manual collectors only. It removes every open attribution
// window without writing weak negatives.
func (c *Collector) DiscardOpen() {
	c.mu.Lock()
	c.windows = make(map[string]*attributionWindow)
	c.mu.Unlock()
}

// sweepExpired preserves the legacy background sweep entry point.
func (c *Collector) sweepExpired() {
	_, _ = c.sweepExpiredAt(context.Background(), time.Now())
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
