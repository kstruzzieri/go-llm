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
	"slices"
	"strings"
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
	// A commit after construction is visible to the same reader: no read
	// snapshot outlives a WeightsBatch call.
	if _, err := db.ExecContext(ctx, `UPDATE feedback_aggregates SET weighted_score = 0.3 WHERE chunk_key = 'chunk-live'`); err != nil {
		t.Fatalf("update aggregate after open: %v", err)
	}
	weights, err = reader.WeightsBatch(ctx, []string{"chunk-live"})
	if err != nil {
		t.Fatalf("WeightsBatch after a post-open commit: %v", err)
	}
	if got := weights["chunk-live"]; got != 0.3 {
		t.Fatalf("post-open WAL weight = %v, want 0.3", got)
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
		// D2 (#591): SQLite may add its -shm coordination file; nothing else.
		names := dirEntryNames(t, dir)
		if !slices.Equal(names, []string{"unmigrated.db", "unmigrated.db-wal"}) &&
			!slices.Equal(names, []string{"unmigrated.db", "unmigrated.db-shm", "unmigrated.db-wal"}) {
			t.Fatalf("fixture directory after construction = %v, want main and WAL plus at most -shm", names)
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

// TestSQLiteWeightReaderValidatesSchemaCommittedOnlyToWAL: the migrations
// sit in an open writer's WAL and were never checkpointed, so validation must
// read through the WAL rather than the main file alone.
func TestSQLiteWeightReaderValidatesSchemaCommittedOnlyToWAL(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := NewSignalStore(ctx, db); err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	// Fixture check: the main file alone holds no schema version.
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	mainOnly, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatalf("open immutable fixture check: %v", err)
	}
	var versionTables int
	err = mainOnly.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE name = 'feedback_schema_version'`).Scan(&versionTables)
	_ = mainOnly.Close()
	if err != nil {
		t.Fatalf("query immutable fixture check: %v", err)
	}
	if versionTables != 0 {
		t.Fatal("fixture invalid: migrations already reached the main file")
	}
	reader, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{})
	if err != nil {
		t.Fatalf("NewSQLiteWeightReader with the schema only in the WAL: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

// TestSQLiteWeightReaderRejectionKeepsMainAndWAL pins the D2 contract
// (#591): rejecting a database never writes or deletes its main file or an
// existing WAL; only SQLite's coordination sidecars may appear.
func TestSQLiteWeightReaderRejectionKeepsMainAndWAL(t *testing.T) {
	ctx := t.Context()
	t.Run("short main beside WAL", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "src.db")
		w, err := sql.Open("sqlite", src)
		if err != nil {
			t.Fatal(err)
		}
		w.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = w.Close() })
		for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, `CREATE TABLE existing (value TEXT)`, `INSERT INTO existing (value) VALUES ('wal-only')`} {
			if _, err := w.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		walBefore, err := os.ReadFile(src + "-wal")
		if err != nil || len(walBefore) == 0 {
			t.Fatalf("fixture invalid: source WAL = %d bytes, %v", len(walBefore), err)
		}
		// modernc's VFS sizes a 1-byte file as 0 bytes (SQLite ticket #3260);
		// every file below one 512-byte page must be rejected before any open.
		for _, tc := range []struct {
			name string
			main []byte
		}{
			{"0 bytes", nil},
			{"1 byte", []byte("S")},
			{"100 bytes", bytes.Repeat([]byte{0xAB}, 100)},
			{"511 bytes", bytes.Repeat([]byte{0xAB}, 511)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "short.db")
				if err := os.WriteFile(path, tc.main, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+"-wal", walBefore, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{}); err == nil {
					t.Fatal("NewSQLiteWeightReader accepted a database file shorter than one page")
				}
				mainAfter, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read short main after rejection: %v", err)
				}
				if !bytes.Equal(mainAfter, tc.main) {
					t.Fatalf("short main after rejection is %d bytes, want %d unchanged", len(mainAfter), len(tc.main))
				}
				walAfter, err := os.ReadFile(path + "-wal")
				if err != nil {
					t.Fatalf("WAL beside the short main is gone: %v", err)
				}
				if !bytes.Equal(walAfter, walBefore) {
					t.Fatal("WAL beside the short main changed")
				}
				// No -shm: the rejection came before any SQLite open.
				if names := dirEntryNames(t, dir); !slices.Equal(names, []string{"short.db", "short.db-wal"}) {
					t.Fatalf("directory after rejection = %v, want only short.db and short.db-wal", names)
				}
			})
		}
	})

	t.Run("WAL-mode main without WAL", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "closed.db")
		w, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		for _, q := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE existing (value TEXT)`, `INSERT INTO existing (value) VALUES ('main')`} {
			if _, err := w.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		// The last close checkpoints and removes -wal and -shm.
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if names := dirEntryNames(t, dir); !slices.Equal(names, []string{"closed.db"}) {
			t.Fatalf("fixture invalid: directory = %v, want only closed.db", names)
		}
		mainBefore, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewSQLiteWeightReader(ctx, path, CollectorConfig{}); err == nil {
			t.Fatal("NewSQLiteWeightReader accepted an unmigrated database")
		}
		mainAfter, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(mainAfter, mainBefore) {
			t.Fatal("main database changed during rejection")
		}
		allowed := map[string]bool{"closed.db": true, "closed.db-shm": true, "closed.db-wal": true}
		for _, name := range dirEntryNames(t, dir) {
			if !allowed[name] {
				t.Fatalf("rejection created %q", name)
			}
		}
		switch info, err := os.Stat(path + "-wal"); {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			t.Fatalf("stat WAL after rejection: %v", err)
		case info.Size() != 0:
			t.Fatalf("rejection wrote %d WAL bytes", info.Size())
		}
	})
}

// TestSQLiteWeightReaderClosesHandleOnRejection: a rejected database must not
// keep a reader connection open. Leaving WAL mode needs exclusive access, so a
// leaked connection makes the writer's journal_mode change fail as locked.
func TestSQLiteWeightReaderClosesHandleOnRejection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   string
		wantErr string
	}{
		// currentSchemaVersion returns 0: the version-mismatch branch.
		{"unmigrated", `CREATE TABLE existing (value TEXT)`, "version 0, want 1"},
		// The version table lacks its version column, so
		// currentSchemaVersion fails: the schema-query error branch.
		{"unreadable version", `CREATE TABLE feedback_schema_version (other INTEGER)`, "query schema version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "rejected.db")
			u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(0)"}
			w, err := sql.Open("sqlite", u.String())
			if err != nil {
				t.Fatal(err)
			}
			w.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = w.Close() })
			for _, q := range []string{`PRAGMA journal_mode=WAL`, tc.setup} {
				if _, err := w.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			_, err = NewSQLiteWeightReader(ctx, path, CollectorConfig{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NewSQLiteWeightReader error = %v, want rejection containing %q", err, tc.wantErr)
			}
			var mode string
			if err := w.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
				t.Fatalf("leave WAL after rejection: %v", err)
			}
			if mode != "delete" {
				t.Fatalf("journal_mode after rejection = %q, want delete", mode)
			}
		})
	}
}

// TestSQLiteWeightReaderResolvesRelativePath: a relative path must open the
// file in the working directory rather than render as a file://name URI,
// which SQLite rejects as an authority.
func TestSQLiteWeightReaderResolvesRelativePath(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	w, err := sql.Open("sqlite", filepath.Join(dir, "rel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSignalStore(ctx, w); err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, name := range []string{"rel.db", "./rel.db"} {
		t.Run(name, func(t *testing.T) {
			reader, err := NewSQLiteWeightReader(ctx, name, CollectorConfig{})
			if err != nil {
				t.Fatalf("NewSQLiteWeightReader(%q): %v", name, err)
			}
			if _, err := reader.WeightsBatch(ctx, []string{"chunk"}); err != nil {
				t.Fatalf("WeightsBatch via %q: %v", name, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatalf("Close via %q: %v", name, err)
			}
		})
	}
}

func TestSQLiteWeightReaderRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	_, err := NewSQLiteWeightReader(t.Context(), dir, CollectorConfig{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("NewSQLiteWeightReader(directory) error = %v, want not a regular file", err)
	}
}
