package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestRetrieverRetrieve(t *testing.T) {
	// Mock embed server returns a fixed vector
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.EmbedResponse{Embeddings: [][]float64{{1.0, 0.0, 0.0}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	// Pre-populate store
	chunks := []Chunk{
		{ID: "c1", Content: "relevant content", Source: "a.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c2", Content: "irrelevant content", Source: "b.go", StartLine: 1, EndLine: 3, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{0.9, 0.1, 0.0}, // similar to query
		{0.0, 0.0, 1.0}, // orthogonal to query
	}
	_ = store.Store(context.Background(), chunks, embeddings)

	retriever := NewRetriever(client, store)
	results, err := retriever.Retrieve(context.Background(), "test query", 2)
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// First result should be the more similar one
	if results[0].Chunk.ID != "c1" {
		t.Errorf("top result ID = %q, want %q", results[0].Chunk.ID, "c1")
	}
}

func TestRetrieverBuildContext(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{
			Chunk: Chunk{
				Source: "main.go", StartLine: 1, EndLine: 10,
				Content: "func main() { fmt.Println(\"hello\") }",
			},
			Score: 0.95,
		},
		{
			Chunk: Chunk{
				Source: "util.go", StartLine: 5, EndLine: 15,
				Content: "func helper() string { return \"help\" }",
			},
			Score: 0.80,
		},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "main.go") {
		t.Error("context should contain source file name")
	}
	if !strings.Contains(ctx, "0.95") {
		t.Error("context should contain similarity score")
	}
	if !strings.Contains(ctx, "func main()") {
		t.Error("context should contain code content")
	}
}

func TestRetrieverBuildContextEmpty(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)
	ctx := retriever.BuildContext(nil, 1000)
	if ctx != "" {
		t.Errorf("expected empty context, got %q", ctx)
	}
}

func TestRetrieverBuildContextTruncation(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{
			Chunk: Chunk{Source: "a.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("x", 500)},
			Score: 0.9,
		},
		{
			Chunk: Chunk{Source: "b.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("y", 500)},
			Score: 0.8,
		},
	}

	// Very small maxTokens should truncate
	ctx := retriever.BuildContext(results, 50) // 50 tokens ~= 200 chars
	if strings.Contains(ctx, "b.go") {
		t.Error("second result should be truncated due to token limit")
	}
}

func TestRetrieverBuildContextLineNumbersSingleLine(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "a.go", StartLine: 42, EndLine: 42, Content: "x := compute()"}, Score: 0.9},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "42| x := compute()") {
		t.Errorf("context missing line-anchored content; got:\n%s", ctx)
	}
	// Attribution (source, line range, score) is still present.
	if !strings.Contains(ctx, "a.go") || !strings.Contains(ctx, "lines 42-42") || !strings.Contains(ctx, "0.90") {
		t.Errorf("context missing attribution; got:\n%s", ctx)
	}
}

func TestRetrieverBuildContextLineNumbersMultiLine(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "m.go", StartLine: 10, EndLine: 12, Content: "func f() {\n\treturn 1\n}"}, Score: 0.8},
	}

	ctx := retriever.BuildContext(results, 1000)
	for _, want := range []string{"10| func f() {", "11| \treturn 1", "12| }"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q; got:\n%s", want, ctx)
		}
	}
}

func TestRetrieverBuildContextLineNumbersTrailingNewline(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	// Content ending in a newline must not produce a phantom numbered line.
	results := []SearchResult{
		{Chunk: Chunk{Source: "t.go", StartLine: 5, EndLine: 6, Content: "a\nb\n"}, Score: 0.7},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "5| a") || !strings.Contains(ctx, "6| b") {
		t.Errorf("context missing numbered lines; got:\n%s", ctx)
	}
	if strings.Contains(ctx, "7| ") {
		t.Errorf("context numbered a phantom trailing line; got:\n%s", ctx)
	}
}

func TestRetrieverBuildContextLineNumbersTruncation(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "first.go", StartLine: 1, EndLine: 1, Content: "small"}, Score: 0.9},
		{Chunk: Chunk{Source: "second.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("y", 500)}, Score: 0.8},
	}

	// Budget fits the first numbered chunk but not the large second one.
	ctx := retriever.BuildContext(results, 40) // ~160 chars
	if !strings.Contains(ctx, "1| small") {
		t.Errorf("first chunk should be line-numbered and present; got:\n%s", ctx)
	}
	if strings.Contains(ctx, "second.go") {
		t.Error("second result should be truncated due to token limit")
	}
}

func TestRetrieverWithCustomModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.EmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "custom-embed" {
			t.Errorf("expected model %q, got %q", "custom-embed", req.Model)
		}
		resp := ollama.EmbedResponse{Embeddings: [][]float64{{1.0, 0.0}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	_ = store.Store(context.Background(),
		[]Chunk{{ID: "c1", Content: "test", Source: "a.go", Metadata: map[string]string{}}},
		[][]float64{{1.0, 0.0}},
	)

	retriever := NewRetriever(client, store, WithRetrieverModel("custom-embed"))
	results, err := retriever.Retrieve(context.Background(), "query", 1)
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}
