package rag

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var errTestWeighter = errors.New("test weighter failure")

func TestSearchMulti_ScoreContractAndOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "golang concurrency", Source: "main.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c2", Content: "python typing", Source: "ml.py", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c3", Content: "golang errors", Source: "errors.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1, 0, 0}, {0, 1, 0}, {0.8, 0, 0.6}}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	results, err := store.SearchMulti(ctx, []float64{0.9, 0, 0.1}, "golang", 3, QueryContext{Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}

	for i, r := range results {
		// Score is semantic cosine similarity, honoring the store.go contract.
		if got := r.Signals["semantic"]; math.Abs(r.Score-got) > 1e-9 {
			t.Errorf("result %d: Score=%v, want semantic signal %v", i, r.Score, got)
		}
		if math.Abs(r.Score-(1-r.Distance)) > 1e-9 {
			t.Errorf("result %d: Score=%v Distance=%v, want Score == 1-Distance", i, r.Score, r.Distance)
		}
		// RankScore is the fused sort key: the slice is non-increasing in RankScore.
		if i > 0 && results[i-1].RankScore < r.RankScore {
			t.Errorf("not sorted by RankScore: [%d]=%v < [%d]=%v",
				i-1, results[i-1].RankScore, i, r.RankScore)
		}
	}
}

func TestSearchMultiBasic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store chunks with distinct content and embeddings.
	chunks := []Chunk{
		{ID: "c1", Content: "golang concurrency patterns", Source: "main.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c2", Content: "python machine learning", Source: "ml.py", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c3", Content: "golang error handling", Source: "errors.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.8, 0.0, 0.6},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Query with an embedding similar to c1 and c3, and keyword "golang".
	queryEmb := []float64{0.9, 0.0, 0.1}
	qCtx := QueryContext{
		CurrentFile: "main.go",
		Timestamp:   time.Now(),
	}

	results, err := store.SearchMulti(ctx, queryEmb, "golang", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank highest: best semantic match + keyword match + structural (same file).
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 as top result, got %s", results[0].Chunk.ID)
	}

	// Verify signal breakdowns are populated.
	for i, r := range results {
		if r.Signals == nil {
			t.Errorf("result %d has nil Signals", i)
			continue
		}
		if _, ok := r.Signals["semantic"]; !ok {
			t.Errorf("result %d missing 'semantic' signal", i)
		}
		if _, ok := r.Signals["keyword"]; !ok {
			t.Errorf("result %d missing 'keyword' signal", i)
		}
		if _, ok := r.Signals["temporal"]; !ok {
			t.Errorf("result %d missing 'temporal' signal", i)
		}
		if _, ok := r.Signals["structural"]; !ok {
			t.Errorf("result %d missing 'structural' signal", i)
		}
	}
}

func TestSearchMultiEmptyStore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	results, err := store.SearchMulti(ctx, []float64{1.0, 0.0}, "test", 5, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty store, got %d", len(results))
	}
}

func TestSearchMultiRejectsDimensionMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "test content", Source: "test.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{0.1, 0.2, 0.3, 0.4}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	_, err := store.SearchMulti(ctx, []float64{0.1, 0.2, 0.3}, "test", 5, QueryContext{})
	if err == nil {
		t.Fatal("expected dimension-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "dimension mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "dimension mismatch")
	}
}

func TestSearchMultiKeywordBoostAffectsRanking(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two chunks with identical embeddings but different content.
	chunks := []Chunk{
		{ID: "c1", Content: "database optimization query performance", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "unrelated topic discussion", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	// Identical embeddings — semantic signal alone cannot distinguish them.
	embeddings := [][]float64{
		{0.5, 0.5},
		{0.5, 0.5},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	qCtx := QueryContext{Timestamp: time.Now()}
	results, err := store.SearchMulti(ctx, []float64{0.5, 0.5}, "database", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank higher due to keyword match on "database".
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 ranked first (keyword match), got %s", results[0].Chunk.ID)
	}

	// c1 should have a higher keyword signal than c2.
	if results[0].Signals["keyword"] <= results[1].Signals["keyword"] {
		t.Errorf("expected c1 keyword score > c2 keyword score, got %f <= %f",
			results[0].Signals["keyword"], results[1].Signals["keyword"])
	}
}

func TestSearchMultiStructuralBoost(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "function handler", Source: "pkg/handler.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "function handler", Source: "other/handler.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0},
		{1.0, 0.0},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Query from the same directory as c1.
	qCtx := QueryContext{
		CurrentFile: "pkg/main.go",
		Timestamp:   time.Now(),
	}
	results, err := store.SearchMulti(ctx, []float64{1.0, 0.0}, "handler", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank higher: same directory as current file.
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 ranked first (structural proximity), got %s", results[0].Chunk.ID)
	}
	if results[0].Signals["structural"] <= results[1].Signals["structural"] {
		t.Errorf("expected c1 structural > c2 structural, got %f <= %f",
			results[0].Signals["structural"], results[1].Signals["structural"])
	}
}

func TestSearchMultiReturnsAllSignals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "test content", Source: "test.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	qCtx := QueryContext{
		CurrentFile: "test.go",
		Timestamp:   time.Now(),
	}
	results, err := store.SearchMulti(ctx, []float64{1.0}, "test", 1, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	expectedSignals := []string{"semantic", "keyword", "temporal", "structural"}
	for _, sig := range expectedSignals {
		if _, ok := results[0].Signals[sig]; !ok {
			t.Errorf("missing signal %q in results", sig)
		}
	}

	// Semantic should be 1.0 (identical embedding).
	if results[0].Signals["semantic"] != 1.0 {
		t.Errorf("expected semantic=1.0, got %f", results[0].Signals["semantic"])
	}
	// Structural should be 1.0 (same file).
	if results[0].Signals["structural"] != 1.0 {
		t.Errorf("expected structural=1.0, got %f", results[0].Signals["structural"])
	}
}

func TestSearchMultiImplementsInterface(t *testing.T) {
	// Compile-time check is in search_multi.go, but verify at runtime too.
	store := newTestStore(t)
	var ms MultiSignalSearcher = store
	_ = ms
}

func TestComputeRanks(t *testing.T) {
	tests := []struct {
		name   string
		scores []float64
		want   []int
	}{
		{"descending", []float64{0.9, 0.7, 0.5}, []int{1, 2, 3}},
		{"ascending", []float64{0.1, 0.5, 0.9}, []int{3, 2, 1}},
		{"single", []float64{0.5}, []int{1}},
		{"empty", []float64{}, []int{}},
		{"equal", []float64{0.5, 0.5, 0.5}, []int{1, 1, 1}}, // ties get the same rank
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRanks(tt.scores)
			if len(got) != len(tt.want) {
				t.Fatalf("len(ranks) = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("rank[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// behavioralFixture stores three chunks with StableKeys and returns the store.
// T is the relevant chunk (top semantic+keyword), M is middling, B is
// irrelevant. qCtx is empty so temporal is uniform (all indexed together) and
// structural is zero for all — isolating behavioral per the spec.
func behavioralFixture(t *testing.T) *SQLiteStore {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	chunks := []Chunk{
		{ID: "T", StableKey: "skT", Content: "golang concurrency channels", Source: "a.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "M", StableKey: "skM", Content: "golang slices", Source: "b.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "B", StableKey: "skB", Content: "python pandas dataframe", Source: "c.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0}, // T closest to query
		{0.6, 0.0, 0.8}, // M partial
		{0.0, 1.0, 0.0}, // B orthogonal
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}
	return store
}

func ids(rs []ScoredResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Chunk.ID
	}
	return out
}

// query "golang concurrency", embedding aligned with T.
var (
	behQuery = "golang concurrency"
	behEmb   = []float64{0.95, 0.0, 0.1}
)

func TestSearchMultiNilWeighterNoBehavioralSignal(t *testing.T) {
	store := behavioralFixture(t)
	res, err := store.SearchMulti(context.Background(), behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	for _, r := range res {
		if _, ok := r.Signals["behavioral"]; ok {
			t.Errorf("nil weighter must not add a 'behavioral' signal key, got %v", r.Signals)
		}
	}
}

func TestSearchMultiColdStartInert(t *testing.T) {
	store := behavioralFixture(t)
	ctx := context.Background()
	base, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// All-zero weighter == cold-start: contribution skipped, raw scores unchanged.
	store.SetBehavioralWeighter(&fakeWeighter{weights: map[string]float64{}})
	got, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("with weighter: %v", err)
	}
	for i := range base {
		if got[i].Chunk.ID != base[i].Chunk.ID || got[i].Score != base[i].Score {
			t.Errorf("cold-start not inert at %d: base=(%s,%v) got=(%s,%v)",
				i, base[i].Chunk.ID, base[i].Score, got[i].Chunk.ID, got[i].Score)
		}
	}
}

func TestSearchMultiFailOpenPreservesBaseline(t *testing.T) {
	store := behavioralFixture(t)
	ctx := context.Background()
	base, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	store.SetBehavioralWeighter(&fakeWeighter{err: errTestWeighter})
	got, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti must not surface weighter error: %v", err)
	}
	for i := range base {
		if got[i].Chunk.ID != base[i].Chunk.ID || got[i].Score != base[i].Score {
			t.Errorf("fail-open not inert at %d: base=(%s,%v) got=(%s,%v)",
				i, base[i].Chunk.ID, base[i].Score, got[i].Chunk.ID, got[i].Score)
		}
	}
}

func TestSearchMultiPropagatesBehavioralCancellation(t *testing.T) {
	store := behavioralFixture(t)
	store.SetBehavioralWeighter(&fakeWeighter{err: context.Canceled})
	_, err := store.SearchMulti(context.Background(), behEmb, behQuery, 3, QueryContext{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SearchMulti error = %v, want context.Canceled", err)
	}
}

// Dominance: a behavior-only favorite (B, irrelevant) with a huge weight cannot
// outrank T, which is rank-1 in both semantic and keyword. Temporal/structural
// are uniform (empty qCtx, co-indexed), isolating behavioral per the spec.
func TestSearchMultiBehavioralCannotDominate(t *testing.T) {
	store := behavioralFixture(t)
	store.SetBehavioralWeighter(&fakeWeighter{weights: map[string]float64{"skB": 100.0}})
	res, err := store.SearchMulti(context.Background(), behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if res[0].Chunk.ID != "T" {
		t.Errorf("behavior-only favorite dominated: top=%s, want T; order=%v", res[0].Chunk.ID, ids(res))
	}
}

// Warmed feedback reorders genuinely near-tied candidates: with M and B close on
// relevance, a strong positive weight on B lifts it above M.
func TestSearchMultiWarmedReordersNearTies(t *testing.T) {
	store := behavioralFixture(t)
	ctx := context.Background()
	base, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	basePos := map[string]int{}
	for i, id := range ids(base) {
		basePos[id] = i
	}
	// Favor B heavily; it should climb at least one position vs baseline.
	store.SetBehavioralWeighter(&fakeWeighter{weights: map[string]float64{"skB": 50.0}})
	got, err := store.SearchMulti(ctx, behEmb, behQuery, 3, QueryContext{})
	if err != nil {
		t.Fatalf("warmed: %v", err)
	}
	gotPos := map[string]int{}
	for i, id := range ids(got) {
		gotPos[id] = i
	}
	if gotPos["B"] >= basePos["B"] {
		t.Errorf("expected B to climb with positive feedback: base pos=%d got pos=%d (order %v)",
			basePos["B"], gotPos["B"], ids(got))
	}
	if got[0].Chunk.ID != "T" {
		t.Errorf("T must remain top (relevance winner), got %v", ids(got))
	}
}
