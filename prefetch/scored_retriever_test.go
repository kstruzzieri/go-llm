package prefetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/rag"
)

// mockVectorStore implements rag.VectorStore for testing.
type mockVectorStore struct {
	searchResults []rag.SearchResult
	searchErr     error
	storeCalled   bool
	deleteCalled  string
}

func (m *mockVectorStore) Store(_ context.Context, _ []rag.Chunk, _ [][]float64) error {
	m.storeCalled = true
	return nil
}

func (m *mockVectorStore) Search(_ context.Context, _ []float64, k int) ([]rag.SearchResult, error) {
	if m.searchErr != nil {
		return nil, m.searchErr
	}
	results := m.searchResults
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results, nil
}

func (m *mockVectorStore) DeleteBySource(_ context.Context, source string) error {
	m.deleteCalled = source
	return nil
}

func (m *mockVectorStore) Stats(_ context.Context) (rag.StoreStats, error) {
	return rag.StoreStats{}, nil
}

func (m *mockVectorStore) Close() error {
	return nil
}

// mockMultiSignalStore implements both VectorStore and MultiSignalSearcher.
type mockMultiSignalStore struct {
	mockVectorStore
	multiResults []rag.ScoredResult
	multiErr     error
}

func (m *mockMultiSignalStore) SearchMulti(_ context.Context, _ []float64, _ string,
	k int, _ rag.QueryContext) ([]rag.ScoredResult, error) {
	if m.multiErr != nil {
		return nil, m.multiErr
	}
	results := m.multiResults
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results, nil
}

// newMockEmbedServer creates an httptest server that responds to /api/embed
// with a fixed embedding vector.
func newMockEmbedServer(embedding []float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resp := ollama.EmbedResponse{
			Embeddings: [][]float64{embedding},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestScoredRetriever_PlainSearch(t *testing.T) {
	embedding := []float64{0.1, 0.2, 0.3}
	server := newMockEmbedServer(embedding)
	defer server.Close()

	client := ollama.NewClient(ollama.WithBaseURL(server.URL))

	store := &mockVectorStore{
		searchResults: []rag.SearchResult{
			{
				Chunk: rag.Chunk{
					ID:      "chunk-1",
					Content: "func main() {}",
					Source:  "main.go",
				},
				Score:    0.95,
				Distance: 0.05,
			},
			{
				Chunk: rag.Chunk{
					ID:      "chunk-2",
					Content: "func helper() {}",
					Source:  "helper.go",
				},
				Score:    0.80,
				Distance: 0.20,
			},
		},
	}

	retriever := NewScoredRetriever(client, store, "test-model")

	result, err := retriever.Retrieve(context.Background(), "main function", 5,
		rag.QueryContext{CurrentFile: "main.go"}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.CacheHit {
		t.Error("expected CacheHit=false for ScoredRetriever")
	}
	if len(result.Chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(result.Chunks))
	}

	// Verify semantic-only signal wrapping.
	for i, chunk := range result.Chunks {
		if _, ok := chunk.Signals["semantic"]; !ok {
			t.Errorf("chunk %d missing 'semantic' signal", i)
		}
		if chunk.Signals["semantic"] != chunk.Score {
			t.Errorf("chunk %d: semantic signal %v != score %v", i, chunk.Signals["semantic"], chunk.Score)
		}
	}
}

func TestScoredRetriever_MultiSignalSearch(t *testing.T) {
	embedding := []float64{0.1, 0.2, 0.3}
	server := newMockEmbedServer(embedding)
	defer server.Close()

	client := ollama.NewClient(ollama.WithBaseURL(server.URL))

	store := &mockMultiSignalStore{
		multiResults: []rag.ScoredResult{
			{
				SearchResult: rag.SearchResult{
					Chunk: rag.Chunk{
						ID:      "chunk-1",
						Content: "func main() {}",
						Source:  "main.go",
					},
					Score:    0.92,
					Distance: 0.08,
				},
				Signals: map[string]float64{
					"semantic": 0.90,
					"keyword":  0.95,
				},
			},
		},
	}

	retriever := NewScoredRetriever(client, store, "test-model")

	result, err := retriever.Retrieve(context.Background(), "main function", 5,
		rag.QueryContext{CurrentFile: "main.go"}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.CacheHit {
		t.Error("expected CacheHit=false for ScoredRetriever")
	}
	if len(result.Chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(result.Chunks))
	}
	if result.Chunks[0].Signals["keyword"] != 0.95 {
		t.Errorf("expected keyword signal 0.95, got %v", result.Chunks[0].Signals["keyword"])
	}
}

func TestScoredRetriever_EmbedError(t *testing.T) {
	// Server that returns an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
	}))
	defer server.Close()

	client := ollama.NewClient(ollama.WithBaseURL(server.URL))
	store := &mockVectorStore{}

	retriever := NewScoredRetriever(client, store, "nonexistent-model")

	_, err := retriever.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err == nil {
		t.Fatal("expected error from embed, got nil")
	}
}

func TestScoredRetriever_ContextCancellation(t *testing.T) {
	// Server that delays long enough for context to be cancelled.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()

	client := ollama.NewClient(ollama.WithBaseURL(server.URL))
	store := &mockVectorStore{}

	retriever := NewScoredRetriever(client, store, "test-model")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := retriever.Retrieve(ctx, "query", 5, rag.QueryContext{}, RetrieveOptions{})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestScoredRetriever_TopK(t *testing.T) {
	embedding := []float64{0.1, 0.2, 0.3}
	server := newMockEmbedServer(embedding)
	defer server.Close()

	client := ollama.NewClient(ollama.WithBaseURL(server.URL))

	results := make([]rag.SearchResult, 10)
	for i := range results {
		results[i] = rag.SearchResult{
			Chunk: rag.Chunk{ID: "chunk-" + string(rune('0'+i))},
			Score: float64(10-i) / 10.0,
		}
	}

	store := &mockVectorStore{searchResults: results}
	retriever := NewScoredRetriever(client, store, "test-model")

	result, err := retriever.Retrieve(context.Background(), "query", 3,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Chunks) != 3 {
		t.Errorf("expected 3 chunks after top-k, got %d", len(result.Chunks))
	}
}
