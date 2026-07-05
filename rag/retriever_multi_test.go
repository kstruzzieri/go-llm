package rag

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// retrieverMultiStore is a plain store that additionally implements
// MultiSignalSearcher, so Retrieve should prefer SearchMulti over Search.
type retrieverMultiStore struct {
	retrieverPlainStore
	multiResults     []ScoredResult
	multiErr         error
	searchMultiCalls int
	gotQuery         string
	gotK             int
}

func (s *retrieverMultiStore) SearchMulti(_ context.Context, _ []float64, query string,
	k int, _ QueryContext) ([]ScoredResult, error) {
	s.searchMultiCalls++
	s.gotQuery = query
	s.gotK = k
	if s.multiErr != nil {
		return nil, s.multiErr
	}
	return s.multiResults, nil
}

// retrieverMultiProbingStore adds vector-space probing on top of the
// multi-signal store, to prove vsid validation still gates the hybrid path.
type retrieverMultiProbingStore struct {
	retrieverMultiStore
	probe VectorSpaceProbe
}

func (s *retrieverMultiProbingStore) ProbeVectorSpaces(context.Context) (VectorSpaceProbe, error) {
	return s.probe, nil
}

func TestRetrieve_usesSearchMultiWhenAvailable(t *testing.T) {
	store := &retrieverMultiStore{
		multiResults: []ScoredResult{
			// SearchMulti reports semantic distance; Retrieve must derive the
			// returned Score from it (Score == 1 - Distance), not pass through.
			{SearchResult: SearchResult{Chunk: Chunk{ID: "m1"}, Distance: 0.1, Score: 0.0163}},
		},
	}
	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(context.Background(), "myquery", 3)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if store.searchMultiCalls != 1 {
		t.Fatalf("SearchMulti calls = %d, want 1", store.searchMultiCalls)
	}
	if store.searchCalls != 0 {
		t.Fatalf("Search calls = %d, want 0 when SearchMulti is available", store.searchCalls)
	}
	if store.gotQuery != "myquery" || store.gotK != 3 {
		t.Fatalf("SearchMulti got query=%q k=%d, want %q 3", store.gotQuery, store.gotK, "myquery")
	}
	if len(results) != 1 || results[0].Chunk.ID != "m1" {
		t.Fatalf("results = %+v, want one m1", results)
	}
	if results[0].Score != 0.9 {
		t.Fatalf("Score = %v, want 0.9 (1 - Distance), not the raw RRF score", results[0].Score)
	}
}

// TestRetrieve_hybridScoreContract drives the real SQLiteStore.SearchMulti and
// proves the Score field stays a 0..1 similarity consistent with Distance,
// rather than leaking raw RRF magnitudes (which BuildContext labels as
// "similarity") into callers.
func TestRetrieve_hybridScoreContract(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedHybridCorpus(t, store)

	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "alpha", 5)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("results empty, want hybrid results")
	}
	for _, res := range results {
		if res.Score < 0 || res.Score > 1 {
			t.Fatalf("Score = %v for %q, want a 0..1 similarity", res.Score, res.Chunk.ID)
		}
		if math.Abs(res.Score-(1-res.Distance)) > 1e-9 {
			t.Fatalf("Score=%v Distance=%v for %q, want Score == 1 - Distance",
				res.Score, res.Distance, res.Chunk.ID)
		}
	}
}

// TestRetrieve_readOnlyStore_usesHybrid guards the Golem index path: an
// immutable read-only store (no migrations) must still serve hybrid retrieval,
// proving chunks_fts written at index time is queryable read-only.
func TestRetrieve_readOnlyStore_usesHybrid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	ctx := context.Background()

	w, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	seedHybridCorpus(t, w)
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	ro, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, ro)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "alpha", 5)
	if err != nil {
		t.Fatalf("read-only hybrid Retrieve error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("read-only results empty, want hybrid results")
	}
}

// TestRetrieve_vectorOnlyOption_usesSearch proves WithVectorOnly bypasses the
// hybrid path on a store that does support it, falling back to cosine Search.
func TestRetrieve_vectorOnlyOption_usesSearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedHybridCorpus(t, store)
	spy := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, spy, WithVectorOnly())
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "alpha", 5)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if spy.searchCalls != 1 {
		t.Fatalf("Search calls = %d, want 1 under WithVectorOnly", spy.searchCalls)
	}
	if spy.searchMultiCalls != 0 {
		t.Fatalf("SearchMulti calls = %d, want 0 under WithVectorOnly", spy.searchMultiCalls)
	}
	if len(results) == 0 {
		t.Fatal("results empty, want cosine results")
	}
}

// TestRetrieve_searchMultiError_wrapped proves a SearchMulti failure is
// surfaced (not swallowed) and tagged as the hybrid path.
func TestRetrieve_searchMultiError_wrapped(t *testing.T) {
	boom := errors.New("boom")
	store := &retrieverMultiStore{multiErr: boom}
	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(context.Background(), "question", 1)
	if !errors.Is(err, ErrStoreOperation) {
		t.Fatalf("Retrieve error = %v, want it to wrap ErrStoreOperation", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Retrieve error = %v, want it to wrap the underlying error", err)
	}
	if got := err.Error(); !strings.Contains(got, "rag: hybrid search:") {
		t.Fatalf("Retrieve error = %q, want it to identify the hybrid path", got)
	}
}

func TestRetrieve_multiStore_vsidMismatch_skipsSearchMulti(t *testing.T) {
	store := &retrieverMultiProbingStore{probe: VectorSpaceProbe{KnownIDs: []string{"X"}}}
	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0}},
		VectorSpaceID: "Y",
	}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(context.Background(), "question", 1)
	if !errors.Is(err, ErrVectorSpaceMismatch) {
		t.Fatalf("Retrieve error = %v, want ErrVectorSpaceMismatch", err)
	}
	if store.searchMultiCalls != 0 {
		t.Fatalf("SearchMulti calls = %d, want 0 on vector-space mismatch", store.searchMultiCalls)
	}
}

func TestRetrieveScored_usesSearchMulti(t *testing.T) {
	store := &retrieverMultiStore{
		multiResults: []ScoredResult{
			{
				SearchResult: SearchResult{Chunk: Chunk{ID: "c1"}, Score: 0.9, Distance: 0.1},
				RankScore:    0.033,
				Signals:      map[string]float64{"semantic": 0.9, "keyword": 0.5},
			},
		},
	}
	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	got, err := r.RetrieveScored(context.Background(), "q", 5, QueryContext{})
	if err != nil {
		t.Fatalf("RetrieveScored: %v", err)
	}
	if store.searchMultiCalls != 1 {
		t.Fatalf("SearchMulti calls = %d, want 1", store.searchMultiCalls)
	}
	if len(got) != 1 || got[0].Chunk.ID != "c1" {
		t.Fatalf("unexpected results: %+v", got)
	}
	// The scored surface preserves Signals (unlike Retrieve, which flattens them).
	if got[0].Signals["keyword"] != 0.5 {
		t.Errorf("keyword signal = %v, want 0.5 (Signals must be preserved)", got[0].Signals["keyword"])
	}
	if got[0].RankScore != 0.033 {
		t.Errorf("RankScore = %v, want 0.033", got[0].RankScore)
	}
}

// seedHybridCorpus stores a small content-bearing corpus (so FTS5 is
// populated) with orthonormal embeddings for deterministic semantic scores.
func seedHybridCorpus(t *testing.T, store *SQLiteStore) {
	t.Helper()
	chunks := []Chunk{
		{ID: "c1", Content: "alpha beta gamma", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "delta epsilon zeta", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "eta theta iota", Source: "c.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	if err := store.Store(context.Background(), chunks, embeddings); err != nil {
		t.Fatalf("seed corpus: %v", err)
	}
}

// BenchmarkRetrieve measures the default hybrid path against the vector-only
// fallback on a populated store, documenting the per-query cost of making
// hybrid the default (#225).
func BenchmarkRetrieve(b *testing.B) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		b.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	const n = 200
	chunks := make([]Chunk, n)
	embeddings := make([][]float64, n)
	for i := range chunks {
		chunks[i] = Chunk{
			ID:       "c" + strconv.Itoa(i),
			Content:  "alpha beta gamma delta token" + strconv.Itoa(i),
			Source:   "f" + strconv.Itoa(i) + ".go",
			Metadata: map[string]string{},
		}
		embeddings[i] = []float64{float64(i % 7), float64(i % 3), 1}
	}
	if err := store.Store(context.Background(), chunks, embeddings); err != nil {
		b.Fatalf("seed: %v", err)
	}
	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1, 1}}}}

	b.Run("hybrid", func(b *testing.B) {
		r, _ := NewRetrieverWithEmbedder(emb, store)
		for i := 0; i < b.N; i++ {
			if _, err := r.Retrieve(context.Background(), "alpha token", 5); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("vector_only", func(b *testing.B) {
		r, _ := NewRetrieverWithEmbedder(emb, store, WithVectorOnly())
		for i := 0; i < b.N; i++ {
			if _, err := r.Retrieve(context.Background(), "alpha token", 5); err != nil {
				b.Fatal(err)
			}
		}
	})
}
