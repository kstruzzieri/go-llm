package rag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errTestEmbed = errors.New("test embed failure")

// stubEmbedder returns a fixed-dim vector per input with a stable vector space.
func stubEmbedder(vsid string) Embedder {
	return EmbedderFunc(func(_ context.Context, model string, inputs []string) (EmbedResult, error) {
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return EmbedResult{Embeddings: vecs, Model: "m", Provider: "p", VectorSpaceID: vsid}, nil
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexDirectoryWithStatus_Counts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "b.md"), "# B\n\nsome text\n")
	writeFile(t, filepath.Join(dir, "skip.bin"), "binary-ish")              // excluded by extension
	writeFile(t, filepath.Join(dir, "node_modules", "c.go"), "package c\n") // excluded dir

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	idx, err := NewIndexerWithEmbedder(stubEmbedder("p/m"), store, WithEmbeddingModel("p/m"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := idx.IndexDirectoryWithStatus(context.Background(), dir, WithExclude("node_modules"))
	if err != nil {
		t.Fatalf("IndexDirectoryWithStatus: %v", err)
	}
	if st.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2 (a.go, b.md)", st.TotalFiles)
	}
	if st.IndexedFiles != 2 {
		t.Errorf("IndexedFiles = %d, want 2", st.IndexedFiles)
	}
	if len(st.Errors) != 0 {
		t.Errorf("Errors = %v, want none", st.Errors)
	}
	if st.InProgress {
		t.Error("InProgress = true, want false in final snapshot")
	}
}

func TestIndexDirectoryWithStatus_PartialErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(dir, "bad.go"), "package a\n\nfunc B() {}\n")

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// Embedder errors only for the file whose content contains "func B".
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		for _, in := range inputs {
			if strings.Contains(in, "func B") {
				return EmbedResult{}, errTestEmbed
			}
		}
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return EmbedResult{Embeddings: vecs, Model: "m", Provider: "p", VectorSpaceID: "p/m"}, nil
	})
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("p/m"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := idx.IndexDirectoryWithStatus(context.Background(), dir)
	if err == nil {
		t.Fatal("want aggregate error for partial failure")
	}
	if st.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2", st.TotalFiles)
	}
	if st.IndexedFiles != 1 {
		t.Errorf("IndexedFiles = %d, want 1", st.IndexedFiles)
	}
	if len(st.Errors) != 1 {
		t.Errorf("Errors = %v, want exactly 1", st.Errors)
	}
}
