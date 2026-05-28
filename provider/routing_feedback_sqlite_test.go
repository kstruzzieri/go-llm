package provider

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newSQLiteFeedbackStoreForTest opens an isolated in-memory DB pinned
// to one connection so the pool doesn't observe an empty DB on a second
// connection, then constructs the store via the caller-owned-*sql.DB path.
func newSQLiteFeedbackStoreForTest(t *testing.T, cfg SQLiteFeedbackStoreConfig) *SQLiteFeedbackStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteFeedbackStore(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("NewSQLiteFeedbackStore: %v", err)
	}
	return store
}

// runRoutingFeedbackStoreContract is the parity test runner: PR3 keeps
// MemoryStore and SQLiteFeedbackStore behavior in lockstep by running
// every contract case against both implementations.
func runRoutingFeedbackStoreContract(t *testing.T, name string, factory func(t *testing.T) RoutingFeedbackStore) {
	t.Helper()
	t.Run(name+"/GetEmptyIsNeutral", func(t *testing.T) {
		store := factory(t)
		got, err := store.Get(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SampleCount != 0 || got.ScoredCount != 0 {
			t.Errorf("empty store returned %+v, want zero counts", got)
		}
		if got.Score != DefaultNeutralScore {
			t.Errorf("empty store score = %v, want %v", got.Score, DefaultNeutralScore)
		}
	})
	t.Run(name+"/RecordRejectsInvalidKey", func(t *testing.T) {
		store := factory(t)
		err := store.Record(context.Background(), FeedbackKey{Model: "m"}, FeedbackSignal{Kind: RoutingSignalSuccess})
		if !errors.Is(err, ErrInvalidFeedbackKey) {
			t.Errorf("Record err = %v, want ErrInvalidFeedbackKey", err)
		}
	})
	t.Run(name+"/RecordSuccessIncrementsScore", func(t *testing.T) {
		store := factory(t)
		k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
		if err := store.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		agg, err := store.Get(context.Background(), k)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if agg.SampleCount != 1 || agg.ScoredCount != 1 {
			t.Errorf("counts = (%d,%d), want (1,1)", agg.SampleCount, agg.ScoredCount)
		}
		if agg.Score <= DefaultNeutralScore {
			t.Errorf("Score = %v, want > %v", agg.Score, DefaultNeutralScore)
		}
	})
	t.Run(name+"/RecordBatchAtomicValidationFailureRollsBack", func(t *testing.T) {
		store := factory(t)
		k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
		items := []FeedbackItem{
			{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}},
			{Key: FeedbackKey{}, Signal: FeedbackSignal{Kind: RoutingSignalSuccess}}, // invalid
		}
		err := store.RecordBatch(context.Background(), items)
		if !errors.Is(err, ErrInvalidFeedbackKey) {
			t.Fatalf("RecordBatch err = %v, want ErrInvalidFeedbackKey", err)
		}
		agg, _ := store.Get(context.Background(), k)
		if agg.SampleCount != 0 {
			t.Errorf("partial application: SampleCount = %d, want 0 (full rollback)", agg.SampleCount)
		}
	})
	t.Run(name+"/RetentionFIFOSameTimestamp", func(t *testing.T) {
		store := factory(t)
		k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}
		now := time.Now()
		items := make([]FeedbackItem, 5)
		for i := range items {
			items[i] = FeedbackItem{Key: k, Signal: FeedbackSignal{Kind: RoutingSignalSuccess, At: now}}
		}
		if err := store.RecordBatch(context.Background(), items); err != nil {
			t.Fatalf("RecordBatch: %v", err)
		}
		agg, err := store.Get(context.Background(), k)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if agg.SampleCount != 3 {
			t.Errorf("retention SampleCount = %d, want 3 (cap honored even on same timestamp)", agg.SampleCount)
		}
	})
}

func TestMemoryStoreContract(t *testing.T) {
	runRoutingFeedbackStoreContract(t, "MemoryStore", func(t *testing.T) RoutingFeedbackStore {
		store, err := NewMemoryStore(MemoryStoreConfig{MaxRetainedSamples: 3})
		if err != nil {
			t.Fatalf("NewMemoryStore: %v", err)
		}
		return store
	})
}

func TestSQLiteFeedbackStoreContract(t *testing.T) {
	runRoutingFeedbackStoreContract(t, "SQLiteFeedbackStore", func(t *testing.T) RoutingFeedbackStore {
		return newSQLiteFeedbackStoreForTest(t, SQLiteFeedbackStoreConfig{MaxRetainedSamples: 3})
	})
}

func TestSQLiteFeedbackStoreConcurrentRecord(t *testing.T) {
	store := newSQLiteFeedbackStoreForTest(t, SQLiteFeedbackStoreConfig{MaxRetainedSamples: 0 /* default 1000 */})
	k := FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}

	const goroutines = 16
	const perGoroutine = 32
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := store.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	agg, err := store.Get(context.Background(), k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if agg.SampleCount != goroutines*perGoroutine {
		t.Errorf("SampleCount = %d, want %d (concurrent Record lost writes)", agg.SampleCount, goroutines*perGoroutine)
	}
}

func TestNewSQLiteFeedbackStoreNilDB(t *testing.T) {
	_, err := NewSQLiteFeedbackStore(context.Background(), nil, SQLiteFeedbackStoreConfig{})
	if err == nil {
		t.Errorf("NewSQLiteFeedbackStore(nil) should error, got nil")
	}
}

func TestOpenSQLiteFeedbackStoreEmptyPath(t *testing.T) {
	_, err := OpenSQLiteFeedbackStore(context.Background(), "", SQLiteFeedbackStoreConfig{})
	if err == nil {
		t.Errorf("OpenSQLiteFeedbackStore(\"\") should error, got nil")
	}
}

func TestOpenSQLiteFeedbackStoreInMemory(t *testing.T) {
	store, err := OpenSQLiteFeedbackStore(context.Background(), ":memory:", SQLiteFeedbackStoreConfig{})
	if err != nil {
		t.Fatalf("OpenSQLiteFeedbackStore(:memory:): %v", err)
	}
	defer store.Close()
	if err := store.Record(context.Background(), FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"},
		FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
		t.Errorf("Record after Open: %v", err)
	}
}
