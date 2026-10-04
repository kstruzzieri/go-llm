package rag

import (
	"context"
	"io"
	"path/filepath"
	"strings"
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
// A connection that cannot use WAL must fail the open, not run the store with
// a rollback journal: nolock=1 turns locking off, and SQLite then reports
// journal mode "delete".
func TestNewSQLiteStoreRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "rag.db"))
	store, err := NewSQLiteStore("file:" + path + "?nolock=1")
	if err == nil {
		_ = store.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
}

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
