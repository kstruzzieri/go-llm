package rag

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestNewSQLiteStoreConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(_ context.Context, path string) (io.Closer, error) {
		s, err := NewSQLiteStore(path)
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}

// A relative read-only path used to render as file://<name>, which SQLite
// rejects as a URI authority.
func TestOpenSQLiteStoreReadOnlyRelativePath(t *testing.T) {
	dir := t.TempDir()
	seed, err := NewSQLiteStore(filepath.Join(dir, "ro.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	store, err := OpenSQLiteStoreReadOnly("ro.db")
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly(relative) error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
