package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestEmbedToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","embeddings":[[0.1,0.2,0.3]]}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "embed",
		Arguments: map[string]any{
			"model": "m",
			"text":  "hello world",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var vec []float64
	if err := json.Unmarshal([]byte(text), &vec); err != nil {
		t.Fatalf("unmarshal embedding: %v (text = %q)", err, text)
	}
	if len(vec) != 3 {
		t.Errorf("embedding length = %d, want 3", len(vec))
	}
}

func TestEmbedToolEmptyText(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "embed",
		Arguments: map[string]any{
			"model": "m",
			"text":  "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty text")
	}
	if text := extractText(result); !strings.Contains(text, "text must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "text must not be empty")
	}
}

func TestEmbedBatchToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","embeddings":[[0.1,0.2],[0.3,0.4],[0.5,0.6]]}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "embed_batch",
		Arguments: map[string]any{
			"model": "m",
			"texts": []string{"one", "two", "three"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var vecs [][]float64
	if err := json.Unmarshal([]byte(text), &vecs); err != nil {
		t.Fatalf("unmarshal embeddings: %v (text = %q)", err, text)
	}
	if len(vecs) != 3 {
		t.Errorf("embeddings count = %d, want 3", len(vecs))
	}
}

func TestEmbedBatchToolExceedsLimit(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	// Build 101 texts to exceed the limit.
	texts := make([]string, maxBatchSize+1)
	for i := range texts {
		texts[i] = "text"
	}

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "embed_batch",
		Arguments: map[string]any{
			"model": "m",
			"texts": texts,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when batch exceeds limit")
	}
	text := extractText(result)
	if !strings.Contains(text, "maximum batch size") {
		t.Errorf("error = %q, want to contain %q", text, "maximum batch size")
	}
}

func TestEmbedBatchToolEmptyTexts(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "embed_batch",
		Arguments: map[string]any{
			"model": "m",
			"texts": []string{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty texts")
	}
	if text := extractText(result); !strings.Contains(text, "texts must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "texts must not be empty")
	}
}
