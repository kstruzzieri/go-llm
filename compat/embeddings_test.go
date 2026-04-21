package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestEmbeddings_DisabledByDefault(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapEmbed})
	defer teardown()
	raw, _ := json.Marshal(EmbeddingRequest{Model: "qwen3-embedding:8b", Input: EmbeddingInput{Values: []string{"hi"}}})
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(raw)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (route not registered)", rec.Code)
	}
}

func TestEmbeddings_EnabledReturnsVectors(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapEmbed,
		models: []provider.ModelInfo{{Name: "qwen3-embedding:8b", ContextWindow: 32768, Capabilities: []string{"embedding"}}},
		embedFn: func(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
			return &provider.EmbedResponse{
				Model:      req.Model,
				Embeddings: [][]float64{{0.1, 0.2}},
				Usage:      provider.Usage{PromptTokens: 1, TotalTokens: 1},
			}, nil
		},
	}
	srv, teardown := newTestServer(t, mp, WithEmbeddings(true))
	defer teardown()

	raw, _ := json.Marshal(EmbeddingRequest{Model: "qwen3-embedding:8b", Input: EmbeddingInput{Values: []string{"hi"}}})
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp EmbeddingResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 {
		t.Errorf("bad data: %+v", resp.Data)
	}
}

func TestEmbeddings_EnabledAcceptsSingleStringInput(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapEmbed,
		models: []provider.ModelInfo{{Name: "qwen3-embedding:8b", ContextWindow: 32768, Capabilities: []string{"embedding"}}},
		embedFn: func(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
			if len(req.Input) != 1 || req.Input[0] != "hi" {
				t.Errorf("expected Input=[\"hi\"], got %v", req.Input)
			}
			return &provider.EmbedResponse{
				Model:      req.Model,
				Embeddings: [][]float64{{0.3, 0.4}},
				Usage:      provider.Usage{PromptTokens: 1, TotalTokens: 1},
			}, nil
		},
	}
	srv, teardown := newTestServer(t, mp, WithEmbeddings(true))
	defer teardown()

	// Marshal the raw OpenAI shape directly: input as a single string.
	raw := []byte(`{"model":"qwen3-embedding:8b","input":"hi"}`)
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp EmbeddingResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 || resp.Data[0].Embedding[0] != 0.3 {
		t.Errorf("bad data: %+v", resp.Data)
	}
}
