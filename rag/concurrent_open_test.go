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

// A caller's cache=shared would join the btree of an earlier read-write opener
// in the same process and ignore mode=ro, making the snapshot writable.
func TestOpenSQLiteStoreReadOnlyIgnoresSharedCache(t *testing.T) {
	uri := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "shared.db")) + "?cache=shared"
	rw, err := NewSQLiteStore(uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rw.Close() })
	ro, err := OpenSQLiteStoreReadOnly(uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if _, err := ro.db.Exec("CREATE TABLE ro_write_probe (x)"); err == nil {
		t.Fatal("CREATE TABLE through the read-only store succeeded; want a read-only error")
	}
}

// "" is SQLite's private temporary database: every pooled connection would get
// its own unmigrated copy, so the pool must stay at one connection like :memory:.
func TestNewSQLiteStoreEmptyPathUsesOneConnection(t *testing.T) {
	store, err := NewSQLiteStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
}
