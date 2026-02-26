package rag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlidingWindowChunker(t *testing.T) {
	sw := NewSlidingWindowChunker(100, 20)

	content := strings.Repeat("line of text\n", 20)
	chunks, err := sw.Chunk("test.txt", content)
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// Verify all chunks have IDs and source
	for i, c := range chunks {
		if c.ID == "" {
			t.Errorf("chunk %d has empty ID", i)
		}
		if c.Source != "test.txt" {
			t.Errorf("chunk %d source = %q, want %q", i, c.Source, "test.txt")
		}
		if c.StartLine < 1 {
			t.Errorf("chunk %d start line = %d, want >= 1", i, c.StartLine)
		}
	}
}

func TestSlidingWindowChunkerEmpty(t *testing.T) {
	sw := NewSlidingWindowChunker(100, 20)
	chunks, err := sw.Chunk("empty.txt", "")
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty content, got %d", len(chunks))
	}
}

func TestSlidingWindowChunkerSmallContent(t *testing.T) {
	sw := NewSlidingWindowChunker(1000, 100)
	chunks, err := sw.Chunk("small.txt", "short text")
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small content, got %d", len(chunks))
	}
	if chunks[0].Content != "short text" {
		t.Errorf("content = %q, want %q", chunks[0].Content, "short text")
	}
}

func TestCodeChunkerGo(t *testing.T) {
	testdataDir := filepath.Join("..", "testdata")
	content, err := os.ReadFile(filepath.Join(testdataDir, "sample.go"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	chunker := NewCodeChunker(WithMaxChunkSize(500))
	chunks, err := chunker.Chunk("sample.go", string(content))
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	// All chunks should be detected as Go
	for i, c := range chunks {
		if c.Language != "go" {
			t.Errorf("chunk %d language = %q, want %q", i, c.Language, "go")
		}
	}
}

func TestCodeChunkerPython(t *testing.T) {
	testdataDir := filepath.Join("..", "testdata")
	content, err := os.ReadFile(filepath.Join(testdataDir, "sample.py"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	chunker := NewCodeChunker(WithMaxChunkSize(300))
	chunks, err := chunker.Chunk("sample.py", string(content))
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	for i, c := range chunks {
		if c.Language != "python" {
			t.Errorf("chunk %d language = %q, want %q", i, c.Language, "python")
		}
	}
}

func TestCodeChunkerTypeScript(t *testing.T) {
	testdataDir := filepath.Join("..", "testdata")
	content, err := os.ReadFile(filepath.Join(testdataDir, "sample.ts"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	chunker := NewCodeChunker(WithMaxChunkSize(400))
	chunks, err := chunker.Chunk("sample.ts", string(content))
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	for i, c := range chunks {
		if c.Language != "typescript" {
			t.Errorf("chunk %d language = %q, want %q", i, c.Language, "typescript")
		}
	}
}

func TestCodeChunkerFallsBackToSlidingWindow(t *testing.T) {
	testdataDir := filepath.Join("..", "testdata")
	content, err := os.ReadFile(filepath.Join(testdataDir, "plain.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	chunker := NewCodeChunker(WithMaxChunkSize(200))
	chunks, err := chunker.Chunk("plain.txt", string(content))
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk from fallback")
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"app.py", "python"},
		{"index.ts", "typescript"},
		{"index.tsx", "typescript"},
		{"app.js", "javascript"},
		{"lib.rs", "rust"},
		{"README.md", ""},
		{"data.json", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := detectLanguage(tt.path)
			if got != tt.want {
				t.Errorf("detectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestChunkIDDeterministic(t *testing.T) {
	id1 := chunkID("file.go", "hello world")
	id2 := chunkID("file.go", "hello world")
	if id1 != id2 {
		t.Errorf("chunkID not deterministic: %q != %q", id1, id2)
	}

	id3 := chunkID("file.go", "different content")
	if id1 == id3 {
		t.Error("different content should produce different IDs")
	}

	id4 := chunkID("other.go", "hello world")
	if id1 == id4 {
		t.Error("different source should produce different IDs")
	}
}

func TestWithLanguageOption(t *testing.T) {
	chunker := NewCodeChunker(WithLanguage("python"))
	// Should treat content as Python regardless of file extension
	content := "def hello():\n    return 'hello'\n\ndef world():\n    return 'world'\n"
	chunks, err := chunker.Chunk("unknown_ext.xyz", content)
	if err != nil {
		t.Fatalf("Chunk() error: %v", err)
	}
	for i, c := range chunks {
		if c.Language != "python" {
			t.Errorf("chunk %d language = %q, want %q", i, c.Language, "python")
		}
	}
}
