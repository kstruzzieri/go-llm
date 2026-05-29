// transcript/store_test.go
package transcript

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open(\"\") should error")
	}
}

func TestOpen_MemoryAndCloseIdempotent(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpen_FileUsesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}
