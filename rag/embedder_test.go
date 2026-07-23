package rag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// recordingEmbedder is the canonical mock for rag tests that need to inspect
// how callers invoke an Embedder. Use this pattern in new tests rather than
// spinning up an httptest mock Ollama unless the test specifically exercises
// the ollama compat shim.
type recordingEmbedder struct {
	model      string
	inputs     []string
	calls      int
	result     EmbedResult
	err        error
	beforeFunc func(ctx context.Context)
}

func (r *recordingEmbedder) Embed(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
	if r.beforeFunc != nil {
		r.beforeFunc(ctx)
	}
	r.calls++
	r.model = model
	r.inputs = append([]string(nil), inputs...)
	if r.err != nil {
		return EmbedResult{}, r.err
	}
	return r.result, nil
}

func TestEmbedderFunc_Delegates(t *testing.T) {
	called := 0
	var gotModel string
	var gotInputs []string
	want := EmbedResult{Embeddings: [][]float64{{1, 2, 3}}, Model: "m", Provider: "fake", VectorSpaceID: "fake/m"}
	e := EmbedderFunc(func(_ context.Context, model string, inputs []string) (EmbedResult, error) {
		called++
		gotModel = model
		gotInputs = inputs
		return want, nil
	})
	got, err := e.Embed(context.Background(), "m", []string{"a"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if called != 1 || gotModel != "m" || len(gotInputs) != 1 || gotInputs[0] != "a" {
		t.Errorf("delegation incorrect: calls=%d model=%q inputs=%v", called, gotModel, gotInputs)
	}
	if got.VectorSpaceID != want.VectorSpaceID {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestEmbedderFromOllamaClient_NilReturnsNil(t *testing.T) {
	if got := embedderFromOllamaClient(nil); got != nil {
		t.Errorf("expected nil, got %T", got)
	}
}

func TestEmbedderFromOllamaClient_EmptyInputShortCircuits(t *testing.T) {
	// Server that fails the test if hit — proves we did not call Ollama.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	emb := embedderFromOllamaClient(c)
	if emb == nil {
		t.Fatal("expected non-nil embedder")
	}

	res, err := emb.Embed(context.Background(), "m", nil)
	if err != nil {
		t.Errorf("nil inputs: err = %v, want nil", err)
	}
	if len(res.Embeddings) != 0 {
		t.Errorf("nil inputs: got %d embeddings, want 0", len(res.Embeddings))
	}

	res, err = emb.Embed(context.Background(), "m", []string{})
	if err != nil {
		t.Errorf("empty inputs: err = %v, want nil", err)
	}
	if len(res.Embeddings) != 0 {
		t.Errorf("empty inputs: got %d embeddings, want 0", len(res.Embeddings))
	}

	if hits != 0 {
		t.Errorf("Ollama was contacted %d times for empty inputs; want 0", hits)
	}
}

func TestEmbedderFromOllamaClient_PopulatesProvenance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2]]}`))
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	emb := embedderFromOllamaClient(c)
	res, err := emb.Embed(context.Background(), "test-model", []string{"hello"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Model != "test-model" {
		t.Errorf("Model = %q, want %q", res.Model, "test-model")
	}
	if res.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", res.Provider, "ollama")
	}
	if res.VectorSpaceID != "ollama/test-model" {
		t.Errorf("VectorSpaceID = %q, want %q", res.VectorSpaceID, "ollama/test-model")
	}
	if len(res.Embeddings) != 1 || len(res.Embeddings[0]) != 2 {
		t.Errorf("Embeddings shape = %v, want [[_,_]]", res.Embeddings)
	}
}

func TestIndexer_LengthMismatchSurfacesError(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Embedder returns one fewer embedding than chunks. Indexer must error.
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		out := make([][]float64, len(inputs)-1)
		for i := range out {
			out[i] = []float64{1, 2, 3}
		}
		return EmbedResult{Embeddings: out, Model: "m", Provider: "fake"}, nil
	})

	idx, err := NewIndexerWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Use a chunker that yields >=2 chunks for any non-empty content; the
	// default code chunker may yield 1, so override with a deterministic mock.
	idx.chunker = chunkerFunc(func(_ string, content string) ([]Chunk, error) {
		return []Chunk{
			{ID: "a", Content: content, Source: "f.go", StartLine: 1, EndLine: 1},
			{ID: "b", Content: content, Source: "f.go", StartLine: 2, EndLine: 2},
		}, nil
	})

	tmp := t.TempDir()
	path := tmp + "/f.go"
	if err := writeFileImpl(path, "package x"); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = idx.IndexFile(context.Background(), path)
	if err == nil {
		t.Fatal("expected count-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "count mismatch")
	}
}

// TestIndexer_CountMismatchPreservesExistingData guards Indexer.IndexFile's
// "existing data is preserved on embedding failure" doc invariant for the
// count-mismatch failure path specifically. If a future refactor moves the
// length check below replaceSourceWithHash, prior chunks would be wiped
// silently; this test catches that regression.
func TestIndexer_CountMismatchPreservesExistingData(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	twoChunks := chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{
			{ID: "a", Content: content, Source: path, StartLine: 1, EndLine: 1},
			{ID: "b", Content: content, Source: path, StartLine: 2, EndLine: 2},
		}, nil
	})

	goodEmb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		out := make([][]float64, len(inputs))
		for i := range out {
			out[i] = []float64{1, 2, 3}
		}
		return EmbedResult{Embeddings: out, Model: "m", Provider: "fake"}, nil
	})
	idx, err := NewIndexerWithEmbedder(goodEmb, store)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	idx.chunker = twoChunks

	tmp := t.TempDir()
	path := tmp + "/f.go"
	if err := writeFileImpl(path, "v1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("first IndexFile: %v", err)
	}

	pre, err := store.Search(context.Background(), []float64{1, 2, 3}, 5)
	if err != nil {
		t.Fatalf("pre-Search: %v", err)
	}
	if len(pre) != 2 {
		t.Fatalf("pre-Search: got %d results, want 2 (seed phase failed)", len(pre))
	}

	// Second pass: rewrite content, return one fewer embedding than chunks.
	if err := writeFileImpl(path, "v2"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	badEmb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		out := make([][]float64, len(inputs)-1)
		for i := range out {
			out[i] = []float64{4, 5, 6}
		}
		return EmbedResult{Embeddings: out, Model: "m", Provider: "fake"}, nil
	})
	idxBad, err := NewIndexerWithEmbedder(badEmb, store)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	idxBad.chunker = twoChunks

	if err := idxBad.IndexFile(context.Background(), path); err == nil {
		t.Fatal("expected count-mismatch error, got nil")
	} else if !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "count mismatch")
	}

	post, err := store.Search(context.Background(), []float64{1, 2, 3}, 5)
	if err != nil {
		t.Fatalf("post-Search: %v", err)
	}
	if len(post) != 2 {
		t.Errorf("post-Search: got %d results, want 2 — count-mismatch path wiped or partially overwrote prior data", len(post))
	}
}

func TestRetriever_LengthMismatchSurfacesError(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Embedder returns 0 embeddings for a 1-input batch.
	emb := EmbedderFunc(func(_ context.Context, _ string, _ []string) (EmbedResult, error) {
		return EmbedResult{Embeddings: nil, Model: "m"}, nil
	})

	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	_, err = r.Retrieve(context.Background(), "query", 5)
	if err == nil {
		t.Fatal("expected length-1 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "expected 1 embedding") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "expected 1 embedding")
	}
}

// chunkerFunc adapts a closure to the Chunker interface for tests.
type chunkerFunc func(path string, content string) ([]Chunk, error)

func (f chunkerFunc) Chunk(path string, content string) ([]Chunk, error) {
	return f(path, content)
}

func writeFileImpl(path string, content string) error {
	// 0o644 matches stdlib Go's convention for source files.
	return os.WriteFile(path, []byte(content), 0o644)
}
