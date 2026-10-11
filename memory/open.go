// This file provides the shared hardened-open primitives for the package's
// SQLite databases (#237 lesson): private dir/file modes, WAL single-conn
// open, and sidecar re-securing after migrations and after every write.
// cmd/golem and mcp both consume these so the invariant set exists once.

package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
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
// DB paths may live under shared/user-configured directories. path may be any
// input sqlitedsn.Resolve accepts; the file prepared is the one SQLite opens
// for it, and an input naming no file (an in-memory database) prepares
// nothing.
func PrepareDBFile(path string) error {
	disk, err := diskPath(path)
	if err != nil || disk == "" {
		return err
	}
	return prepareDiskFile(disk)
}

// diskPath is the file SQLite opens for path, "" for none (#648).
func diskPath(path string) (string, error) {
	_, disk, err := sqlitedsn.Resolve(path, 0)
	if err != nil {
		return "", fmt.Errorf("memory: resolve db %q: %w", path, err)
	}
	return disk, nil
}

func prepareDiskFile(disk string) error {
	if err := os.MkdirAll(filepath.Dir(disk), dbDirMode); err != nil {
		return fmt.Errorf("memory: create db dir %q: %w", filepath.Dir(disk), err)
	}
	f, err := os.OpenFile(disk, os.O_RDWR|os.O_CREATE, dbFileMode)
	if err != nil {
		return fmt.Errorf("memory: create db %q: %w", disk, err)
	}
	if err := f.Chmod(dbFileMode); err != nil {
		_ = f.Close()
		return fmt.Errorf("memory: chmod db %q: %w", disk, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memory: close db %q: %w", disk, err)
	}
	return nil
}

// OpenHardenedDB prepares the hardened DB file and opens it WAL-mode with a
// single connection (modernc.org/sqlite is not safe for concurrent writers
// on separate connections to the same file). The DSN gives every connection,
// including replacements database/sql opens after a context-cancelled
// statement, a 5s busy_timeout. The file prepared and secured is the one
// SQLite opens for path, which can differ from path itself for a file: URI,
// ".." after a symlink, or a symlinked final component; sqlitedsn.Resolve
// computes it before anything is created, so an input it rejects is never
// created, and the open fails if SQLite then reports a different file. The WAL
// switch retries within that timeout, or until ctx's deadline if sooner, when
// another opener races it, and the open fails if the connection cannot use WAL
// (for example a file: URI with immutable=1 or nolock=1). Existing -wal and
// -shm files are secured too; callers still call SecureDBFiles after
// migrations and writes, which can create new ones.
func OpenHardenedDB(ctx context.Context, path string) (*sql.DB, error) {
	db, _, err := openHardenedDB(ctx, path)
	return db, err
}

// openHardenedDB is OpenHardenedDB that also returns the file SQLite opened
// ("" for an in-memory database).
func openHardenedDB(ctx context.Context, path string) (*sql.DB, string, error) {
	dsn, disk, err := sqlitedsn.Resolve(path, 5*time.Second)
	if err != nil {
		return nil, "", fmt.Errorf("memory: open db %q: %w", path, err)
	}
	if disk != "" {
		if err := prepareDiskFile(disk); err != nil {
			return nil, "", err
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("memory: open db %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedsn.EnableWAL(ctx, db); err != nil {
		_ = db.Close()
		_ = secureDiskFiles(disk)
		return nil, "", fmt.Errorf("memory: db PRAGMA journal_mode=WAL: %w", err)
	}
	if err := sqlitedsn.CheckMainFile(ctx, db, disk); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("memory: open db %q: %w", path, err)
	}
	// Sidecars an older run left loose keep their bits when SQLite reopens
	// them; secure them now rather than at the caller's first SecureDBFiles.
	if err := secureDiskFiles(disk); err != nil {
		_ = db.Close()
		return nil, "", err
	}
	return db, disk, nil
}

// SecureDBFiles chmods the DB file and its -wal/-shm sidecars to 0600.
// Missing sidecars are skipped, including one another connection's last
// close unlinks between its stat and chmod. Callers invoke this after
// migrations and after every write: SQLite gives a new sidecar the DB file's
// mode, but a sidecar left with looser bits (by an older run or a looser DB
// file) keeps them.
// path may be any input sqlitedsn.Resolve accepts: the files secured are the
// ones SQLite opens for it, and an in-memory input secures nothing.
func SecureDBFiles(path string) error {
	disk, err := diskPath(path)
	if err != nil {
		return err
	}
	return secureDiskFiles(disk)
}

func secureDiskFiles(disk string) error {
	if disk == "" {
		return nil
	}
	return secureDBFilesWith(disk, os.Chmod)
}

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
// the handle and returns the error. Without an explicit KeyDir or injected
// credentials, the signing keys live in a ".keys" directory beside the
// database file SQLite opens, so an input naming no file is refused.
func OpenRecordStore(ctx context.Context, path string, config RecordStoreConfig) (*RecordRuntime, error) {
	db, disk, err := openHardenedDB(ctx, path)
	if err != nil {
		return nil, err
	}
	if config.KeyDir == "" && config.Signer == nil && config.Verifiers == nil {
		if disk == "" {
			_ = db.Close()
			return nil, fmt.Errorf("memory: record store %q names no database file to keep signing keys beside; set RecordStoreConfig.KeyDir or inject credentials", path)
		}
		config.KeyDir = disk + ".keys"
		if err := checkLegacyKeyDir(path, config.KeyDir); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	store, err := NewMemoryRecordStore(ctx, db, config)
	if err != nil {
		_ = db.Close()
		// Best-effort: migrations may have created loose sidecars before failing.
		_ = secureDiskFiles(disk)
		return nil, err
	}
	if err := secureDiskFiles(disk); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &RecordRuntime{store: store, db: db, dbPath: disk}, nil
}

// checkLegacyKeyDir refuses to start a new signing identity in keyDir while
// the store's keys may be in the directory derived from the raw path, where
// they lived before #648: for a file: URI, ".." after a symlink, or a
// symlinked final component that names another directory. Moving private keys
// is left to the operator, who the error tells how.
func checkLegacyKeyDir(path, keyDir string) error {
	legacy := path + ".keys"
	if _, err := os.Lstat(keyDir); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err // keys already beside the database (or an error to report)
	}
	legacyInfo, err := os.Lstat(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memory: inspect legacy key directory %q: %w", legacy, err)
	}
	if !legacyInfo.IsDir() {
		return nil
	}
	return fmt.Errorf("memory: signing keys for %q are in %q, where go-llm kept them before it resolved database paths (#648); move that directory to %q (mv %q %q) and reopen", path, legacy, keyDir, legacy, keyDir)
}
