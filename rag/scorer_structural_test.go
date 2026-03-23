package rag

import (
	"context"
	"math"
	"testing"
)

func TestStructuralScorerWorkspaceRootNormalization(t *testing.T) {
	scorer := NewStructuralScorer()

	// Chunks indexed with relative paths (as from IndexDirectory(".")).
	chunks := []Chunk{
		{ID: "c1", Source: "rag/store.go"},
		{ID: "c2", Source: "rag/search.go"},
		{ID: "c3", Source: "ollama/client.go"},
	}

	// Editor provides absolute paths.
	qCtx := QueryContext{
		CurrentFile:   "/home/user/project/rag/store.go",
		WorkspaceRoot: "/home/user/project",
		OpenFiles:     []string{"/home/user/project/ollama/client.go"},
	}

	scores, err := scorer.ScoreBatch(context.Background(), chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	// Same file: relative "rag/store.go" should match absolute
	// "/home/user/project/rag/store.go" through WorkspaceRoot.
	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("same file (normalized): scores[0] = %f, want 1.0", scores[0])
	}
	// Same directory.
	if math.Abs(scores[1]-0.8) > 0.001 {
		t.Errorf("same dir (normalized): scores[1] = %f, want 0.8", scores[1])
	}
	// Open file bonus.
	if math.Abs(scores[2]-0.3) > 0.001 {
		t.Errorf("open file (normalized): scores[2] = %f, want 0.3", scores[2])
	}
}

func TestStructuralScorerAbsoluteChunksRelativeEditor(t *testing.T) {
	scorer := NewStructuralScorer()

	// Chunks indexed with absolute paths.
	chunks := []Chunk{
		{ID: "c1", Source: "/home/user/project/rag/store.go"},
	}

	// Editor provides relative path, workspace root makes it absolute.
	qCtx := QueryContext{
		CurrentFile:   "rag/store.go",
		WorkspaceRoot: "/home/user/project",
	}

	scores, err := scorer.ScoreBatch(context.Background(), chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	// Both resolve to /home/user/project/rag/store.go.
	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("scores[0] = %f, want 1.0", scores[0])
	}
}

func TestStructuralScorerNoWorkspaceRootUnchanged(t *testing.T) {
	scorer := NewStructuralScorer()

	// Without WorkspaceRoot, behavior should match the original:
	// relative paths compared as-is.
	chunks := []Chunk{
		{ID: "c1", Source: "rag/store.go"},
		{ID: "c2", Source: "rag/search.go"},
	}
	qCtx := QueryContext{
		CurrentFile: "rag/store.go",
	}

	scores, err := scorer.ScoreBatch(context.Background(), chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("same file: scores[0] = %f, want 1.0", scores[0])
	}
	if math.Abs(scores[1]-0.8) > 0.001 {
		t.Errorf("same dir: scores[1] = %f, want 0.8", scores[1])
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		workspaceRoot string
		want          string
	}{
		{"relative with root", "rag/store.go", "/home/user/project", "/home/user/project/rag/store.go"},
		{"absolute with root", "/home/user/project/rag/store.go", "/home/user/project", "/home/user/project/rag/store.go"},
		{"relative without root", "rag/store.go", "", "rag/store.go"},
		{"absolute without root", "/home/user/project/rag/store.go", "", "/home/user/project/rag/store.go"},
		{"dot path", "./rag/store.go", "/home/user/project", "/home/user/project/rag/store.go"},
		{"dirty path", "rag/../rag/store.go", "/home/user/project", "/home/user/project/rag/store.go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizePath(tt.path, tt.workspaceRoot)
			if got != tt.want {
				t.Errorf("normalizePath(%q, %q) = %q, want %q", tt.path, tt.workspaceRoot, got, tt.want)
			}
		})
	}
}
