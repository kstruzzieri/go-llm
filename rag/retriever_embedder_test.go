package rag

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewRetrieverWithEmbedder_NilEmbedderRejected(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	r, err := NewRetrieverWithEmbedder(nil, store)
	if err == nil {
		t.Fatal("expected error for nil embedder, got nil")
	}
	if r != nil {
		t.Errorf("expected nil retriever on error, got %v", r)
	}
	if !strings.Contains(err.Error(), "NewRetrieverWithEmbedder") {
		t.Errorf("error = %q, want it to identify the constructor", err.Error())
	}
}

func TestNewRetrieverWithEmbedder_NilStoreRejected(t *testing.T) {
	emb := &recordingEmbedder{}

	tests := []struct {
		name  string
		store VectorStore
	}{
		{name: "nil interface", store: nil},
		{name: "typed nil", store: (*SQLiteStore)(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRetrieverWithEmbedder(emb, tt.store)
			if err == nil {
				t.Fatal("expected error for nil store, got nil")
			}
			if r != nil {
				t.Errorf("expected nil retriever on error, got %v", r)
			}
			if !strings.Contains(err.Error(), "store") {
				t.Errorf("error = %q, want it to mention store", err.Error())
			}
		})
	}
}

// TestNewRetrieverWithEmbedder_AppliesRetrieverModel verifies WithRetrieverModel
// surfaces the configured model in the embedder call (observable via
// recordingEmbedder), rather than asserting on private struct fields.
func TestNewRetrieverWithEmbedder_AppliesRetrieverModel(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings: [][]float64{{1, 2, 3}},
	}}
	r, err := NewRetrieverWithEmbedder(emb, store, WithRetrieverModel("test-model"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := r.Retrieve(context.Background(), "query", 5); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if emb.calls != 1 {
		t.Errorf("embedder calls = %d, want 1", emb.calls)
	}
	if emb.model != "test-model" {
		t.Errorf("embedder saw model = %q, want %q", emb.model, "test-model")
	}
}

func TestRetrieve_VSIDMismatch_errsClosed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "stored", Source: "s.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "s.go", chunks, [][]float64{{1, 0}}, "hash", "ollama/nomic-embed-text"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0}},
		Provider:      "ollama",
		Model:         "qwen3-embedding:8b",
		VectorSpaceID: "ollama/qwen3-embedding:8b",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrVectorSpaceMismatch) {
		t.Fatalf("Retrieve error = %v, want ErrVectorSpaceMismatch", err)
	}
	for _, want := range []string{"ollama/nomic-embed-text", "ollama/qwen3-embedding:8b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Retrieve error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if spyStore.searchCalls != 0 {
		t.Fatalf("Search calls = %d, want 0 on vector-space mismatch", spyStore.searchCalls)
	}
}

func TestRetrieve_VSIDMatch_proceeds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "stored", Source: "s.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "s.go", chunks, [][]float64{{1, 0}}, "hash", "X"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0}},
		VectorSpaceID: "X",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "question", 1)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if spyStore.searchCalls != 1 {
		t.Fatalf("Search calls = %d, want 1", spyStore.searchCalls)
	}
	if len(results) != 1 || results[0].Chunk.ID != "c1" {
		t.Fatalf("results = %+v, want c1", results)
	}
}

func TestRetrieve_emptyQueryVSID_errs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "stored", Source: "s.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "s.go", chunks, [][]float64{{1, 0}}, "hash", "ollama/indexed"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrVectorSpaceMismatch) {
		t.Fatalf("Retrieve error = %v, want ErrVectorSpaceMismatch", err)
	}
	if !strings.Contains(err.Error(), "no VectorSpaceID") {
		t.Errorf("Retrieve error = %q, want it to mention missing VectorSpaceID", err.Error())
	}
	if spyStore.searchCalls != 0 {
		t.Fatalf("Search calls = %d, want 0 on missing query vector-space id", spyStore.searchCalls)
	}
}

func TestRetrieve_mixedCorpus_errs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertVSIDRow(t, store, "a", "Z")
	insertVSIDRow(t, store, "b", "A")
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "A",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
		t.Fatalf("Retrieve error = %v, want ErrCorpusMixedVectorSpaces", err)
	}
	for _, want := range []string{"A", "Z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Retrieve error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if spyStore.searchCalls != 0 {
		t.Fatalf("Search calls = %d, want 0 on mixed corpus", spyStore.searchCalls)
	}
}

func TestRetrieve_partialMigration_errs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		insertVSIDRow(t, store, "known-"+string(rune('a'+i)), "X")
		insertVSIDRow(t, store, "legacy-"+string(rune('a'+i)), "")
	}
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "X",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
		t.Fatalf("Retrieve error = %v, want ErrCorpusMixedVectorSpaces", err)
	}
	for _, want := range []string{"X", "unknown legacy vector space"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Retrieve error = %q, want it to contain %q", err.Error(), want)
		}
	}
	if spyStore.searchCalls != 0 {
		t.Fatalf("Search calls = %d, want 0 on partial migration", spyStore.searchCalls)
	}
}

func TestRetrieve_fullyLegacy_proceeds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertVSIDRow(t, store, "a", "")
	insertVSIDRow(t, store, "b", "")
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "new-space",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "question", 5)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if spyStore.searchCalls != 1 {
		t.Fatalf("Search calls = %d, want 1", spyStore.searchCalls)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
}

func TestRetrieve_emptyCorpus_proceeds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	spyStore := &countingSQLiteRetrieverStore{SQLiteStore: store}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "new-space",
	}}
	r, err := NewRetrieverWithEmbedder(emb, spyStore)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(ctx, "question", 5)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if spyStore.searchCalls != 1 {
		t.Fatalf("Search calls = %d, want 1", spyStore.searchCalls)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

func TestRetrieve_searchWrapperPreserved(t *testing.T) {
	errBoom := errors.New("boom")
	store := &retrieverProbingStore{
		retrieverPlainStore: retrieverPlainStore{searchErr: errBoom},
		probe:               vectorSpaceProbe{KnownIDs: []string{"X"}},
	}
	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "X",
	}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(context.Background(), "question", 1)
	if err == nil {
		t.Fatal("Retrieve error = nil, want search error")
	}
	if !strings.HasPrefix(err.Error(), "rag: search:") {
		t.Fatalf("Retrieve error = %q, want prefix %q", err.Error(), "rag: search:")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("Retrieve error = %v, want it to wrap errBoom", err)
	}
	if !errors.Is(err, ErrStoreOperation) {
		t.Fatalf("Retrieve error = %v, want it to wrap ErrStoreOperation", err)
	}
}

func TestRetrieve_storeWithoutProber_skipsCheck(t *testing.T) {
	store := &retrieverPlainStore{
		searchResults: []SearchResult{{Chunk: Chunk{ID: "c1"}, Score: 1}},
	}
	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		VectorSpaceID: "query-space",
	}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	results, err := r.Retrieve(context.Background(), "question", 1)
	if err != nil {
		t.Fatalf("Retrieve error: %v", err)
	}
	if store.searchCalls != 1 {
		t.Fatalf("Search calls = %d, want 1", store.searchCalls)
	}
	if len(results) != 1 || results[0].Chunk.ID != "c1" {
		t.Fatalf("results = %+v, want c1", results)
	}
}

type countingSQLiteRetrieverStore struct {
	*SQLiteStore
	searchCalls int
}

func (s *countingSQLiteRetrieverStore) Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	s.searchCalls++
	return s.SQLiteStore.Search(ctx, queryEmbedding, k)
}

type retrieverPlainStore struct {
	searchResults []SearchResult
	searchErr     error
	searchCalls   int
}

func (s *retrieverPlainStore) Store(context.Context, []Chunk, [][]float64) error {
	return nil
}

func (s *retrieverPlainStore) Search(context.Context, []float64, int) ([]SearchResult, error) {
	s.searchCalls++
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return s.searchResults, nil
}

func (s *retrieverPlainStore) DeleteBySource(context.Context, string) error {
	return nil
}

func (s *retrieverPlainStore) Stats(context.Context) (StoreStats, error) {
	return StoreStats{}, nil
}

func (s *retrieverPlainStore) Close() error {
	return nil
}

type retrieverProbingStore struct {
	retrieverPlainStore
	probe    vectorSpaceProbe
	probeErr error
}

func (s *retrieverProbingStore) ProbeVectorSpaces(context.Context) (vectorSpaceProbe, error) {
	if s.probeErr != nil {
		return vectorSpaceProbe{}, s.probeErr
	}
	return s.probe, nil
}
