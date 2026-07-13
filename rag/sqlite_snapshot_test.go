package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newReadOnlyTestStore(t *testing.T, chunks []Chunk, embeddings [][]float64, vectorSpaceID string) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	w, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	seedTestStoreWithVectorSpaceID(t, w, chunks, embeddings, vectorSpaceID)
	if _, err := w.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint seed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ro, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

func seedTestStoreWithVectorSpaceID(t *testing.T, store *SQLiteStore, chunks []Chunk, embeddings [][]float64, vectorSpaceID string) {
	t.Helper()
	tx, err := store.beginWriteTx(context.Background())
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	if err := store.insertChunksTx(context.Background(), tx, chunks, embeddings, "test-hash", vectorSpaceID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed chunks: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

func TestSQLiteSnapshotLoadsOnceAcrossProbeAndSearch(t *testing.T) {
	chunks := []Chunk{
		{ID: "a", Content: "alpha", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{"kind": "function"}},
		{ID: "b", Content: "beta", Source: "b.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{"kind": "type"}},
	}
	ro := newReadOnlyTestStore(t, chunks, [][]float64{{1, 0}, {0, 1}}, "test/v1")

	var loads atomic.Int32
	ro.recordStage = func(stage string, _ time.Duration) {
		if stage == "snapshot_load_decode" {
			loads.Add(1)
		}
	}

	ctx := context.Background()
	for range 2 {
		if _, err := ro.ProbeVectorSpaces(ctx); err != nil {
			t.Fatalf("ProbeVectorSpaces: %v", err)
		}
		if _, err := ro.Search(ctx, []float64{1, 0}, 1); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("snapshot loads = %d, want 1", got)
	}
}

// residentSnapshot reads the published snapshot under the store lock so tests
// cannot race the decoupled load goroutine.
func residentSnapshot(s *SQLiteStore) *sqliteSnapshot {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	return s.resident
}

type cancelAfterChecksContext struct {
	mu       sync.Mutex
	done     chan struct{}
	checks   int
	after    int
	canceled bool
}

func (c *cancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecksContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterChecksContext) Value(any) any               { return nil }
func (c *cancelAfterChecksContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canceled {
		return context.Canceled
	}
	c.checks++
	if c.checks > c.after {
		close(c.done)
		c.canceled = true
		return context.Canceled
	}
	return nil
}

func newCancelAfterChecksContext(after int) *cancelAfterChecksContext {
	return &cancelAfterChecksContext{after: after, done: make(chan struct{})}
}

func TestSQLiteSnapshotLoadSurvivesInitiatorCancel(t *testing.T) {
	chunks := []Chunk{
		{ID: "a", Content: "alpha", Source: "a.go", Metadata: map[string]string{}},
		{ID: "b", Content: "beta", Source: "b.go", Metadata: map[string]string{}},
		{ID: "c", Content: "gamma", Source: "c.go", Metadata: map[string]string{}},
		{ID: "d", Content: "delta", Source: "d.go", Metadata: map[string]string{}},
	}
	ro := newReadOnlyTestStore(t, chunks, [][]float64{{1, 0}, {0, 1}, {1, 1}, {-1, 0}}, "test/v1")

	// Block the load goroutine just before it publishes (the load-decode
	// stage hook fires at the end of the decode) so the initiator's
	// cancellation deterministically races an in-flight shared load.
	var loads atomic.Int32
	gate := make(chan struct{})
	release := make(chan struct{})
	var gateOnce sync.Once
	ro.recordStage = func(stage string, _ time.Duration) {
		if stage == "snapshot_load_decode" {
			loads.Add(1)
			gateOnce.Do(func() {
				close(gate)
				<-release
			})
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	initiatorErr := make(chan error, 1)
	go func() {
		_, err := ro.ProbeVectorSpaces(canceled)
		initiatorErr <- err
	}()

	<-gate
	if err := <-initiatorErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("initiator error = %v, want context.Canceled", err)
	}
	if snap := residentSnapshot(ro); snap != nil {
		t.Fatal("snapshot published before the load completed")
	}

	waiterErr := make(chan error, 1)
	go func() {
		_, err := ro.Search(context.Background(), []float64{1, 0}, 2)
		waiterErr <- err
	}()
	close(release)
	if err := <-waiterErr; err != nil {
		t.Fatalf("concurrent caller with live context failed after initiator cancel: %v", err)
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("snapshot loads = %d, want 1 (canceled initiator must not abort or rerun the shared load)", got)
	}
	if snap := residentSnapshot(ro); snap == nil || len(snap.chunks) != len(chunks) {
		t.Fatalf("decoupled load did not publish the complete snapshot: %+v", snap)
	}
}

func TestSQLiteSnapshotDenseSearchHydratesDeterministicFinalists(t *testing.T) {
	chunks := []Chunk{
		{ID: "first", Content: "first body", Source: "same.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "stable-first", Metadata: map[string]string{"kind": "function"}},
		{ID: "second", Content: "second body", Source: "same.go", StartLine: 3, EndLine: 4, Language: "go", StableKey: "stable-second", Metadata: map[string]string{"kind": "type"}},
		{ID: "third", Content: "third body", Source: "other.go", StartLine: 5, EndLine: 6, Language: "go", StableKey: "stable-third", Metadata: map[string]string{"kind": "variable"}},
	}
	ro := newReadOnlyTestStore(t, chunks, [][]float64{{1, 0}, {2, 0}, {0, 1}}, "test/v1")
	stages := make(map[string]int)
	ro.recordStage = func(stage string, _ time.Duration) { stages[stage]++ }

	results, err := ro.Search(context.Background(), []float64{1, 0}, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for i, want := range chunks[:2] {
		want.Metadata = map[string]string{"kind": want.Metadata["kind"], "vector_space_id": "test/v1"}
		if !reflect.DeepEqual(results[i].Chunk, want) {
			t.Errorf("result %d chunk = %+v, want %+v", i, results[i].Chunk, want)
		}
		if results[i].Score != 1 || results[i].Distance != 0 {
			t.Errorf("result %d score/distance = %v/%v, want 1/0", i, results[i].Score, results[i].Distance)
		}
	}
	if stages["snapshot_load_decode"] != 1 {
		t.Errorf("snapshot loads = %d, want 1", stages["snapshot_load_decode"])
	}
	if stages["finalist_hydration"] != 1 {
		t.Errorf("finalist hydration stages = %d, want 1", stages["finalist_hydration"])
	}
}

func TestSQLiteSnapshotHybridMatchesWritableRankingAndAttribution(t *testing.T) {
	chunks := []Chunk{
		{ID: "a", Content: "alpha retrieval token", Source: "pkg/a.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "stable-a", Metadata: map[string]string{"kind": "function"}},
		{ID: "b", Content: "alpha helper", Source: "pkg/b.go", StartLine: 3, EndLine: 4, Language: "go", StableKey: "stable-b", Metadata: map[string]string{"kind": "type"}},
		{ID: "c", Content: "unrelated", Source: "other/c.go", StartLine: 5, EndLine: 6, Language: "go", StableKey: "stable-c", Metadata: map[string]string{"kind": "variable"}},
	}
	embeddings := [][]float64{{1, 0, 0}, {0.8, 0.6, 0}, {0, 1, 0}}
	writable := newTestStore(t)
	seedTestStoreWithVectorSpaceID(t, writable, chunks, embeddings, "test/v1")
	ro := newReadOnlyTestStore(t, chunks, embeddings, "test/v1")
	weights := &fakeWeighter{weights: map[string]float64{"stable-b": 0.5, "stable-c": 1}}
	writable.SetBehavioralWeighter(weights)
	ro.SetBehavioralWeighter(weights)
	stages := make(map[string]int)
	ro.recordStage = func(stage string, _ time.Duration) { stages[stage]++ }
	query := []float64{0.9, 0.1, 0}
	qCtx := QueryContext{CurrentFile: "pkg/a.go", Timestamp: time.Unix(2000, 0)}
	want, err := writable.SearchMulti(context.Background(), query, "alpha retrieval", 3, qCtx)
	if err != nil {
		t.Fatalf("writable SearchMulti: %v", err)
	}
	got, err := ro.SearchMulti(context.Background(), query, "alpha retrieval", 3, qCtx)
	if err != nil {
		t.Fatalf("read-only SearchMulti: %v", err)
	}
	again, err := ro.SearchMulti(context.Background(), query, "alpha retrieval", 3, qCtx)
	if err != nil {
		t.Fatalf("repeated read-only SearchMulti: %v", err)
	}
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("repeated hybrid result changed: first=%+v second=%+v", got, again)
	}
	if len(got) != len(want) {
		t.Fatalf("result count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i].Chunk, want[i].Chunk) {
			t.Errorf("result %d chunk = %+v, want %+v", i, got[i].Chunk, want[i].Chunk)
		}
		if got[i].Chunk.ID != want[i].Chunk.ID {
			t.Errorf("result %d ID = %q, want %q", i, got[i].Chunk.ID, want[i].Chunk.ID)
		}
		if math.Abs(got[i].Score-want[i].Score) > 1e-12 || math.Abs(got[i].Distance-want[i].Distance) > 1e-12 {
			t.Errorf("result %d score/distance = %v/%v, want %v/%v", i, got[i].Score, got[i].Distance, want[i].Score, want[i].Distance)
		}
		if math.Abs(got[i].RankScore-want[i].RankScore) > 1e-12 {
			t.Errorf("result %d RankScore = %v, want %v", i, got[i].RankScore, want[i].RankScore)
		}
		for signal, wantScore := range want[i].Signals {
			if math.Abs(got[i].Signals[signal]-wantScore) > 1e-12 {
				t.Errorf("result %d signal %q = %v, want %v", i, signal, got[i].Signals[signal], wantScore)
			}
		}
	}
	if stages["snapshot_load_decode"] != 1 {
		t.Errorf("snapshot loads = %d, want 1", stages["snapshot_load_decode"])
	}
	if stages["finalist_hydration"] != 2 {
		t.Errorf("finalist hydration stages = %d, want 2 searches", stages["finalist_hydration"])
	}
}

func TestSQLiteSnapshotRejectsMixedStoredDimensionsWithoutPublishing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.db")
	w, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	for _, row := range []struct {
		id        string
		embedding string
	}{{"one", "[1]"}, {"two", "[1,0]"}} {
		if _, err := w.db.Exec(`
			INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			VALUES (?, 'body', 'mixed.go', 1, 1, 'go', '{}', ?, 1, '', '', 'test/v1')`, row.id, row.embedding); err != nil {
			t.Fatalf("insert %q: %v", row.id, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	ro, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	if _, err := ro.ProbeVectorSpaces(context.Background()); err == nil || !strings.Contains(err.Error(), "dimension mismatch") {
		t.Fatalf("ProbeVectorSpaces error = %v, want dimension mismatch", err)
	}
	if residentSnapshot(ro) != nil {
		t.Fatal("failed mixed-dimension load published a snapshot")
	}
}

func TestSQLiteSnapshotConcurrentSearchesShareOneLoad(t *testing.T) {
	chunks := make([]Chunk, 32)
	embeddings := make([][]float64, len(chunks))
	for i := range chunks {
		chunks[i] = Chunk{ID: fmt.Sprintf("chunk-%02d", i), Content: "body", Source: "all.go", Metadata: map[string]string{}}
		embeddings[i] = []float64{float64(i + 1), 1}
	}
	ro := newReadOnlyTestStore(t, chunks, embeddings, "test/v1")
	var loads atomic.Int32
	ro.recordStage = func(stage string, _ time.Duration) {
		if stage == "snapshot_load_decode" {
			loads.Add(1)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ro.Search(context.Background(), []float64{1, 0}, 5)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Search: %v", err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("snapshot loads = %d, want 1", got)
	}
}

func TestSQLiteSnapshotDecodeFailureIsNotPublishedAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.db")
	w, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	seedTestStoreWithVectorSpaceID(t, w,
		[]Chunk{{ID: "broken", Content: "body", Source: "broken.go", Metadata: map[string]string{}}},
		[][]float64{{1, 0}}, "test/v1")
	if _, err := w.db.Exec(`UPDATE chunks SET embedding = '{' WHERE id = 'broken'`); err != nil {
		t.Fatalf("corrupt embedding: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	ro, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()
	var loads atomic.Int32
	ro.recordStage = func(stage string, _ time.Duration) {
		if stage == "snapshot_load_decode" {
			loads.Add(1)
		}
	}

	for range 2 {
		if _, err := ro.ProbeVectorSpaces(context.Background()); err == nil || !strings.Contains(err.Error(), "decode embedding for chunk") {
			t.Fatalf("ProbeVectorSpaces error = %v, want embedding decode failure", err)
		}
		if residentSnapshot(ro) != nil {
			t.Fatal("failed decode published a snapshot")
		}
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("load attempts = %d, want 2 retries", got)
	}
}

func TestSQLiteSnapshotProbeReturnsDefensiveCopy(t *testing.T) {
	ro := newReadOnlyTestStore(t,
		[]Chunk{{ID: "a", Content: "body", Source: "a.go", Metadata: map[string]string{}}},
		[][]float64{{1}}, "test/v1")
	first, err := ro.ProbeVectorSpaces(context.Background())
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	first.KnownIDs[0] = "mutated"
	second, err := ro.ProbeVectorSpaces(context.Background())
	if err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if !reflect.DeepEqual(second.KnownIDs, []string{"test/v1"}) {
		t.Fatalf("cached probe was mutated: %v", second.KnownIDs)
	}
}

func TestSQLiteSnapshotScoringHonorsCancellation(t *testing.T) {
	chunks := []Chunk{
		{ID: "a", Content: "a", Source: "a.go", Metadata: map[string]string{}},
		{ID: "b", Content: "b", Source: "b.go", Metadata: map[string]string{}},
		{ID: "c", Content: "c", Source: "c.go", Metadata: map[string]string{}},
		{ID: "d", Content: "d", Source: "d.go", Metadata: map[string]string{}},
	}
	ro := newReadOnlyTestStore(t, chunks, [][]float64{{1, 0}, {0, 1}, {1, 1}, {-1, 0}}, "test/v1")
	if _, err := ro.ProbeVectorSpaces(context.Background()); err != nil {
		t.Fatalf("warm snapshot: %v", err)
	}
	ctx := newCancelAfterChecksContext(2)
	if _, err := ro.Search(ctx, []float64{1, 0}, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search error = %v, want context.Canceled", err)
	}
}

func TestSelectSnapshotTopKMatchesStableFullSort(t *testing.T) {
	scores := []float64{0.2, 0.9, 0.9, -0.1, 0.7, 0.7, 0.4}
	for _, k := range []int{1, 3, 0, len(scores) + 5} {
		got, err := selectSnapshotTopK(context.Background(), scores, k)
		if err != nil {
			t.Fatalf("k=%d: selectSnapshotTopK: %v", k, err)
		}
		want := make([]snapshotCandidate, len(scores))
		for i, score := range scores {
			want[i] = snapshotCandidate{index: i, score: score}
		}
		sort.SliceStable(want, func(i, j int) bool { return candidateBetter(want[i], want[j]) })
		limit := k
		if limit <= 0 || limit > len(want) {
			limit = len(want)
		}
		want = want[:limit]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("k=%d candidates = %v, want %v", k, got, want)
		}
	}
}

func TestSQLiteSnapshotSemanticTopKMatchesFullScoring(t *testing.T) {
	chunks := make([]Chunk, 7)
	embeddings := [][]float64{{1, 0}, {2, 0}, {0.7, 0.7}, {0, 1}, {-1, 0}, {0.7, 0.7}, {0, -1}}
	for i := range chunks {
		chunks[i] = Chunk{ID: fmt.Sprintf("chunk-%d", i), Content: "body", Source: "all.go", Metadata: map[string]string{}}
	}
	ro := newReadOnlyTestStore(t, chunks, embeddings, "test/v1")
	snapshot, err := ro.sqliteSnapshot(context.Background())
	if err != nil {
		t.Fatalf("sqliteSnapshot: %v", err)
	}

	got, err := snapshot.semanticTopK(context.Background(), []float64{1, 0}, 3)
	if err != nil {
		t.Fatalf("semanticTopK: %v", err)
	}
	scores, err := snapshot.semanticScores(context.Background(), []float64{1, 0})
	if err != nil {
		t.Fatalf("semanticScores: %v", err)
	}
	want, err := selectSnapshotTopK(context.Background(), scores, 3)
	if err != nil {
		t.Fatalf("selectSnapshotTopK: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("semanticTopK = %v, want %v", got, want)
	}
}
