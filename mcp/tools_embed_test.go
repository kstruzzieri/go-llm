package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestEmbedToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; request-shape coverage now lives " +
		"in TestHandleEmbed_UsesRouter via the routeEngine seam.")
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
	t.Skip("end-to-end Ollama-traffic test; request-shape coverage now lives " +
		"in TestHandleEmbedBatch_UsesRouter via the routeEngine seam.")
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

func TestHandleEmbed_UsesRouter(t *testing.T) {
	router := newRecordingRouteEngine("")
	router.embedVectors = [][]float64{{0.1, 0.2, 0.3}}
	s := &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"embedding": "embedder"},
			Models: map[string]config.ModelConfig{
				"embedder": {Name: "qwen3-embedding:8b", Provider: "ollama"},
			},
		},
		router: router,
	}

	args, _ := json.Marshal(embedArgs{Text: "hello world"})
	res, err := s.handleEmbed(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleEmbed: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleEmbed returned error: %s", extractText(res))
	}
	if !router.called {
		t.Fatal("router was not called")
	}
	if want := []string{"hello world"}; !reflect.DeepEqual(router.last.Input, want) {
		t.Errorf("Input = %v, want %v", router.last.Input, want)
	}
	if router.last.RequiredCaps != provider.CapEmbed {
		t.Errorf("RequiredCaps = %v, want CapEmbed", router.last.RequiredCaps)
	}
	if want := []string{"ollama/qwen3-embedding:8b"}; !reflect.DeepEqual(router.last.PreferredChain, want) {
		t.Errorf("PreferredChain = %v, want %v", router.last.PreferredChain, want)
	}
}

func TestHandleEmbedBatch_UsesRouter(t *testing.T) {
	router := newRecordingRouteEngine("")
	router.embedVectors = [][]float64{{0.1}, {0.2}, {0.3}}
	s := &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"embedding": "embedder"},
			Models: map[string]config.ModelConfig{
				"embedder": {Name: "qwen3-embedding:8b", Provider: "ollama"},
			},
		},
		router: router,
	}

	args, _ := json.Marshal(embedBatchArgs{Texts: []string{"a", "b", "c"}})
	_, err := s.handleEmbedBatch(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleEmbedBatch: %v", err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(router.last.Input, want) {
		t.Errorf("Input = %v, want %v", router.last.Input, want)
	}
	if router.last.UseCase != "embedding" {
		t.Errorf("UseCase = %q, want embedding", router.last.UseCase)
	}
}

func TestHandleEmbedBatch_BatchSizeValidatedBeforeRouting(t *testing.T) {
	router := newRecordingRouteEngine("")
	s := &Server{router: router}

	texts := make([]string, maxBatchSize+1)
	for i := range texts {
		texts[i] = "x"
	}
	args, _ := json.Marshal(embedBatchArgs{Texts: texts, Model: "ollama/m:8b"})
	res, err := s.handleEmbedBatch(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleEmbedBatch: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected validation error for batch over limit")
	}
	if router.called {
		t.Error("router was called despite batch-size validation failure")
	}
}
