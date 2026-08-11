package feedback

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteSignalStore {
	t.Helper()
	db := openTestDB(t)
	store, err := NewSignalStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	return store
}

func TestInsertRetrievalAndSignal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "test query", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}

	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 1 {
		t.Errorf("SignalCount = %d, want 1", count)
	}
}

func TestSQLiteSignalStoreInsertRetrievalWithCountsAtomic(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.db.SetMaxOpenConns(1)

	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER abort_second_aggregate
		BEFORE INSERT ON feedback_aggregates
		WHEN NEW.chunk_key = 'chunk-2'
		BEGIN
			SELECT RAISE(ABORT, 'second aggregate');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}

	err := store.InsertRetrievalWithCounts(ctx, "r-atomic", "query", []string{"chunk-1", "chunk-2"}, time.UnixMilli(1234))
	if err == nil {
		t.Fatal("InsertRetrievalWithCounts succeeded, want trigger error")
	}

	var retrievalRows int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_retrievals WHERE retrieval_id = 'r-atomic'`,
	).Scan(&retrievalRows); err != nil {
		t.Fatalf("query raw retrieval rows: %v", err)
	}
	if retrievalRows != 0 {
		t.Errorf("raw retrieval rows = %d, want 0", retrievalRows)
	}

	var aggregateRows int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_aggregates WHERE chunk_key IN ('chunk-1', 'chunk-2')`,
	).Scan(&aggregateRows); err != nil {
		t.Fatalf("query raw aggregate rows: %v", err)
	}
	if aggregateRows != 0 {
		t.Errorf("raw aggregate rows = %d, want 0", aggregateRows)
	}
}

func TestSQLiteSignalStoreInsertRetrievalWithCountsPersistsCreatedAtAndEmptyKeys(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	createdAt := time.UnixMilli(1700000000123)

	if err := store.InsertRetrievalWithCounts(ctx, "r-counted", "query", []string{"chunk-1", "chunk-2"}, createdAt); err != nil {
		t.Fatalf("InsertRetrievalWithCounts: %v", err)
	}

	var query, keys string
	var createdAtMillis int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT query, chunk_keys, created_at FROM feedback_retrievals WHERE retrieval_id = 'r-counted'`,
	).Scan(&query, &keys, &createdAtMillis); err != nil {
		t.Fatalf("query raw retrieval row: %v", err)
	}
	if query != "query" || keys != "chunk-1\nchunk-2" || createdAtMillis != 1700000000123 {
		t.Errorf("raw retrieval = (%q, %q, %d), want (%q, %q, %d)", query, keys, createdAtMillis, "query", "chunk-1\nchunk-2", int64(1700000000123))
	}

	for _, key := range []string{"chunk-1", "chunk-2"} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT retrieval_count FROM feedback_aggregates WHERE chunk_key = ?`, key,
		).Scan(&count); err != nil {
			t.Fatalf("query raw aggregate %q: %v", key, err)
		}
		if count != 1 {
			t.Errorf("raw aggregate %q retrieval_count = %d, want 1", key, count)
		}
	}

	if err := store.InsertRetrievalWithCounts(ctx, "r-empty", "empty", nil, createdAt); err != nil {
		t.Fatalf("InsertRetrievalWithCounts empty: %v", err)
	}
	var emptyKeys string
	if err := store.db.QueryRowContext(ctx,
		`SELECT chunk_keys FROM feedback_retrievals WHERE retrieval_id = 'r-empty'`,
	).Scan(&emptyKeys); err != nil {
		t.Fatalf("query empty-key retrieval: %v", err)
	}
	if emptyKeys != "" {
		t.Errorf("empty-key retrieval chunk_keys = %q, want empty", emptyKeys)
	}
	var emptyAggregateRows int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_aggregates WHERE chunk_key = ''`,
	).Scan(&emptyAggregateRows); err != nil {
		t.Fatalf("query empty-key aggregate rows: %v", err)
	}
	if emptyAggregateRows != 0 {
		t.Errorf("empty-key aggregate rows = %d, want 0", emptyAggregateRows)
	}
}

func TestSQLiteSignalStoreInsertSignalsAtomic(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.db.SetMaxOpenConns(1)

	if err := store.InsertRetrieval(ctx, "r-signals", "query", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO feedback_aggregates (chunk_key, last_signal_at)
		VALUES ('chunk-1', 111), ('chunk-2', 111)`); err != nil {
		t.Fatalf("seed aggregate timestamps: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER abort_second_signal
		BEFORE INSERT ON feedback_signals
		WHEN NEW.chunk_key = 'chunk-2'
		BEGIN
			SELECT RAISE(ABORT, 'second signal');
		END`); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}

	err := store.InsertSignals(ctx, "r-signals", []string{"chunk-1", "chunk-2"}, SignalCodeKept, 0.6, time.UnixMilli(222))
	if err == nil {
		t.Fatal("InsertSignals succeeded, want trigger error")
	}

	var signalRows int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM feedback_signals WHERE retrieval_id = 'r-signals'`,
	).Scan(&signalRows); err != nil {
		t.Fatalf("query raw signal rows: %v", err)
	}
	if signalRows != 0 {
		t.Errorf("raw signal rows = %d, want 0", signalRows)
	}

	rows, err := store.db.QueryContext(ctx,
		`SELECT chunk_key, last_signal_at FROM feedback_aggregates WHERE chunk_key IN ('chunk-1', 'chunk-2') ORDER BY chunk_key`)
	if err != nil {
		t.Fatalf("query raw aggregate timestamps: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var aggregateRows int
	for rows.Next() {
		var key string
		var lastSignalAt int64
		if err := rows.Scan(&key, &lastSignalAt); err != nil {
			t.Fatalf("scan raw aggregate timestamp: %v", err)
		}
		if lastSignalAt != 111 {
			t.Errorf("raw aggregate %q last_signal_at = %d, want 111", key, lastSignalAt)
		}
		aggregateRows++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate raw aggregate timestamps: %v", err)
	}
	if aggregateRows != 2 {
		t.Errorf("raw aggregate timestamp rows = %d, want 2", aggregateRows)
	}
}

func TestSQLiteSignalStoreInsertSignalsPersistsCreatedAtAndEmptyKeys(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	createdAt := time.UnixMilli(1700000000456)

	if err := store.InsertRetrieval(ctx, "r-signals", "query", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignals(ctx, "r-signals", []string{"chunk-1", "chunk-2"}, SignalCodeKept, 0.6, createdAt); err != nil {
		t.Fatalf("InsertSignals: %v", err)
	}

	rows, err := store.db.QueryContext(ctx,
		`SELECT chunk_key, signal_kind, strength, created_at FROM feedback_signals WHERE retrieval_id = 'r-signals' ORDER BY chunk_key`)
	if err != nil {
		t.Fatalf("query raw signal rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var gotKeys []string
	for rows.Next() {
		var key, kind string
		var strength float64
		var createdAtMillis int64
		if err := rows.Scan(&key, &kind, &strength, &createdAtMillis); err != nil {
			t.Fatalf("scan raw signal row: %v", err)
		}
		gotKeys = append(gotKeys, key)
		if kind != "code_kept" || strength != 0.6 || createdAtMillis != 1700000000456 {
			t.Errorf("raw signal %q = (%q, %v, %d), want (%q, %v, %d)", key, kind, strength, createdAtMillis, "code_kept", 0.6, int64(1700000000456))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate raw signal rows: %v", err)
	}
	if strings.Join(gotKeys, ",") != "chunk-1,chunk-2" {
		t.Errorf("raw signal keys = %q, want %q", strings.Join(gotKeys, ","), "chunk-1,chunk-2")
	}

	for _, key := range []string{"chunk-1", "chunk-2"} {
		var lastSignalAt int64
		if err := store.db.QueryRowContext(ctx,
			`SELECT last_signal_at FROM feedback_aggregates WHERE chunk_key = ?`, key,
		).Scan(&lastSignalAt); err != nil {
			t.Fatalf("query raw aggregate %q: %v", key, err)
		}
		if lastSignalAt != 1700000000456 {
			t.Errorf("raw aggregate %q last_signal_at = %d, want %d", key, lastSignalAt, int64(1700000000456))
		}
	}

	if err := store.InsertSignals(ctx, "missing", nil, SignalCodeUndone, -0.7, createdAt); err != nil {
		t.Fatalf("InsertSignals empty: %v", err)
	}
	var signalRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_signals`).Scan(&signalRows); err != nil {
		t.Fatalf("count raw signals: %v", err)
	}
	if signalRows != 2 {
		t.Errorf("raw signal rows after empty insert = %d, want 2", signalRows)
	}
}

func TestSignalCountEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SignalCount = %d, want 0", count)
	}
}

func TestGetAggregate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Non-existent chunk returns zero aggregate.
	agg, err := store.GetAggregate(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	if agg.WeightedScore != 0 || agg.RetrievalCount != 0 {
		t.Errorf("expected zero aggregate, got %+v", agg)
	}

	// Insert a signal to create an aggregate row.
	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	agg, err = store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	// Signal creates an aggregate with last_signal_at set, but weighted_score
	// is only updated by RecomputeAggregates. retrieval_count is 0 until
	// IncrementRetrievalCount is called.
	if agg.RetrievalCount != 0 {
		t.Errorf("retrieval_count = %d, want 0", agg.RetrievalCount)
	}
}

func TestGetAggregatesBatch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Empty batch returns empty map.
	result, err := store.GetAggregatesBatch(ctx, nil)
	if err != nil {
		t.Fatalf("GetAggregatesBatch empty: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}

	// Set up some data.
	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-2", SignalCodeUndone, -0.7, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	result, err = store.GetAggregatesBatch(ctx, []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("GetAggregatesBatch: %v", err)
	}

	// chunk-1 and chunk-2 exist, chunk-3 does not.
	if len(result) != 2 {
		t.Errorf("expected 2 results, got %d", len(result))
	}
	if _, ok := result["chunk-1"]; !ok {
		t.Error("chunk-1 not in results")
	}
	if _, ok := result["chunk-2"]; !ok {
		t.Error("chunk-2 not in results")
	}
}

func TestIncrementRetrievalCount(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	keys := []string{"chunk-1", "chunk-2"}
	if err := store.IncrementRetrievalCount(ctx, keys); err != nil {
		t.Fatalf("IncrementRetrievalCount: %v", err)
	}
	if err := store.IncrementRetrievalCount(ctx, keys); err != nil {
		t.Fatalf("IncrementRetrievalCount (2nd): %v", err)
	}

	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	if agg.RetrievalCount != 2 {
		t.Errorf("retrieval_count = %d, want 2", agg.RetrievalCount)
	}

	// Empty slice is a no-op.
	if err := store.IncrementRetrievalCount(ctx, nil); err != nil {
		t.Fatalf("IncrementRetrievalCount nil: %v", err)
	}
}

func TestRecomputeAggregates(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6, time.Time{}); err != nil {
		t.Fatalf("InsertSignal (2nd): %v", err)
	}

	if err := store.RecomputeAggregates(ctx, 0.1); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	// Both signals are very recent so decay is negligible.
	// Score should be approximately 0.8 + 0.6 = 1.4.
	if agg.WeightedScore < 1.3 || agg.WeightedScore > 1.5 {
		t.Errorf("weighted_score = %f, expected ~1.4", agg.WeightedScore)
	}
}

func TestRecomputeAggregatesEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Recompute with no signals should be a no-op.
	if err := store.RecomputeAggregates(ctx, 0.1); err != nil {
		t.Fatalf("RecomputeAggregates (empty): %v", err)
	}
}

func TestRecomputeAggregatesZerosStaleScores(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Insert a signal and recompute to set a non-zero weighted_score.
	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if err := store.RecomputeAggregates(ctx, 0.0); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate before prune: %v", err)
	}
	if agg.WeightedScore < 0.7 {
		t.Fatalf("expected non-zero score before prune, got %f", agg.WeightedScore)
	}

	// Prune ALL signals for this chunk.
	cutoff := time.Now().Add(time.Hour)
	if _, err := store.PruneSignals(ctx, cutoff); err != nil {
		t.Fatalf("PruneSignals: %v", err)
	}

	// Recompute -- chunk-1 has no signals left, score should be zeroed.
	if err := store.RecomputeAggregates(ctx, 0.0); err != nil {
		t.Fatalf("RecomputeAggregates after prune: %v", err)
	}

	agg, err = store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate after prune+recompute: %v", err)
	}
	if agg.WeightedScore != 0 {
		t.Errorf("weighted_score after prune+recompute = %f, want 0", agg.WeightedScore)
	}
}

func TestPruneSignals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	// Prune with a future cutoff should delete the signal.
	cutoff := time.Now().Add(time.Hour)
	n, err := store.PruneSignals(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneSignals: %v", err)
	}
	if n != 1 {
		t.Errorf("PruneSignals deleted %d, want 1", n)
	}

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SignalCount = %d, want 0 after prune", count)
	}
}

func TestPruneSignalsNone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	// Prune with a past cutoff should delete nothing.
	cutoff := time.Now().Add(-time.Hour)
	n, err := store.PruneSignals(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneSignals: %v", err)
	}
	if n != 0 {
		t.Errorf("PruneSignals deleted %d, want 0", n)
	}
}

func TestPruneRetrievals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Insert two retrievals, only one with signals.
	if err := store.InsertRetrieval(ctx, "r1", "q1", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval r1: %v", err)
	}
	if err := store.InsertRetrieval(ctx, "r2", "q2", []string{"chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval r2: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	n, err := store.PruneRetrievals(ctx)
	if err != nil {
		t.Fatalf("PruneRetrievals: %v", err)
	}
	if n != 1 {
		t.Errorf("PruneRetrievals deleted %d, want 1", n)
	}
}
