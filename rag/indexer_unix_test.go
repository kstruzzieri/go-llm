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
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "good.go"), []byte("package good\n\nfunc Good() {}\n"), 0644)

	badFile := filepath.Join(tmpDir, "bad.go")
	_ = os.WriteFile(badFile, []byte("package bad\n"), 0644)
	_ = os.Chmod(badFile, 0000)

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

func TestIndexDirectory_PruneDeleted_SkipsOnIncompleteWalk(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user for permission-based errors")
	}

	idx, store := newPruneTestIndexer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	keptFile := filepath.Join(tmpDir, "main.go")
	lockedDir := filepath.Join(tmpDir, "locked")
	_ = os.MkdirAll(lockedDir, 0755)
	lockedFile := filepath.Join(lockedDir, "inner.go")
	_ = os.WriteFile(keptFile, []byte("package main\n\nfunc Hello() {}\n"), 0644)
	_ = os.WriteFile(lockedFile, []byte("package locked\n\nfunc Inner() {}\n"), 0644)

	if err := idx.IndexDirectory(ctx, tmpDir); err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}
	if set := sourceSet(t, store); !set[lockedFile] {
		t.Fatalf("expected %q indexed on first run, got sources %v", lockedFile, set)
	}

	// Make the subtree unreadable: the walk drops it from the eligible set
	// with a non-fatal access error, which must disable pruning entirely.
	_ = os.Chmod(lockedDir, 0000)
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0755) })
	if _, err := os.ReadDir(lockedDir); err == nil {
		t.Skip("platform does not enforce POSIX directory permissions")
	}

	status, err := idx.IndexDirectoryWithStatus(ctx, tmpDir, WithIncremental(), WithPruneDeleted())
	if err == nil {
		t.Fatal("expected error for unreadable subtree")
	}

	set := sourceSet(t, store)
	if !set[lockedFile] {
		t.Errorf("unreadable subtree source %q was pruned; incomplete walk must skip pruning", lockedFile)
	}
	if !set[keptFile] {
		t.Errorf("kept file %q missing", keptFile)
	}

	found := false
	for _, e := range status.Errors {
		if e == "prune skipped: directory walk was incomplete" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("status.Errors missing prune-skipped entry, got %v", status.Errors)
	}
}
