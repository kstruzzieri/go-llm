package rag

import (
	"context"
	"testing"
)

func TestContentHash(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int // SHA-256 hex = 64 chars
	}{
		{"non-empty string", "hello world", 64},
		{"empty string", "", 64},
		{"unicode content", "func main() { fmt.Println(\"こんにちは\") }", 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := contentHash(tt.input)
			if len(got) != tt.wantLen {
				t.Errorf("contentHash(%q) length = %d, want %d", tt.input, len(got), tt.wantLen)
			}
		})
	}

	t.Run("deterministic", func(t *testing.T) {
		a := contentHash("some content")
		b := contentHash("some content")
		if a != b {
			t.Errorf("contentHash is not deterministic: %q != %q", a, b)
		}
	})

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		a := contentHash("hello")
		b := contentHash("world")
		if a == b {
			t.Errorf("different inputs produced same hash: %q", a)
		}
	})

	t.Run("empty string has a hash", func(t *testing.T) {
		got := contentHash("")
		if got == "" {
			t.Error("contentHash(\"\") returned empty string")
		}
		// SHA-256 of empty string is well-known.
		want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		if got != want {
			t.Errorf("contentHash(\"\") = %q, want %q", got, want)
		}
	})
}

func TestChunkWithEmbedding(t *testing.T) {
	chunk := Chunk{
		ID:        "test-id",
		Content:   "func main() {}",
		Source:    "main.go",
		StartLine: 1,
		EndLine:   3,
		Language:  "go",
		Metadata:  map[string]string{"key": "value"},
	}
	embedding := []float64{0.1, 0.2, 0.3}

	cwe := ChunkWithEmbedding{
		Chunk:     chunk,
		Embedding: embedding,
	}

	if cwe.Chunk.ID != "test-id" {
		t.Errorf("Chunk.ID = %q, want %q", cwe.Chunk.ID, "test-id")
	}
	if cwe.Chunk.Content != "func main() {}" {
		t.Errorf("Chunk.Content = %q, want %q", cwe.Chunk.Content, "func main() {}")
	}
	if cwe.Chunk.Source != "main.go" {
		t.Errorf("Chunk.Source = %q, want %q", cwe.Chunk.Source, "main.go")
	}
	if len(cwe.Embedding) != 3 {
		t.Errorf("Embedding length = %d, want 3", len(cwe.Embedding))
	}
	if cwe.Embedding[0] != 0.1 || cwe.Embedding[1] != 0.2 || cwe.Embedding[2] != 0.3 {
		t.Errorf("Embedding = %v, want [0.1 0.2 0.3]", cwe.Embedding)
	}
}

// TestSourceChunkLoaderInterface verifies that sourceChunkLoader compiles
// and can be used as an interface type in type assertions.
func TestSourceChunkLoaderInterface(t *testing.T) {
	var store VectorStore = newTestStore(t)

	if _, ok := store.(sourceChunkLoader); ok {
		t.Log("SQLiteStore implements sourceChunkLoader")
	}
}

// TestSourceHashCheckerInterface verifies that sourceHashChecker compiles
// and can be used as an interface type in type assertions.
func TestSourceHashCheckerInterface(t *testing.T) {
	var store VectorStore = newTestStore(t)

	if _, ok := store.(sourceHashChecker); ok {
		t.Log("SQLiteStore implements sourceHashChecker")
	}
}

// mockSourceChunkLoader is a test double that implements sourceChunkLoader.
type mockSourceChunkLoader struct {
	chunks map[string][]ChunkWithEmbedding
}

func (m *mockSourceChunkLoader) GetBySource(_ context.Context, source string) ([]ChunkWithEmbedding, error) {
	return m.chunks[source], nil
}

// mockSourceHashChecker is a test double that implements sourceHashChecker.
type mockSourceHashChecker struct {
	hashes map[string]string
}

func (m *mockSourceHashChecker) GetSourceHash(_ context.Context, source string) (string, error) {
	return m.hashes[source], nil
}

// TestMockSourceChunkLoader verifies that the mock satisfies the interface
// and returns expected data.
func TestMockSourceChunkLoader(t *testing.T) {
	mock := &mockSourceChunkLoader{
		chunks: map[string][]ChunkWithEmbedding{
			"main.go": {
				{
					Chunk:     Chunk{ID: "c1", Content: "func main()", Source: "main.go", StartLine: 1, EndLine: 5},
					Embedding: []float64{1.0, 0.0},
				},
			},
		},
	}

	var loader sourceChunkLoader = mock
	ctx := context.Background()

	results, err := loader.GetBySource(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetBySource() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Chunk.ID != "c1" {
		t.Errorf("Chunk.ID = %q, want %q", results[0].Chunk.ID, "c1")
	}

	// Non-existent source returns nil/empty.
	results, err = loader.GetBySource(ctx, "nonexistent.go")
	if err != nil {
		t.Fatalf("GetBySource() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results for nonexistent source, want 0", len(results))
	}
}

// TestMockSourceHashChecker verifies that the mock satisfies the interface
// and returns expected data.
func TestMockSourceHashChecker(t *testing.T) {
	mock := &mockSourceHashChecker{
		hashes: map[string]string{
			"main.go": "abc123",
		},
	}

	var checker sourceHashChecker = mock
	ctx := context.Background()

	hash, err := checker.GetSourceHash(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash != "abc123" {
		t.Errorf("hash = %q, want %q", hash, "abc123")
	}

	// Non-existent source returns empty string.
	hash, err = checker.GetSourceHash(ctx, "nonexistent.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash != "" {
		t.Errorf("hash = %q, want empty string", hash)
	}
}
