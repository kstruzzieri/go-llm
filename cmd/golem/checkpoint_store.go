package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/memory"
)

// checkpointState is the lifecycle of one persisted checkpoint (#355).
// open: the turn is in progress, or a process died mid-turn (startup
// reconciliation resolves those). completed: sealed at turn end; the only
// state retention may prune. undoing: an undo committed intent; never pruned,
// deleted only when every applied file row is restored.
type checkpointState string

const (
	checkpointOpen      checkpointState = "open"
	checkpointCompleted checkpointState = "completed"
	checkpointUndoing   checkpointState = "undoing"
)

// Retention defaults (issue #355): strict per-workspace caps.
const (
	defaultMaxCheckpoints = 50
	defaultMaxPriorBytes  = 64 << 20 // 64 MiB of prior content
)

// checkpointSchemaVersion is the newest schema this binary understands. The
// schema is internal: #347 consumes the semantic mutation/turn contract, never
// these tables.
const checkpointSchemaVersion = 1

// errCheckpointLeaseHeld reports that another live golem process owns this
// workspace's checkpoint store. Checkpoints fail closed on contention (D10):
// there is no last-writer-wins mode for undo history.
var errCheckpointLeaseHeld = errors.New("golem: another golem process holds this workspace's checkpoint store")

// checkpointStore persists per-turn mutation checkpoints in one hardened
// per-workspace SQLite DB under the per-user data dir, owned exclusively via
// an OS file lease for the store's lifetime. It knows nothing about the
// Workspace; restoring files is checkpointJournal's job. Deleted rows never
// shrink the DB file (no vacuum): the disk high-water mark is bounded by the
// retention caps plus SQLite overhead.
type checkpointStore struct {
	db     *sql.DB
	dbPath string
	lease  *flockLease

	maxCheckpoints int
	maxPriorBytes  int64
}

// checkpointDBPath locates the per-workspace checkpoint DB OUTSIDE the repo:
// <dataDirBase>/golem/checkpoints/<workspace-hash>.db, where the hash is the
// stable identity workspaceID derives from the canonical root.
func checkpointDBPath(getenv func(string) string, root string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	hash := strings.TrimPrefix(workspaceID(root), "workspace:")
	return filepath.Join(base, "golem", "checkpoints", hash+".db"), nil
}

// openCheckpointStore acquires this workspace's exclusive checkpoint lease,
// opens (creating if missing) the hardened DB, migrates to the v1 schema, and
// re-secures every on-disk file. Any failure releases whatever was acquired
// and returns the error: -allow-write startup fails closed on it (D6).
func openCheckpointStore(ctx context.Context, getenv func(string) string, root string) (*checkpointStore, error) {
	path, err := checkpointDBPath(getenv, root)
	if err != nil {
		return nil, err
	}
	if err := validatePathOutsideWorkspace(path, root); err != nil {
		return nil, err
	}
	// Create and re-chmod the leaf directory only; existing parents may be
	// shared/user-configured (mirrors memory.PrepareDBFile's stance).
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("golem: create checkpoint dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("golem: chmod checkpoint dir %q: %w", dir, err)
	}
	lease, err := acquireFlockLease(path + ".lock")
	if err != nil {
		if errors.Is(err, errIndexWriterLeaseHeld) {
			return nil, fmt.Errorf("%w (lock %s)", errCheckpointLeaseHeld, path+".lock")
		}
		return nil, err
	}
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	s := &checkpointStore{
		db:             db,
		dbPath:         path,
		lease:          lease,
		maxCheckpoints: defaultMaxCheckpoints,
		maxPriorBytes:  defaultMaxPriorBytes,
	}
	fail := func(err error) (*checkpointStore, error) {
		return nil, errors.Join(err, db.Close(), memory.SecureDBFiles(path), lease.Close())
	}
	// The single connection relies on ON DELETE CASCADE for checkpoint_files.
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fail(fmt.Errorf("golem: checkpoint foreign_keys: %w", err))
	}
	if err := s.migrate(ctx); err != nil {
		return fail(err)
	}
	if err := memory.SecureDBFiles(path); err != nil {
		return nil, errors.Join(err, db.Close(), lease.Close())
	}
	return s, nil
}

// migrate applies the v1 schema and version stamp in one transaction, so a
// crash mid-migration leaves version 0 and a clean retry. A database from a
// newer binary is rejected rather than guessed at.
func (s *checkpointStore) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("golem: checkpoint schema version: %w", err)
	}
	if version > checkpointSchemaVersion {
		return fmt.Errorf("golem: checkpoint db %s is schema v%d; this binary supports up to v%d",
			s.dbPath, version, checkpointSchemaVersion)
	}
	if version == checkpointSchemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("golem: checkpoint migrate begin: %w", err)
	}
	stmts := []string{
		`CREATE TABLE checkpoints (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT    NOT NULL,
			goal       TEXT    NOT NULL,
			state      TEXT    NOT NULL
				CHECK (state IN ('open', 'completed', 'undoing'))
		)`,
		`CREATE UNIQUE INDEX checkpoints_one_open
			ON checkpoints(state) WHERE state = 'open'`,
		`CREATE INDEX checkpoints_state_id ON checkpoints(state, id DESC)`,
		`CREATE TABLE checkpoint_files (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			checkpoint_id INTEGER NOT NULL
				REFERENCES checkpoints(id) ON DELETE CASCADE,
			path          TEXT    NOT NULL,
			prior_content BLOB,
			prior_hash    TEXT    NOT NULL,
			existed       INTEGER NOT NULL CHECK (existed IN (0, 1)),
			after_hash    TEXT    NOT NULL,
			summary       TEXT    NOT NULL,
			at            TEXT    NOT NULL,
			applied       INTEGER NOT NULL DEFAULT 0 CHECK (applied IN (0, 1)),
			restored      INTEGER NOT NULL DEFAULT 0 CHECK (restored IN (0, 1)),
			CHECK (restored = 0 OR applied = 1)
		)`,
		`CREATE INDEX checkpoint_files_checkpoint
			ON checkpoint_files(checkpoint_id, id DESC)`,
		fmt.Sprintf("PRAGMA user_version = %d", checkpointSchemaVersion),
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return errors.Join(fmt.Errorf("golem: checkpoint migrate: %w", err), tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("golem: checkpoint migrate commit: %w", err)
	}
	return nil
}

// secure re-chmods the DB file and sidecars (the #237 per-write invariant).
func (s *checkpointStore) secure() error { return memory.SecureDBFiles(s.dbPath) }

// Close releases the DB handle and the workspace lease, joining both errors.
func (s *checkpointStore) Close() error {
	return errors.Join(s.db.Close(), s.lease.Close())
}
