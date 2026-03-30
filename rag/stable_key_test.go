package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestComputeStableKeyCodeChunk(t *testing.T) {
	chunk := Chunk{
		ID:      "abc",
		Content: "func Hello() {}",
		Source:  "/workspace/src/main.go",
		Metadata: map[string]string{
			"symbol":        "Hello",
			"symbol_path":   "Hello",
			"chunk_ordinal": "0",
		},
	}

	key, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("ComputeStableKey() error: %v", err)
	}
	if key != "src/main.go::Hello#0" {
		t.Errorf("key = %q, want %q", key, "src/main.go::Hello#0")
	}
}

func TestComputeStableKeySectionChunk(t *testing.T) {
	chunk := Chunk{
		ID:      "abc",
		Content: "## Getting Started\nSome text...",
		Source:  "/workspace/docs/README.md",
		Metadata: map[string]string{
			"section":       "Getting Started",
			"section_path":  "Getting Started",
			"chunk_ordinal": "0",
		},
	}

	key, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("ComputeStableKey() error: %v", err)
	}
	if key != "docs/README.md::Getting Started#0" {
		t.Errorf("key = %q, want %q", key, "docs/README.md::Getting Started#0")
	}
}

func TestComputeStableKeyFallbackChunk(t *testing.T) {
	chunk := Chunk{
		ID:      "abc",
		Content: "some random text",
		Source:  "/workspace/data/notes.txt",
		Metadata: map[string]string{
			"anchor_hash":   "deadbeef01234567",
			"chunk_ordinal": "2",
		},
	}

	key, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("ComputeStableKey() error: %v", err)
	}
	if key != "data/notes.txt::deadbeef01234567#2" {
		t.Errorf("key = %q, want %q", key, "data/notes.txt::deadbeef01234567#2")
	}
}

func TestComputeStableKeyEmptyWorkspaceRoot(t *testing.T) {
	chunk := Chunk{
		ID:       "abc",
		Content:  "test",
		Source:   "/workspace/test.go",
		Metadata: map[string]string{"symbol_path": "Foo", "chunk_ordinal": "0"},
	}

	_, err := ComputeStableKey(chunk, "")
	if err == nil {
		t.Fatal("expected error for empty workspaceRoot")
	}
}

func TestComputeStableKeyNoMetadata(t *testing.T) {
	chunk := Chunk{
		ID:       "abc",
		Content:  "test",
		Source:   "/workspace/test.go",
		Metadata: map[string]string{},
	}

	_, err := ComputeStableKey(chunk, "/workspace")
	if err == nil {
		t.Fatal("expected error for chunk with no stable key metadata")
	}
}

func TestComputeStableKeyDeterministic(t *testing.T) {
	chunk := Chunk{
		ID:      "abc",
		Content: "func Foo() {}",
		Source:  "/workspace/main.go",
		Metadata: map[string]string{
			"symbol_path":   "Foo",
			"chunk_ordinal": "0",
		},
	}

	key1, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	key2, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if key1 != key2 {
		t.Errorf("not deterministic: %q != %q", key1, key2)
	}
}

func TestComputeStableKeyPathNormalization(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		root    string
		wantKey string
	}{
		{
			name:    "trailing slash on root",
			source:  "/workspace/src/main.go",
			root:    "/workspace/",
			wantKey: "src/main.go::Foo#0",
		},
		{
			name:    "double slash in root",
			source:  "/workspace/src/main.go",
			root:    "/workspace//",
			wantKey: "src/main.go::Foo#0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk := Chunk{
				ID:      "abc",
				Content: "func Foo() {}",
				Source:  tt.source,
				Metadata: map[string]string{
					"symbol_path":   "Foo",
					"chunk_ordinal": "0",
				},
			}
			key, err := ComputeStableKey(chunk, tt.root)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if key != tt.wantKey {
				t.Errorf("key = %q, want %q", key, tt.wantKey)
			}
		})
	}
}

func TestComputeStableKeyDefaultOrdinal(t *testing.T) {
	// When chunk_ordinal is missing, default to "0".
	chunk := Chunk{
		ID:      "abc",
		Content: "func Bar() {}",
		Source:  "/workspace/bar.go",
		Metadata: map[string]string{
			"symbol_path": "Bar",
		},
	}

	key, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if key != "bar.go::Bar#0" {
		t.Errorf("key = %q, want %q", key, "bar.go::Bar#0")
	}
}

func TestComputeStableKeySymbolPrecedence(t *testing.T) {
	// When both symbol_path and section_path are present, symbol_path wins.
	chunk := Chunk{
		ID:      "abc",
		Content: "func Baz() {}",
		Source:  "/workspace/baz.go",
		Metadata: map[string]string{
			"symbol_path":   "Baz",
			"section_path":  "Section",
			"anchor_hash":   "deadbeef",
			"chunk_ordinal": "0",
		},
	}

	key, err := ComputeStableKey(chunk, "/workspace")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.HasPrefix(key, "baz.go::Baz#") {
		t.Errorf("expected symbol_path to take precedence, got key = %q", key)
	}
}

func TestLineShiftStability(t *testing.T) {
	// Adding blank lines before a function should not change its StableKey.
	// The key depends on the symbol name, not the line number.
	root := "/workspace"

	chunkBefore := Chunk{
		ID:        "v1",
		Content:   "func Stable() { return nil }",
		Source:    filepath.Join(root, "file.go"),
		StartLine: 10,
		EndLine:   12,
		Metadata: map[string]string{
			"symbol":        "Stable",
			"symbol_path":   "Stable",
			"chunk_ordinal": "0",
		},
	}

	chunkAfter := Chunk{
		ID:        "v2",
		Content:   "func Stable() { return nil }",
		Source:    filepath.Join(root, "file.go"),
		StartLine: 15, // shifted 5 lines
		EndLine:   17,
		Metadata: map[string]string{
			"symbol":        "Stable",
			"symbol_path":   "Stable",
			"chunk_ordinal": "0",
		},
	}

	keyBefore, err := ComputeStableKey(chunkBefore, root)
	if err != nil {
		t.Fatalf("before error: %v", err)
	}
	keyAfter, err := ComputeStableKey(chunkAfter, root)
	if err != nil {
		t.Fatalf("after error: %v", err)
	}
	if keyBefore != keyAfter {
		t.Errorf("StableKey changed after line shift: %q != %q", keyBefore, keyAfter)
	}
}

func TestExtractSymbolName(t *testing.T) {
	tests := []struct {
		name    string
		content string
		lang    string
		want    string
	}{
		{"go func", "func Hello() {}", "go", "Hello"},
		{"go method", "func (s *Server) Start() error {}", "go", "Start"},
		{"python def", "def process_data():", "python", "process_data"},
		{"python class", "class MyModel:", "python", "MyModel"},
		{"ts function", "export function render() {}", "typescript", "render"},
		{"ts class", "class Widget {}", "typescript", "Widget"},
		{"ts const", "const config = {}", "typescript", "config"},
		{"ts interface", "interface Props {}", "typescript", "Props"},
		{"js function", "function init() {}", "javascript", "init"},
		{"rust fn", "fn calculate() {}", "rust", "calculate"},
		{"rust pub fn", "pub fn serve() {}", "rust", "serve"},
		{"rust struct", "struct Config {}", "rust", "Config"},
		{"ruby def", "def process\nend", "ruby", "process"},
		{"ruby class", "class Worker\nend", "ruby", "Worker"},
		{"unknown lang", "func Hello() {}", "unknown", ""},
		{"no match", "// just a comment", "go", ""},
		{"multiline finds first", "// comment\nfunc First() {}\nfunc Second() {}", "go", "First"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSymbolName(tt.content, tt.lang)
			if got != tt.want {
				t.Errorf("extractSymbolName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAnchorHashDeterministic(t *testing.T) {
	h1 := anchorHash("hello world\nfoo bar")
	h2 := anchorHash("hello world\nfoo bar")
	if h1 != h2 {
		t.Errorf("anchorHash not deterministic: %q != %q", h1, h2)
	}
}

func TestAnchorHashWhitespaceResilience(t *testing.T) {
	// Same content with extra leading/trailing whitespace per line.
	h1 := anchorHash("hello\n  world\n")
	h2 := anchorHash("  hello  \n  world  \n")
	if h1 != h2 {
		t.Errorf("anchorHash changed with whitespace: %q != %q", h1, h2)
	}
}

func TestAnchorHashBlankLineCollapse(t *testing.T) {
	h1 := anchorHash("a\n\nb")
	h2 := anchorHash("a\n\n\n\nb")
	if h1 != h2 {
		t.Errorf("anchorHash changed with extra blank lines: %q != %q", h1, h2)
	}
}

func TestAnchorHashDifferentContent(t *testing.T) {
	h1 := anchorHash("hello world")
	h2 := anchorHash("goodbye world")
	if h1 == h2 {
		t.Error("anchorHash should differ for different content")
	}
}

func TestPopulateCodeChunkMetadata(t *testing.T) {
	chunks := []Chunk{
		{Content: "func Alpha() {}", Metadata: make(map[string]string)},
		{Content: "func Beta() {}", Metadata: make(map[string]string)},
		{Content: "// just a comment", Metadata: make(map[string]string)},
		{Content: "func Alpha() { return 2 }", Metadata: make(map[string]string)}, // duplicate symbol
	}

	populateCodeChunkMetadata(chunks, "go")

	// Alpha, Beta, unknown, Alpha
	expected := []struct {
		symbol  string
		ordinal string
	}{
		{"Alpha", "0"},
		{"Beta", "0"},
		{"unknown", "0"},
		{"Alpha", "1"}, // second Alpha gets ordinal 1
	}

	for i, exp := range expected {
		if chunks[i].Metadata["symbol"] != exp.symbol {
			t.Errorf("chunk %d symbol = %q, want %q", i, chunks[i].Metadata["symbol"], exp.symbol)
		}
		if chunks[i].Metadata["symbol_path"] != exp.symbol {
			t.Errorf("chunk %d symbol_path = %q, want %q", i, chunks[i].Metadata["symbol_path"], exp.symbol)
		}
		if chunks[i].Metadata["chunk_ordinal"] != exp.ordinal {
			t.Errorf("chunk %d chunk_ordinal = %q, want %q", i, chunks[i].Metadata["chunk_ordinal"], exp.ordinal)
		}
	}
}

func TestPopulateSlidingWindowMetadata(t *testing.T) {
	chunks := []Chunk{
		{Content: "first chunk content", Metadata: make(map[string]string)},
		{Content: "second chunk content", Metadata: make(map[string]string)},
		{Content: "third chunk content", Metadata: make(map[string]string)},
	}

	populateSlidingWindowMetadata(chunks)

	for i, chunk := range chunks {
		if chunk.Metadata["anchor_hash"] == "" {
			t.Errorf("chunk %d missing anchor_hash", i)
		}
		expected := fmt.Sprintf("%d", i)
		if chunk.Metadata["chunk_ordinal"] != expected {
			t.Errorf("chunk %d chunk_ordinal = %q, want %q", i, chunk.Metadata["chunk_ordinal"], expected)
		}
	}

	// Anchor hashes should differ for different content.
	if chunks[0].Metadata["anchor_hash"] == chunks[1].Metadata["anchor_hash"] {
		t.Error("different chunks should have different anchor hashes")
	}
}

func TestCodeChunkerPopulatesMetadata(t *testing.T) {
	content := "func Alpha() {\n\treturn 1\n}\n\nfunc Beta() {\n\treturn 2\n}\n"
	chunker := NewCodeChunker(WithMaxChunkSize(500))
	chunks, err := chunker.Chunk("/workspace/funcs.go", content)
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	for i, chunk := range chunks {
		if chunk.Metadata["symbol"] == "" {
			t.Errorf("chunk %d missing symbol metadata", i)
		}
		if chunk.Metadata["symbol_path"] == "" {
			t.Errorf("chunk %d missing symbol_path metadata", i)
		}
		if chunk.Metadata["chunk_ordinal"] == "" {
			t.Errorf("chunk %d missing chunk_ordinal metadata", i)
		}
	}
}

func TestSlidingWindowChunkerPopulatesMetadata(t *testing.T) {
	content := strings.Repeat("line of text\n", 20)
	sw, err := NewSlidingWindowChunker(100, 20)
	if err != nil {
		t.Fatalf("NewSlidingWindowChunker() error: %v", err)
	}

	chunks, err := sw.Chunk("test.txt", content)
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}

	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	for i, chunk := range chunks {
		if chunk.Metadata["anchor_hash"] == "" {
			t.Errorf("chunk %d missing anchor_hash metadata", i)
		}
		if chunk.Metadata["chunk_ordinal"] == "" {
			t.Errorf("chunk %d missing chunk_ordinal metadata", i)
		}
	}
}

func TestStableKeyStoreRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID:        "c1",
			Content:   "func Hello() {}",
			Source:    "/workspace/main.go",
			StartLine: 1,
			EndLine:   1,
			Language:  "go",
			Metadata:  map[string]string{"symbol_path": "Hello", "chunk_ordinal": "0"},
			StableKey: "main.go::Hello#0",
		},
		{
			ID:        "c2",
			Content:   "func World() {}",
			Source:    "/workspace/main.go",
			StartLine: 3,
			EndLine:   3,
			Language:  "go",
			Metadata:  map[string]string{"symbol_path": "World", "chunk_ordinal": "0"},
			StableKey: "main.go::World#0",
		},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Verify StableKey survives Search round-trip.
	results, err := store.Search(ctx, []float64{1.0, 0.0, 0.0}, 10)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}

	keyMap := make(map[string]string)
	for _, r := range results {
		keyMap[r.Chunk.ID] = r.Chunk.StableKey
	}

	if keyMap["c1"] != "main.go::Hello#0" {
		t.Errorf("c1 StableKey = %q, want %q", keyMap["c1"], "main.go::Hello#0")
	}
	if keyMap["c2"] != "main.go::World#0" {
		t.Errorf("c2 StableKey = %q, want %q", keyMap["c2"], "main.go::World#0")
	}
}

func TestStableKeySearchMultiRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID:        "c1",
			Content:   "func Alpha() {}",
			Source:    "/workspace/alpha.go",
			StartLine: 1,
			EndLine:   1,
			Language:  "go",
			Metadata:  map[string]string{"symbol_path": "Alpha", "chunk_ordinal": "0"},
			StableKey: "alpha.go::Alpha#0",
		},
	}
	embeddings := [][]float64{{1.0, 0.0, 0.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// SearchMulti should also return StableKey.
	results, err := store.SearchMulti(ctx, []float64{1.0, 0.0, 0.0}, "alpha", 10, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Chunk.StableKey != "alpha.go::Alpha#0" {
		t.Errorf("SearchMulti StableKey = %q, want %q", results[0].Chunk.StableKey, "alpha.go::Alpha#0")
	}
}

func TestStableKeyExportRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID:        "c1",
			Content:   "func Export() {}",
			Source:    "/workspace/export.go",
			StartLine: 1,
			EndLine:   1,
			Language:  "go",
			Metadata:  map[string]string{"symbol_path": "Export", "chunk_ordinal": "0"},
			StableKey: "export.go::Export#0",
		},
	}
	embeddings := [][]float64{{1.0, 0.0, 0.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var exported []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		exported = append(exported, ec)
	}

	if len(exported) != 1 {
		t.Fatalf("expected 1 exported chunk, got %d", len(exported))
	}
	if exported[0].Chunk.StableKey != "export.go::Export#0" {
		t.Errorf("exported StableKey = %q, want %q", exported[0].Chunk.StableKey, "export.go::Export#0")
	}
}

func TestStableKeyReplaceSourcePreservesKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID:        "c1",
			Content:   "func Replace() {}",
			Source:    "replace.go",
			StartLine: 1,
			EndLine:   1,
			Language:  "go",
			Metadata:  map[string]string{},
			StableKey: "replace.go::Replace#0",
		},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1.0, 0.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Replace with updated content but same StableKey.
	replacement := []Chunk{
		{
			ID:        "c1-v2",
			Content:   "func Replace() { return 42 }",
			Source:    "replace.go",
			StartLine: 1,
			EndLine:   1,
			Language:  "go",
			Metadata:  map[string]string{},
			StableKey: "replace.go::Replace#0",
		},
	}
	if err := store.ReplaceSource(ctx, "replace.go", replacement, [][]float64{{0.5, 0.5}}); err != nil {
		t.Fatalf("ReplaceSource() error: %v", err)
	}

	results, err := store.Search(ctx, []float64{0.5, 0.5}, 1)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Chunk.StableKey != "replace.go::Replace#0" {
		t.Errorf("StableKey = %q, want %q", results[0].Chunk.StableKey, "replace.go::Replace#0")
	}
}

func TestIndexerStableKeyWithWorkspaceRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		emb := make([]float64, 4)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer store.Close()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn\n}\n"), 0644)

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	results, err := store.Search(context.Background(), []float64{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	// At least one chunk should have a StableKey.
	hasKey := false
	for _, r := range results {
		if r.Chunk.StableKey != "" {
			hasKey = true
			// Should start with "main.go::"
			if !strings.HasPrefix(r.Chunk.StableKey, "main.go::") {
				t.Errorf("StableKey = %q, expected prefix 'main.go::'", r.Chunk.StableKey)
			}
		}
	}
	if !hasKey {
		t.Error("no chunks have StableKey set after indexing with workspace root")
	}
}

func TestIndexerStableKeyWithoutWorkspaceRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		emb := make([]float64, 4)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer store.Close()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn\n}\n"), 0644)

	// No WithWorkspaceRoot -- StableKey should be empty.
	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	results, err := store.Search(context.Background(), []float64{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}

	for _, r := range results {
		if r.Chunk.StableKey != "" {
			t.Errorf("expected empty StableKey without workspace root, got %q", r.Chunk.StableKey)
		}
	}
}

func TestIndexDirectoryStableKeyConsistency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		emb := make([]float64, 4)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer store.Close()

	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "pkg")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "util.go"), []byte("package pkg\n\nfunc Helper() {\n\treturn\n}\n"), 0644)

	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	if err := idx.IndexDirectory(context.Background(), tmpDir); err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	results, err := store.Search(context.Background(), []float64{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}

	// All chunks should have StableKeys with "pkg/util.go" prefix.
	for _, r := range results {
		if r.Chunk.StableKey == "" {
			t.Error("chunk has empty StableKey after IndexDirectory")
			continue
		}
		if !strings.HasPrefix(r.Chunk.StableKey, "pkg/util.go::") {
			t.Errorf("StableKey = %q, expected prefix 'pkg/util.go::'", r.Chunk.StableKey)
		}
	}
}

func TestIndexFileAndIndexDirectoryProduceSameKey(t *testing.T) {
	// IndexFile with explicit workspace root should produce the same key
	// as IndexDirectory using that directory as root.
	content := "package main\n\nfunc Consistent() {\n\treturn\n}\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		emb := make([]float64, 4)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "consistent.go")
	os.WriteFile(goFile, []byte(content), 0644)

	// Method 1: IndexFile with explicit workspace root.
	store1, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("store1 error: %v", err)
	}
	defer store1.Close()

	idx1 := NewIndexer(client, store1,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)
	if err := idx1.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	// Method 2: IndexDirectory with the same directory.
	store2, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("store2 error: %v", err)
	}
	defer store2.Close()

	idx2 := NewIndexer(client, store2, WithEmbeddingModel("test-embed"))
	if err := idx2.IndexDirectory(context.Background(), tmpDir); err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	results1, _ := store1.Search(context.Background(), []float64{1, 0, 0, 0}, 10)
	results2, _ := store2.Search(context.Background(), []float64{1, 0, 0, 0}, 10)

	if len(results1) == 0 || len(results2) == 0 {
		t.Fatal("expected results from both methods")
	}

	// Collect stable keys from each.
	keys1 := make(map[string]bool)
	for _, r := range results1 {
		if r.Chunk.StableKey != "" {
			keys1[r.Chunk.StableKey] = true
		}
	}
	keys2 := make(map[string]bool)
	for _, r := range results2 {
		if r.Chunk.StableKey != "" {
			keys2[r.Chunk.StableKey] = true
		}
	}

	// At least one key should match between the two methods.
	matched := false
	for k := range keys1 {
		if keys2[k] {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("no matching StableKeys between IndexFile and IndexDirectory\nIndexFile keys: %v\nIndexDirectory keys: %v", keys1, keys2)
	}
}

func TestMigrationV3StableKeyColumn(t *testing.T) {
	store := newTestStore(t)

	// Verify stable_key column exists.
	_, err := store.db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key)
		 VALUES ('test', 'content', 'src.go', 1, 1, 'go', '{}', '[]', 12345, 'src.go::Test#0')`,
	)
	if err != nil {
		t.Fatalf("insert with stable_key: %v", err)
	}

	var stableKey string
	err = store.db.QueryRow(`SELECT stable_key FROM chunks WHERE id = 'test'`).Scan(&stableKey)
	if err != nil {
		t.Fatalf("query stable_key: %v", err)
	}
	if stableKey != "src.go::Test#0" {
		t.Errorf("stable_key = %q, want %q", stableKey, "src.go::Test#0")
	}

	// Verify the index exists.
	var indexCount int
	err = store.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_chunks_stable_key'`,
	).Scan(&indexCount)
	if err != nil {
		t.Fatalf("query index: %v", err)
	}
	if indexCount != 1 {
		t.Error("idx_chunks_stable_key index does not exist")
	}
}
