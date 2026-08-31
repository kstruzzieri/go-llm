package feedback

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errTestBatchTooLarge = errors.New("test batch too large")

type batchingStore struct {
	SignalStore
	signalCount int
	maxBatch    int
	calls       [][]string
	aggs        map[string]Aggregate
}

func (s *batchingStore) SignalCount(ctx context.Context) (int, error) {
	return s.signalCount, nil
}

func (s *batchingStore) GetAggregatesBatch(ctx context.Context, chunkKeys []string) (map[string]Aggregate, error) {
	if len(chunkKeys) > s.maxBatch {
		return nil, errTestBatchTooLarge
	}
	s.calls = append(s.calls, append([]string(nil), chunkKeys...))
	out := make(map[string]Aggregate, len(chunkKeys))
	for _, k := range chunkKeys {
		if agg, ok := s.aggs[k]; ok {
			out[k] = agg
		}
	}
	return out, nil
}

// warmedStore returns a store where chunk-1 has 2 positive signals (clears a
// WarmupSignals=2 gate) and retrieval_count=1, with aggregates recomputed.
func warmedStore(t *testing.T) *SQLiteSignalStore {
	t.Helper()
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.IncrementRetrievalCount(ctx, []string{"chunk-1"}); err != nil {
		t.Fatalf("IncrementRetrievalCount: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal 1: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6, time.Time{}); err != nil {
		t.Fatalf("InsertSignal 2: %v", err)
	}
	if err := store.RecomputeAggregates(ctx, 0); err != nil { // lambda 0 => no decay
		t.Fatalf("RecomputeAggregates: %v", err)
	}
	return store
}

func TestWeightReaderWarmed(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t)
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 2, MinRetrievals: 1})

	w, err := r.WeightsBatch(ctx, []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if got := w["chunk-1"]; got < 1.39 || got > 1.41 {
		t.Errorf("chunk-1 weight = %v, want ~1.4", got)
	}
	if w["chunk-2"] != 0 {
		t.Errorf("chunk-2 weight = %v, want 0 (unknown key)", w["chunk-2"])
	}
}

func TestWeightReaderColdStart(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t) // 2 signals total
	// WarmupSignals=5 not met by 2 signals => all zero.
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 5, MinRetrievals: 1})
	w, err := r.WeightsBatch(ctx, []string{"chunk-1"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if w["chunk-1"] != 0 {
		t.Errorf("cold-start weight = %v, want 0", w["chunk-1"])
	}
}

func TestWeightReaderMinRetrievalsGate(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t) // retrieval_count for chunk-1 == 1
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 2, MinRetrievals: 5})
	w, err := r.WeightsBatch(ctx, []string{"chunk-1"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if w["chunk-1"] != 0 {
		t.Errorf("below-MinRetrievals weight = %v, want 0", w["chunk-1"])
	}
}

func TestWeightReaderBatchesAggregateLookups(t *testing.T) {
	const keyCount = 1001
	keys := make([]string, keyCount)
	aggs := make(map[string]Aggregate, keyCount)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%04d", i)
		aggs[keys[i]] = Aggregate{WeightedScore: float64(i), RetrievalCount: 1}
	}
	store := &batchingStore{signalCount: 1, maxBatch: 900, aggs: aggs}
	reader := NewWeightReader(store, CollectorConfig{WarmupSignals: 1, MinRetrievals: 1})

	got, err := reader.WeightsBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("GetAggregatesBatch calls = %d, want 2", len(store.calls))
	}
	for i, call := range store.calls {
		if len(call) > store.maxBatch {
			t.Fatalf("call %d len = %d, want <= %d", i, len(call), store.maxBatch)
		}
	}
	if got["chunk-1000"] != 1000 {
		t.Fatalf("chunk-1000 weight = %v, want 1000", got["chunk-1000"])
	}
}

func TestWeightReaderParityWithCollector(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t)
	cfg := CollectorConfig{WarmupSignals: 2, MinRetrievals: 1}

	reader := NewWeightReader(store, cfg)
	collector := NewCollector(store, cfg)
	defer collector.Close()

	keys := []string{"chunk-1", "chunk-2"}
	rw, err := reader.WeightsBatch(ctx, keys)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	cw, err := collector.WeightsBatch(ctx, keys)
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	for _, k := range keys {
		if rw[k] != cw[k] {
			t.Errorf("parity mismatch for %q: reader=%v collector=%v", k, rw[k], cw[k])
		}
	}
}

func TestSQLiteWeightReaderSeesLiveWALWithoutWriting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	store, err := NewSignalStore(ctx, db)
	if err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatalf("disable auto-checkpoint: %v", err)
	}
	if err := store.InsertRetrievalWithCounts(ctx, "r-live", "query", []string{"chunk-live"}, time.Now()); err != nil {
		t.Fatalf("insert retrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r-live", "chunk-live", SignalCompletionAccepted, 0.8, time.Now()); err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	if err := store.RecomputeAggregates(ctx, 0); err != nil {
		t.Fatalf("recompute aggregates: %v", err)
	}

	reader, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{})
	if err != nil {
		t.Fatalf("NewSQLiteWeightReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	weights, err := reader.WeightsBatch(ctx, []string{"chunk-live"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if got := weights["chunk-live"]; got != 0.8 {
		t.Fatalf("live WAL weight = %v, want 0.8", got)
	}
	if _, err := reader.db.ExecContext(ctx, `INSERT INTO feedback_aggregates (chunk_key) VALUES ('forbidden')`); err == nil {
		t.Fatal("write through reader succeeded")
	}

	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()
	immutableDB, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatalf("open immutable reader: %v", err)
	}
	defer func() { _ = immutableDB.Close() }()
	immutable := NewWeightReader(&SQLiteSignalStore{db: immutableDB}, CollectorConfig{})
	weights, err = immutable.WeightsBatch(ctx, []string{"chunk-live"})
	if err != nil {
		t.Fatalf("immutable WeightsBatch: %v", err)
	}
	if got := weights["chunk-live"]; got != 0 {
		t.Fatalf("immutable WAL weight = %v, want 0", got)
	}
}

func TestSQLiteWeightReaderRejectsMissingAndUnmigratedDatabasesWithoutMutation(t *testing.T) {
	ctx := context.Background()
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.db")
		if _, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{}); err == nil {
			t.Fatal("NewSQLiteWeightReader succeeded for missing database")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing database Stat error = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("unmigrated", func(t *testing.T) {
		sourcePath := filepath.Join(t.TempDir(), "source.db")
		db, err := sql.Open("sqlite", sourcePath)
		if err != nil {
			t.Fatalf("open fixture: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatalf("enable fixture WAL: %v", err)
		}
		if _, err := db.Exec(`CREATE TABLE existing (value TEXT)`); err != nil {
			t.Fatalf("create fixture schema: %v", err)
		}
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatalf("checkpoint fixture schema: %v", err)
		}
		if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
			t.Fatalf("disable fixture auto-checkpoint: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO existing (value) VALUES ('wal-only')`); err != nil {
			t.Fatalf("commit fixture WAL row: %v", err)
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "unmigrated.db")
		mainBefore, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read fixture main: %v", err)
		}
		walBefore, err := os.ReadFile(sourcePath + "-wal")
		if err != nil {
			t.Fatalf("read fixture WAL: %v", err)
		}
		if err := os.WriteFile(path, mainBefore, 0o600); err != nil {
			t.Fatalf("copy fixture main: %v", err)
		}
		if err := os.WriteFile(path+"-wal", walBefore, 0o600); err != nil {
			t.Fatalf("copy fixture WAL: %v", err)
		}

		if _, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{}); err == nil {
			t.Fatal("NewSQLiteWeightReader succeeded for unmigrated database")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read fixture directory: %v", err)
		}
		if len(entries) != 2 || entries[0].Name() != "unmigrated.db" || entries[1].Name() != "unmigrated.db-wal" {
			t.Fatalf("fixture directory after construction = %v, want only main and WAL", entries)
		}
		mainAfter, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture main after: %v", err)
		}
		walAfter, err := os.ReadFile(path + "-wal")
		if err != nil {
			t.Fatalf("read fixture WAL after: %v", err)
		}
		if !bytes.Equal(mainAfter, mainBefore) || !bytes.Equal(walAfter, walBefore) {
			t.Fatal("unmigrated main database or WAL changed during construction")
		}
		check, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open fixture check: %v", err)
		}
		defer func() { _ = check.Close() }()
		var tables int
		if err := check.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			t.Fatalf("query fixture schema: %v", err)
		}
		if tables != 1 {
			t.Fatalf("table count = %d, want 1", tables)
		}
	})
}

func TestSQLiteWeightReaderOpensMigratedReadOnlyFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := NewSignalStore(ctx, db); err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}

	reader, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{})
	if err != nil {
		t.Fatalf("NewSQLiteWeightReader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSQLiteWeightReaderZeroAndClosed(t *testing.T) {
	ctx := context.Background()
	var zero SQLiteWeightReader
	if _, err := zero.WeightsBatch(ctx, []string{"chunk"}); err == nil {
		t.Fatal("zero reader WeightsBatch succeeded")
	}
	if err := zero.Close(); err != nil {
		t.Fatalf("zero Close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := NewSignalStore(ctx, db); err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	reader, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{})
	if err != nil {
		t.Fatalf("NewSQLiteWeightReader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := reader.WeightsBatch(ctx, []string{"chunk"}); err == nil {
		t.Fatal("closed reader WeightsBatch succeeded")
	}
}
