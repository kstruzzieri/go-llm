package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var req EmbedRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "nomic-embed-text" {
			t.Errorf("expected model %q, got %q", "nomic-embed-text", req.Model)
		}

		resp := EmbedResponse{
			Embeddings: [][]float64{{0.1, 0.2, 0.3, 0.4}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	emb, err := c.Embed(context.Background(), "nomic-embed-text", "hello world")
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(emb) != 4 {
		t.Fatalf("expected 4 dimensions, got %d", len(emb))
	}
	if emb[0] != 0.1 {
		t.Errorf("expected first dim 0.1, got %f", emb[0])
	}
}

func TestEmbedEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := EmbedResponse{Embeddings: [][]float64{}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.Embed(context.Background(), "nomic-embed-text", "hello")
	if err == nil {
		t.Fatal("expected error for empty embeddings response")
	}
}

func TestEmbedBatch(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := EmbedResponse{
			Embeddings: [][]float64{{float64(callCount), 0.0, 0.0}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	results, err := c.EmbedBatch(context.Background(), "nomic-embed-text", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch() error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0][0] != 1.0 {
		t.Errorf("expected first embedding[0]=1.0, got %f", results[0][0])
	}
	if results[2][0] != 3.0 {
		t.Errorf("expected third embedding[0]=3.0, got %f", results[2][0])
	}
}
