//go:build !windows

package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestIndexerConcurrentPartialFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user for permission-based errors")
	}

	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "good.go"), []byte("package good\n\nfunc Good() {}\n"), 0644)

	badFile := filepath.Join(tmpDir, "bad.go")
	os.WriteFile(badFile, []byte("package bad\n"), 0644)
	os.Chmod(badFile, 0000)

	// Verify the chmod actually prevents reading on this platform.
	if _, err := os.ReadFile(badFile); err == nil {
		t.Skip("platform does not enforce POSIX file permissions")
	}

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}

	// Good file should still be indexed despite bad file failure
	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected good file to be indexed despite bad file failure")
	}
}
