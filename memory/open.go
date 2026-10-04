// This file provides the shared hardened-open primitives for the package's
// SQLite databases (#237 lesson): private dir/file modes, WAL single-conn
// open, and sidecar re-securing after migrations and after every write.
// cmd/golem and mcp both consume these so the invariant set exists once.

package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

const (
	dbDirMode  = 0o700
	dbFileMode = 0o600
)

// PrepareDBFile creates a missing DB parent directory (0700) and the DB file
// itself (0600), re-chmodding an existing file so a previously-loosened DB
// is re-secured on open. Existing parent directories are left alone because
// DB paths may live under shared/user-configured directories.
func PrepareDBFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dbDirMode); err != nil {
			return fmt.Errorf("memory: create db dir %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return fmt.Errorf("memory: create db %q: %w", path, err)
	}
	if err := f.Chmod(dbFileMode); err != nil {
		_ = f.Close()
		return fmt.Errorf("memory: chmod db %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memory: close db %q: %w", path, err)
	}
	return nil
}

// OpenHardenedDB prepares the hardened DB file and opens it WAL-mode with a
// single connection (modernc.org/sqlite is not safe for concurrent writers
// on separate connections to the same file). The DSN gives every connection,
// including replacements database/sql opens after a context-cancelled
// statement, a 5s busy_timeout; it is built before the file is prepared, so a
// path it rejects is never created. The WAL switch retries within that
// timeout, or until ctx's deadline if sooner, when another opener races it,
// and the open fails if the connection cannot use WAL (for example a file: URI
// with immutable=1 or nolock=1).
func OpenHardenedDB(ctx context.Context, path string) (*sql.DB, error) {
	dsn, err := sqlitedsn.WithBusyTimeout(path, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("memory: open db %q: %w", path, err)
	}
	if err := PrepareDBFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memory: open db %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedsn.EnableWAL(ctx, db); err != nil {
		_ = db.Close()
		_ = SecureDBFiles(path)
		return nil, fmt.Errorf("memory: db PRAGMA journal_mode=WAL: %w", err)
	}
	return db, nil
}

// SecureDBFiles chmods the DB file and its -wal/-shm sidecars to 0600.
// Missing sidecars are skipped, including one another connection's last
// close unlinks between its stat and chmod. Callers invoke this after
// migrations and after every write: SQLite gives a new sidecar the DB file's
// mode, but a sidecar left with looser bits (by an older run or a looser DB
// file) keeps them.
func SecureDBFiles(path string) error { return secureDBFilesWith(path, os.Chmod) }

// secureDBFilesWith is SecureDBFiles with the chmod call injected, so tests
// can remove a sidecar between its Stat and its Chmod.
func secureDBFilesWith(path string, chmod func(string, os.FileMode) error) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("memory: stat %q: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("memory: %q is a directory", p)
		}
		if err := chmod(p, dbFileMode); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("memory: chmod %q: %w", p, err)
		}
	}
	return nil
}

// RecordRuntime bundles an opened, hardened MemoryRecordStore with its
// lifecycle: Secure() re-chmods sidecars (call after every write), Close()
// releases the underlying handle.
type RecordRuntime struct {
	store  *MemoryRecordStore
	db     *sql.DB
	dbPath string
}

// Store returns the record store.
func (r *RecordRuntime) Store() *MemoryRecordStore { return r.store }

// Secure re-chmods the DB file and sidecars (per-write invariant).
func (r *RecordRuntime) Secure() error { return SecureDBFiles(r.dbPath) }

// Close closes the underlying database handle.
func (r *RecordRuntime) Close() error { return r.db.Close() }

// OpenRecordStore opens path hardened, runs the package migrations, and
// re-secures the files migrations may have (re)created. Any failure closes
// the handle and returns the error.
func OpenRecordStore(ctx context.Context, path string, config RecordStoreConfig) (*RecordRuntime, error) {
	db, err := OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if config.KeyDir == "" && config.Signer == nil && config.Verifiers == nil {
		config.KeyDir = path + ".keys"
	}
	store, err := NewMemoryRecordStore(ctx, db, config)
	if err != nil {
		_ = db.Close()
		// Best-effort: migrations may have created loose sidecars before failing.
		_ = SecureDBFiles(path)
		return nil, err
	}
	if err := SecureDBFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &RecordRuntime{store: store, db: db, dbPath: path}, nil
}
