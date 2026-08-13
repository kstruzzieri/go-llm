package feedback

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type boundarySignalStore struct {
	*SQLiteSignalStore
	rejectSignals bool
}

func (s *boundarySignalStore) InsertSignal(ctx context.Context, retrievalID, chunkKey string, kind SignalKind, strength float64, createdAt time.Time) error {
	if s.rejectSignals {
		return fmt.Errorf("reject signal")
	}
	return s.SQLiteSignalStore.InsertSignal(ctx, retrievalID, chunkKey, kind, strength, createdAt)
}

func (s *boundarySignalStore) InsertSignals(ctx context.Context, retrievalID string, chunkKeys []string, kind SignalKind, strength float64, createdAt time.Time) error {
	if s.rejectSignals {
		return fmt.Errorf("reject signals")
	}
	return s.SQLiteSignalStore.InsertSignals(ctx, retrievalID, chunkKeys, kind, strength, createdAt)
}

type recomputeStore struct {
	*SQLiteSignalStore
	recomputeCalls int
	recomputeErr   error
}

func (s *recomputeStore) RecomputeAggregates(ctx context.Context, lambda float64) error {
	s.recomputeCalls++
	if s.recomputeErr != nil {
		return s.recomputeErr
	}
	return s.SQLiteSignalStore.RecomputeAggregates(ctx, lambda)
}

type blockingRecomputeStore struct {
	*SQLiteSignalStore
	started chan struct{}
	release chan struct{}
	err     error
}

func (s *blockingRecomputeStore) RecomputeAggregates(ctx context.Context, _ float64) error {
	close(s.started)
	select {
	case <-s.release:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type closeBoundaryStore struct {
	*SQLiteSignalStore
	committed        chan struct{}
	releaseInsert    chan struct{}
	recomputeStarted chan struct{}
	releaseRecompute chan struct{}
}

func (s *closeBoundaryStore) InsertSignals(ctx context.Context, retrievalID string, chunkKeys []string, kind SignalKind, strength float64, createdAt time.Time) error {
	if err := s.SQLiteSignalStore.InsertSignals(ctx, retrievalID, chunkKeys, kind, strength, createdAt); err != nil {
		return err
	}
	close(s.committed)
	select {
	case <-s.releaseInsert:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *closeBoundaryStore) RecomputeAggregates(ctx context.Context, _ float64) error {
	close(s.recomputeStarted)
	select {
	case <-s.releaseRecompute:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type concurrentRecomputeStore struct {
	*SQLiteSignalStore
	mu            sync.Mutex
	calls         int
	active        int
	maxActive     int
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
}

func (s *concurrentRecomputeStore) RecomputeAggregates(context.Context, float64) error {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	switch call {
	case 1:
		close(s.firstStarted)
	case 2:
		close(s.secondStarted)
	}
	s.mu.Unlock()
	if call == 1 {
		<-s.releaseFirst
	}
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return nil
}

func (s *concurrentRecomputeStore) state() (calls, maxActive int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maxActive
}

type legacySignalStore struct {
	SignalStore
}

type failSecondLegacySignalStore struct {
	SignalStore
	insertCalls int
}

func (s *failSecondLegacySignalStore) InsertSignal(ctx context.Context, retrievalID, chunkKey string, kind SignalKind, strength float64, createdAt time.Time) error {
	s.insertCalls++
	if s.insertCalls == 2 {
		return fmt.Errorf("reject second signal")
	}
	return s.SignalStore.InsertSignal(ctx, retrievalID, chunkKey, kind, strength, createdAt)
}

// newTestCollector creates a Collector backed by an in-memory SQLite store
// with the given config. The collector is automatically closed on test
// cleanup.
func newTestCollector(t *testing.T, cfg CollectorConfig) *Collector {
	t.Helper()
	store := newTestStore(t)
	c := NewCollector(store, cfg)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRegisterRetrievalReturnsID(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "test query", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty retrieval ID")
	}
	if len(id) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("id length = %d, want 32", len(id))
	}
}

func TestRegisterRetrievalUniqueIDs(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id1, err := c.RegisterRetrieval(ctx, "q1", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval 1: %v", err)
	}
	id2, err := c.RegisterRetrieval(ctx, "q2", []string{"chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval 2: %v", err)
	}
	if id1 == id2 {
		t.Error("expected unique retrieval IDs")
	}
}

func TestRecordAgainstOpenWindow(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	err = c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
		ChunkKeys:   []string{"chunk-1"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestRecordAtPersistenceFailureDoesNotSuppressExpiry(t *testing.T) {
	ctx := context.Background()
	store := &boundarySignalStore{SQLiteSignalStore: newTestStore(t), rejectSignals: true}
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)

	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	committed, err := c.RecordAt(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id}, presentedAt.Add(time.Second))
	if err == nil || committed {
		t.Fatalf("RecordAt = (%v, %v), want (false, persistence error)", committed, err)
	}

	store.rejectSignals = false
	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(maxWindowAge))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != id {
		t.Fatalf("SweepExpired IDs = %v, want [%s]", expiredIDs, id)
	}

	var expired int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`,
		id, SignalWindowExpired,
	).Scan(&expired); err != nil {
		t.Fatalf("query expiry signal: %v", err)
	}
	if expired != 1 {
		t.Errorf("expiry signal rows = %d, want 1", expired)
	}
}

func TestManualCollectorDoesNotSweepAndCloseWritesNoExpiry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	if c.done != nil {
		t.Fatal("manual collector initialized background-sweeper channel")
	}

	presentedAt := time.Unix(1_700_000_000, 0)
	if _, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt); err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	c.Close()

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("signals after manual Close = %d, want 0", count)
	}
}

func TestManualCollectorDiscardOpenClearsWithoutWeakNegatives(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	c.DiscardOpen()
	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(2*maxWindowAge))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(expiredIDs) != 0 {
		t.Errorf("SweepExpired IDs = %v, want none", expiredIDs)
	}
	if committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second)); err == nil || committed {
		t.Errorf("RecordAt discarded window = (%v, %v), want (false, unknown-ID error)", committed, err)
	}
	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("signals after DiscardOpen = %d, want 0", count)
	}
}

func TestManualCollectorRejectsPositiveAtWindowBoundaryThenSweeps(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	committed, err := c.RecordAt(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id}, presentedAt.Add(300*time.Second))
	if err != nil || committed {
		t.Fatalf("RecordAt at 300s = (%v, %v), want (false, nil)", committed, err)
	}
	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(300*time.Second))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != id {
		t.Fatalf("SweepExpired IDs = %v, want [%s]", expiredIDs, id)
	}

	var kind string
	var strength float64
	if err := store.db.QueryRowContext(ctx,
		`SELECT signal_kind, strength FROM feedback_signals WHERE retrieval_id = ?`, id,
	).Scan(&kind, &strength); err != nil {
		t.Fatalf("query expiry signal: %v", err)
	}
	if kind != string(SignalWindowExpired) || strength != -0.1 {
		t.Errorf("expiry signal = (%q, %v), want (%q, -0.1)", kind, strength, SignalWindowExpired)
	}
}

func TestManualCollectorRejectsZeroExplicitTimes(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	if _, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, time.Time{}); err == nil {
		t.Fatal("RegisterRetrievalAt accepted zero presentedAt")
	}

	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	if committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, time.Time{}); err == nil || committed {
		t.Fatalf("RecordAt with zero observedAt = (%v, %v), want (false, error)", committed, err)
	}
	if _, err := c.SweepExpired(ctx, time.Time{}); err == nil {
		t.Fatal("SweepExpired accepted zero now")
	}
}

func TestManualCollectorPersistsExplicitTimesAndDistinctLaterSignals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.UnixMilli(1_700_000_000_123)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	firstSignalAt := time.UnixMilli(1_700_000_001_234)
	committed, err := c.RecordAt(ctx, Signal{
		Kind:        SignalFileOpened,
		RetrievalID: id,
		Timestamp:   firstSignalAt,
	}, presentedAt.Add(time.Second))
	if err != nil || !committed {
		t.Fatalf("first RecordAt = (%v, %v), want (true, nil)", committed, err)
	}
	secondObservedAt := presentedAt.Add(2 * time.Second)
	committed, err = c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, secondObservedAt)
	if err != nil || !committed {
		t.Fatalf("second RecordAt = (%v, %v), want (true, nil)", committed, err)
	}

	var retrievalAt int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT created_at FROM feedback_retrievals WHERE retrieval_id = ?`, id,
	).Scan(&retrievalAt); err != nil {
		t.Fatalf("query retrieval timestamp: %v", err)
	}
	if retrievalAt != presentedAt.UnixMilli() {
		t.Errorf("retrieval created_at = %d, want %d", retrievalAt, presentedAt.UnixMilli())
	}
	rows, err := store.db.QueryContext(ctx,
		`SELECT signal_kind, created_at FROM feedback_signals WHERE retrieval_id = ? ORDER BY rowid`, id)
	if err != nil {
		t.Fatalf("query signal timestamps: %v", err)
	}
	defer func() { _ = rows.Close() }()
	want := []struct {
		kind SignalKind
		at   int64
	}{
		{SignalFileOpened, firstSignalAt.UnixMilli()},
		{SignalCodeKept, secondObservedAt.UnixMilli()},
	}
	got := 0
	for ; rows.Next(); got++ {
		if got >= len(want) {
			t.Fatal("more signal rows than expected")
		}
		var kind string
		var at int64
		if err := rows.Scan(&kind, &at); err != nil {
			t.Fatalf("scan signal timestamp: %v", err)
		}
		if kind != string(want[got].kind) || at != want[got].at {
			t.Errorf("signal %d = (%q, %d), want (%q, %d)", got, kind, at, want[got].kind, want[got].at)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate signal timestamps: %v", err)
	}
	if got != len(want) {
		t.Errorf("signal rows = %d, want %d", got, len(want))
	}
}

func TestManualCollectorRecordAtUsesObservedAtForWindowAge(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	committed, err := c.RecordAt(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
		Timestamp:   presentedAt.Add(time.Second),
	}, presentedAt.Add(maxWindowAge))
	if err != nil || committed {
		t.Fatalf("RecordAt = (%v, %v), want (false, nil)", committed, err)
	}
}

func TestManualCollectorUsesExportedSweepCadence(t *testing.T) {
	if SweepInterval != 60*time.Second {
		t.Fatalf("SweepInterval = %v, want 60s", SweepInterval)
	}
}

func TestManualCollectorRegisterRetrievalIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.db.SetMaxOpenConns(1)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER abort_collector_registration
		BEFORE INSERT ON feedback_aggregates
		WHEN NEW.chunk_key = 'chunk-2'
		BEGIN
			SELECT RAISE(ABORT, 'second aggregate');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1", "chunk-2"}, presentedAt); err == nil {
		t.Fatal("RegisterRetrievalAt succeeded, want trigger error")
	}
	var retrievals, aggregates int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_retrievals`).Scan(&retrievals); err != nil {
		t.Fatalf("query retrievals: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_aggregates`).Scan(&aggregates); err != nil {
		t.Fatalf("query aggregates: %v", err)
	}
	if retrievals != 0 || aggregates != 0 {
		t.Fatalf("rows after failed registration = (%d retrievals, %d aggregates), want zero", retrievals, aggregates)
	}

	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER abort_collector_registration`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1", "chunk-2"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt after trigger removal: %v", err)
	}
	if committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second)); err != nil || !committed {
		t.Fatalf("RecordAt after registration recovery = (%v, %v), want (true, nil)", committed, err)
	}
}

func TestRecordAtPersistenceIsAtomicAndStateAdvancesAfterCommit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.db.SetMaxOpenConns(1)
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1", "chunk-2"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER abort_collector_signal
		BEFORE INSERT ON feedback_signals
		WHEN NEW.chunk_key = 'chunk-2'
		BEGIN
			SELECT RAISE(ABORT, 'second signal');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second))
	if err == nil || committed {
		t.Fatalf("RecordAt = (%v, %v), want (false, trigger error)", committed, err)
	}
	var signals int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ?`, id,
	).Scan(&signals); err != nil {
		t.Fatalf("query signals after rollback: %v", err)
	}
	if signals != 0 {
		t.Fatalf("signals after rollback = %d, want 0", signals)
	}

	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER abort_collector_signal`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	committed, err = c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(2*time.Second))
	if err != nil || !committed {
		t.Fatalf("RecordAt after trigger removal = (%v, %v), want (true, nil)", committed, err)
	}
	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(maxWindowAge))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != id {
		t.Fatalf("SweepExpired IDs = %v, want [%s]", expiredIDs, id)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ?`, id,
	).Scan(&signals); err != nil {
		t.Fatalf("query signals after commit: %v", err)
	}
	if signals != 2 {
		t.Errorf("signals after commit and sweep = %d, want 2 (no weak negatives)", signals)
	}
}

func TestRecordAtPersistenceLegacyPartialWriteDoesNotMarkInteractions(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	store := &failSecondLegacySignalStore{SignalStore: base}
	c := NewCollector(store, CollectorConfig{})
	t.Cleanup(c.Close)
	presentedAt := time.Now()
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1", "chunk-2"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second))
	if err == nil || committed {
		t.Fatalf("RecordAt = (%v, %v), want (false, second-key error)", committed, err)
	}
	var firstRows, secondRows int
	if err := base.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND chunk_key = 'chunk-1'`, id,
	).Scan(&firstRows); err != nil {
		t.Fatalf("query first key: %v", err)
	}
	if err := base.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND chunk_key = 'chunk-2'`, id,
	).Scan(&secondRows); err != nil {
		t.Fatalf("query second key: %v", err)
	}
	if firstRows != 1 || secondRows != 0 {
		t.Fatalf("raw signal rows = (%d first, %d second), want (1, 0)", firstRows, secondRows)
	}
	c.mu.Lock()
	interactions := len(c.windows[id].interacted)
	c.mu.Unlock()
	if interactions != 0 {
		t.Errorf("interaction marks after partial write = %d, want 0", interactions)
	}
}

func TestRecordAtThresholdCrossingRecomputesSynchronously(t *testing.T) {
	ctx := context.Background()
	store := &recomputeStore{SQLiteSignalStore: newTestStore(t)}
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	keys := make([]string, 99)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%03d", i)
	}
	id, err := c.RegisterRetrievalAt(ctx, "q", keys, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	if committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second)); err != nil || !committed {
		t.Fatalf("RecordAt 99 signals = (%v, %v), want (true, nil)", committed, err)
	}
	if store.recomputeCalls != 0 {
		t.Fatalf("recompute calls after 99 signals = %d, want 0", store.recomputeCalls)
	}
	id, err = c.RegisterRetrievalAt(ctx, "q2", []string{"chunk-100"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt second window: %v", err)
	}
	if committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second)); err != nil || !committed {
		t.Fatalf("RecordAt threshold signal = (%v, %v), want (true, nil)", committed, err)
	}
	if store.recomputeCalls != 1 {
		t.Errorf("recompute calls after threshold crossing = %d, want 1", store.recomputeCalls)
	}
}

func TestRecordAtReturnsCommittedWhenSynchronousRecomputeFails(t *testing.T) {
	ctx := context.Background()
	store := &recomputeStore{SQLiteSignalStore: newTestStore(t), recomputeErr: fmt.Errorf("recompute failed")}
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	keys := make([]string, 100)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%03d", i)
	}
	id, err := c.RegisterRetrievalAt(ctx, "q", keys, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	committed, err := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second))
	if err == nil || !committed {
		t.Fatalf("RecordAt = (%v, %v), want (true, recompute error)", committed, err)
	}
	count, countErr := store.SignalCount(ctx)
	if countErr != nil {
		t.Fatalf("SignalCount: %v", countErr)
	}
	if count != 100 {
		t.Fatalf("signal rows after recompute error = %d, want 100", count)
	}

	store.recomputeErr = nil
	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(maxWindowAge))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != id {
		t.Fatalf("SweepExpired IDs = %v, want [%s]", expiredIDs, id)
	}
	count, countErr = store.SignalCount(ctx)
	if countErr != nil {
		t.Fatalf("SignalCount after sweep: %v", countErr)
	}
	if count != 100 {
		t.Errorf("signal rows after sweep = %d, want 100; interaction state did not advance", count)
	}
}

func TestSweepExpiredReturnsCommittedIDWhenRecomputeFails(t *testing.T) {
	ctx := context.Background()
	store := &recomputeStore{SQLiteSignalStore: newTestStore(t), recomputeErr: fmt.Errorf("recompute failed")}
	c := NewManualCollector(store, CollectorConfig{})
	presentedAt := time.Unix(1_700_000_000, 0)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	expiredIDs, err := c.SweepExpired(ctx, presentedAt.Add(maxWindowAge))
	if err == nil {
		t.Fatal("SweepExpired succeeded, want recompute error")
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != id {
		t.Fatalf("SweepExpired IDs = %v, want committed ID [%s]", expiredIDs, id)
	}
	if committed, recordErr := c.RecordAt(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}, presentedAt.Add(time.Second)); recordErr == nil || committed {
		t.Errorf("RecordAt removed window = (%v, %v), want (false, unknown-ID error)", committed, recordErr)
	}
	var expired int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`,
		id, SignalWindowExpired,
	).Scan(&expired); err != nil {
		t.Fatalf("query expiry signal: %v", err)
	}
	if expired != 1 {
		t.Errorf("expiry signal rows = %d, want 1", expired)
	}
}

func TestSweepExpiredReturnsPartialSuccessAndLeavesFailedWindowOpen(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.db.SetMaxOpenConns(1)
	c := NewManualCollector(store, CollectorConfig{})
	firstAt := time.Unix(1_700_000_000, 0)
	secondAt := firstAt.Add(time.Second)
	firstID, err := c.RegisterRetrievalAt(ctx, "first", []string{"chunk-1"}, firstAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt first: %v", err)
	}
	secondID, err := c.RegisterRetrievalAt(ctx, "second", []string{"chunk-2"}, secondAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt second: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER abort_second_expiry
		BEFORE INSERT ON feedback_signals
		WHEN NEW.chunk_key = 'chunk-2'
		BEGIN
			SELECT RAISE(ABORT, 'second expiry');
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	expiredIDs, err := c.SweepExpired(ctx, secondAt.Add(maxWindowAge))
	if err == nil {
		t.Fatal("SweepExpired succeeded, want second-window trigger error")
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != firstID {
		t.Fatalf("SweepExpired IDs = %v, want first committed ID [%s]", expiredIDs, firstID)
	}
	var firstRows, secondRows int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ?`, firstID,
	).Scan(&firstRows); err != nil {
		t.Fatalf("query first signal rows: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ?`, secondID,
	).Scan(&secondRows); err != nil {
		t.Fatalf("query second signal rows: %v", err)
	}
	if firstRows != 1 || secondRows != 0 {
		t.Fatalf("raw expiry rows = (%d first, %d second), want (1, 0)", firstRows, secondRows)
	}

	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER abort_second_expiry`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	expiredIDs, err = c.SweepExpired(ctx, secondAt.Add(maxWindowAge))
	if err != nil {
		t.Fatalf("SweepExpired retry: %v", err)
	}
	if len(expiredIDs) != 1 || expiredIDs[0] != secondID {
		t.Fatalf("SweepExpired retry IDs = %v, want failed ID [%s]", expiredIDs, secondID)
	}
}

func TestSweepExpiredRejectsBackgroundCollectorAndTrackedLoopStillSweeps(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewCollector(store, CollectorConfig{})
	defer c.Close()
	presentedAt := time.Now().Add(-maxWindowAge)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}

	expiredIDs, err := c.SweepExpired(ctx, time.Now())
	if err == nil || !strings.Contains(err.Error(), "sweep owned by background loop") {
		t.Fatalf("SweepExpired = (%v, %v), want no IDs and ownership error", expiredIDs, err)
	}
	if len(expiredIDs) != 0 {
		t.Errorf("SweepExpired IDs = %v, want none", expiredIDs)
	}
	var expired int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`, id, SignalWindowExpired,
	).Scan(&expired); err != nil {
		t.Fatalf("query public-sweep rows: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expiry rows after rejected public sweep = %d, want 0", expired)
	}

	c.Close()
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`, id, SignalWindowExpired,
	).Scan(&expired); err != nil {
		t.Fatalf("query tracked-loop rows: %v", err)
	}
	if expired != 1 {
		t.Errorf("expiry rows after tracked-loop final sweep = %d, want 1", expired)
	}
}

func TestLegacyCollectorConstructorAndMethodsRemainCompatible(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	store := legacySignalStore{SignalStore: base}
	c := NewCollector(store, CollectorConfig{})
	t.Cleanup(c.Close)

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	explicitAt := time.UnixMilli(1_700_000_000_123)
	if err := c.Record(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id, Timestamp: explicitAt}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var rows int
	if err := base.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND created_at = ?`,
		id, explicitAt.UnixMilli(),
	).Scan(&rows); err != nil {
		t.Fatalf("query legacy signal rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("legacy signal rows = %d, want 2", rows)
	}

	before := time.Now().Add(-time.Second)
	if err := c.Record(ctx, Signal{Kind: SignalFileOpened, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record with default timestamp: %v", err)
	}
	var defaultedAt int64
	if err := base.db.QueryRowContext(ctx,
		`SELECT created_at FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`,
		id, SignalFileOpened,
	).Scan(&defaultedAt); err != nil {
		t.Fatalf("query defaulted timestamp: %v", err)
	}
	if defaultedAt <= before.UnixMilli() {
		t.Errorf("defaulted signal timestamp = %d, want after %d", defaultedAt, before.UnixMilli())
	}
}

func TestLegacyRecordReturnsAfterCommitAndCloseTracksRecompute(t *testing.T) {
	ctx := context.Background()
	store := &blockingRecomputeStore{
		SQLiteSignalStore: newTestStore(t),
		started:           make(chan struct{}),
		release:           make(chan struct{}),
		err:               fmt.Errorf("recompute failed"),
	}
	c := NewCollector(store, CollectorConfig{})
	keys := make([]string, recomputeInterval)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%03d", i)
	}
	id, err := c.RegisterRetrieval(ctx, "q", keys)
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	recordDone := make(chan error, 1)
	go func() {
		recordDone <- c.Record(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id})
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		c.Close()
		t.Fatal("maintenance recompute did not start")
	}
	count, err := store.SignalCount(ctx)
	if err != nil || count != recomputeInterval {
		close(store.release)
		<-recordDone
		c.Close()
		t.Fatalf("committed signals = %d, %v; want %d, nil", count, err, recomputeInterval)
	}
	select {
	case err := <-recordDone:
		if err != nil {
			close(store.release)
			c.Close()
			t.Fatalf("Record returned maintenance error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(store.release)
		<-recordDone
		c.Close()
		t.Fatal("Record waited for maintenance recompute")
	}

	closeDone := make(chan struct{})
	go func() {
		c.Close()
		close(closeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.lifecycleMu.Lock()
		closing := c.closed
		c.lifecycleMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(store.release)
			t.Fatal("Close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closeDone:
		close(store.release)
		t.Fatal("Close returned before tracked recompute completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after recompute completed")
	}
}

func TestLegacyRecordCommittedBeforeCloseStillRunsTrackedRecompute(t *testing.T) {
	ctx := context.Background()
	store := &closeBoundaryStore{
		SQLiteSignalStore: newTestStore(t),
		committed:         make(chan struct{}),
		releaseInsert:     make(chan struct{}),
		recomputeStarted:  make(chan struct{}),
		releaseRecompute:  make(chan struct{}),
	}
	c := NewCollector(store, CollectorConfig{})
	keys := make([]string, recomputeInterval)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%03d", i)
	}
	id, err := c.RegisterRetrieval(ctx, "q", keys)
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	recordDone := make(chan error, 1)
	go func() { recordDone <- c.Record(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}) }()
	select {
	case <-store.committed:
	case <-time.After(time.Second):
		close(store.releaseInsert)
		c.Close()
		t.Fatal("signal batch did not commit")
	}
	closeDone := make(chan struct{})
	go func() {
		c.Close()
		close(closeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		c.lifecycleMu.Lock()
		closing := c.closed
		c.lifecycleMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(store.releaseInsert)
			t.Fatal("Close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	close(store.releaseInsert)
	if err := <-recordDone; err != nil {
		close(store.releaseRecompute)
		t.Fatalf("Record: %v", err)
	}
	select {
	case <-store.recomputeStarted:
	case <-closeDone:
		close(store.releaseRecompute)
		t.Fatal("Close returned without recomputing the committed threshold batch")
	case <-time.After(time.Second):
		close(store.releaseRecompute)
		t.Fatal("committed threshold batch did not start recomputation")
	}
	select {
	case <-closeDone:
		close(store.releaseRecompute)
		t.Fatal("Close returned before recomputation completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.releaseRecompute)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after recomputation completed")
	}
}

func TestLegacyRecordSerializesTrackedRecomputes(t *testing.T) {
	ctx := context.Background()
	store := &concurrentRecomputeStore{
		SQLiteSignalStore: newTestStore(t),
		firstStarted:      make(chan struct{}),
		secondStarted:     make(chan struct{}),
		releaseFirst:      make(chan struct{}),
	}
	c := NewCollector(store, CollectorConfig{})
	for window := 0; window < 2; window++ {
		keys := make([]string, recomputeInterval)
		for i := range keys {
			keys[i] = fmt.Sprintf("chunk-%d-%03d", window, i)
		}
		id, err := c.RegisterRetrieval(ctx, fmt.Sprintf("q%d", window), keys)
		if err != nil {
			t.Fatalf("RegisterRetrieval %d: %v", window, err)
		}
		if err := c.Record(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}); err != nil {
			t.Fatalf("Record %d: %v", window, err)
		}
		if window == 0 {
			select {
			case <-store.firstStarted:
			case <-time.After(time.Second):
				close(store.releaseFirst)
				c.Close()
				t.Fatal("first recompute did not start")
			}
		}
	}
	select {
	case <-store.secondStarted:
		calls, maxActive := store.state()
		close(store.releaseFirst)
		c.Close()
		t.Fatalf("recomputes overlapped before release: calls=%d max_active=%d", calls, maxActive)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.releaseFirst)
	select {
	case <-store.secondStarted:
	case <-time.After(time.Second):
		c.Close()
		t.Fatal("second recompute did not run")
	}
	c.Close()
	if calls, maxActive := store.state(); calls != 2 || maxActive != 1 {
		t.Fatalf("recompute state = calls %d, max_active %d; want 2, 1", calls, maxActive)
	}
}

func TestLegacyRecordRejectsAfterClose(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := NewCollector(store, CollectorConfig{})
	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	c.Close()
	if err := c.Record(ctx, Signal{Kind: SignalCodeKept, RetrievalID: id}); err == nil || !strings.Contains(err.Error(), "collector is closed") {
		t.Fatalf("Record after Close = %v, want closed collector error", err)
	}
	if count, err := store.SignalCount(ctx); err != nil || count != 0 {
		t.Fatalf("signals after rejected Record = %d, %v; want 0, nil", count, err)
	}
}

func TestLegacyCollectorCloseRunsFinalSweepThroughFallbackStore(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	c := NewCollector(legacySignalStore{SignalStore: base}, CollectorConfig{})
	presentedAt := time.Now().Add(-maxWindowAge)
	id, err := c.RegisterRetrievalAt(ctx, "q", []string{"chunk-1"}, presentedAt)
	if err != nil {
		t.Fatalf("RegisterRetrievalAt: %v", err)
	}
	c.Close()

	var expired int
	if err := base.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = ? AND signal_kind = ?`,
		id, SignalWindowExpired,
	).Scan(&expired); err != nil {
		t.Fatalf("query legacy expiry signal: %v", err)
	}
	if expired != 1 {
		t.Errorf("legacy expiry signal rows = %d, want 1", expired)
	}
}

func TestRecordUnknownWindow(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	err := c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: "nonexistent",
		ChunkKeys:   []string{"chunk-1"},
	})
	if err == nil {
		t.Error("expected error for unknown retrieval ID")
	}
}

func TestRecordMissingRetrievalID(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	err := c.Record(ctx, Signal{
		Kind:      SignalCompletionAccepted,
		ChunkKeys: []string{"chunk-1"},
	})
	if err == nil {
		t.Error("expected error for empty retrieval ID")
	}
}

func TestRecordBroadcastsToAllChunks(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	// Record with empty ChunkKeys should broadcast to all chunks.
	err = c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Verify signal count: 3 signals (one per chunk).
	count, err := c.store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 3 {
		t.Errorf("SignalCount = %d, want 3", count)
	}
}

func TestWeightsColdStart(t *testing.T) {
	// WarmupSignals=100 means we need at least 100 signals before weights
	// are non-zero.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 100, MinRetrievals: 1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights during warmup = %f, want 0", w)
	}
}

func TestWeightsBelowMinRetrievals(t *testing.T) {
	// Set warmup to 0 so cold-start does not interfere, but require 5
	// retrievals.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 5})
	ctx := context.Background()

	// Register only 1 retrieval (below MinRetrievals=5).
	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Force recompute so weighted_score is populated.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights below MinRetrievals = %f, want 0", w)
	}
}

func TestWeightsAboveThresholds(t *testing.T) {
	// Warmup=0 and MinRetrievals=1 so a single retrieval+signal suffices.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Force recompute.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// Single recent signal of strength 0.8, decay negligible.
	if w < 0.7 || w > 0.9 {
		t.Errorf("Weights = %f, expected ~0.8", w)
	}
}

func TestWeightsBatch(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record chunk-1: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionRejected, RetrievalID: id, ChunkKeys: []string{"chunk-2"}}); err != nil {
		t.Fatalf("Record chunk-2: %v", err)
	}

	// Force recompute.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	weights, err := c.WeightsBatch(ctx, []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}

	if len(weights) != 3 {
		t.Fatalf("WeightsBatch returned %d entries, want 3", len(weights))
	}

	if weights["chunk-1"] < 0.7 || weights["chunk-1"] > 0.9 {
		t.Errorf("chunk-1 weight = %f, expected ~0.8", weights["chunk-1"])
	}
	if weights["chunk-2"] > -0.7 || weights["chunk-2"] < -0.9 {
		t.Errorf("chunk-2 weight = %f, expected ~-0.8", weights["chunk-2"])
	}
	if weights["chunk-3"] != 0 {
		t.Errorf("chunk-3 weight = %f, expected 0", weights["chunk-3"])
	}
}

func TestWeightsBatchColdStart(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 100, MinRetrievals: 1})
	ctx := context.Background()

	weights, err := c.WeightsBatch(ctx, []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	for k, w := range weights {
		if w != 0 {
			t.Errorf("chunk %q weight = %f during warmup, want 0", k, w)
		}
	}
}

func TestCollectorClose(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	// Close should not panic or hang.
	c.Close()
}

func TestRecordCustomStrength(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	// Use a custom strength instead of the default.
	if err := c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
		ChunkKeys:   []string{"chunk-1"},
		Strength:    0.5,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// Custom strength of 0.5 should be used.
	if w < 0.4 || w > 0.6 {
		t.Errorf("Weights = %f, expected ~0.5", w)
	}
}

func TestMultipleSignalsSameChunk(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.0})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	signals := []Signal{
		{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}, // +0.8
		{Kind: SignalCodeKept, RetrievalID: id, ChunkKeys: []string{"chunk-1"}},           // +0.6
		{Kind: SignalFileOpened, RetrievalID: id, ChunkKeys: []string{"chunk-1"}},         // +0.3
	}
	for _, sig := range signals {
		if err := c.Record(ctx, sig); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	if err := c.store.RecomputeAggregates(ctx, 0.0); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// With decay=0, score = 0.8 + 0.6 + 0.3 = 1.7 exactly.
	if w < 1.6 || w > 1.8 {
		t.Errorf("Weights = %f, expected ~1.7", w)
	}
}

func TestSweepExpiredTriggersRecompute(t *testing.T) {
	// Verify that weak-negative signals written by sweepExpired are reflected
	// in materialized aggregates immediately (not deferred to later Record).
	store := newTestStore(t)
	cfg := CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.0}
	c := NewCollector(store, cfg)
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	// Manually expire the window so sweepExpired writes weak negatives.
	c.mu.Lock()
	c.windows[id].createdAt = time.Now().Add(-2 * maxWindowAge)
	c.mu.Unlock()

	c.sweepExpired()

	// The weak-negative should already be materialized in aggregates.
	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	if agg.WeightedScore >= 0 {
		t.Errorf("weighted_score = %f, expected negative after expiry sweep", agg.WeightedScore)
	}
}

func TestWeightsNonExistentChunk(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1})
	ctx := context.Background()

	w, err := c.Weights(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights = %f, want 0 for nonexistent chunk", w)
	}
}
