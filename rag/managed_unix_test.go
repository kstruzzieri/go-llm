//go:build unix

package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// FIFO coverage is unix-only: syscall.Mkfifo does not exist on Windows.
// The guard under test (readManagedRegularFile) is platform-neutral.
func TestManagedSourcesNonRegularFilesRejectedAndMarkedStale(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	dir := t.TempDir()

	fifo := filepath.Join(dir, "pipe.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// Ingesting a FIFO must fail fast instead of blocking on open.
	if _, err := managed.IngestFile(ctx, fifo, DocumentOptions{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("IngestFile(fifo) error = %v, want not-a-regular-file error", err)
	}

	// Swap a previously ingested origin for a FIFO: listing must mark the
	// document stale without blocking, and reindex must fail fast.
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	document, err := managed.IngestFile(ctx, path, DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestFile() error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo replacement: %v", err)
	}

	got := requireManagedDocument(t, managed, document.ID)
	if got.State != DocumentStateIndexed || got.Freshness != DocumentFreshnessStale {
		t.Fatalf("document = %s/%s, want %s/%s", got.State, got.Freshness, DocumentStateIndexed, DocumentFreshnessStale)
	}

	if _, err := managed.ReindexDocument(ctx, document.ID); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ReindexDocument(fifo origin) error = %v, want not-a-regular-file error", err)
	}
}
