package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/memory"
	_ "modernc.org/sqlite"
)

// capProbeHandle owns the shared fingerprint/capability store and its DB handle
// so run() can close it at shutdown (mirrors behavioralWeighterHandle).
type capProbeHandle struct {
	store    fingerprint.CapProbeStore
	profiles fingerprint.Store
	db       *sql.DB
}

// sidecarSecuringCapStore re-chmods the DB file and its -wal/-shm sidecars
// after every mutation. SaveCapProbe runs mid-session (route-time probes), and
// a WAL checkpoint can recreate a sidecar honoring the umask rather than the DB
// file's 0600 mode (#237 lesson: every writable golem DB re-secures per write).
// The store is the only write seam golem controls, so the invariant lives here.
// Reads pass straight through; only mutations trigger the re-chmod.
type sidecarSecuringCapStore struct {
	inner fingerprint.CapProbeStore
	path  string
}

func (s sidecarSecuringCapStore) GetCapProbe(ctx context.Context, backendID, modelName, capability string) (*fingerprint.CapProbe, error) {
	return s.inner.GetCapProbe(ctx, backendID, modelName, capability)
}

func (s sidecarSecuringCapStore) SaveCapProbe(ctx context.Context, probe fingerprint.CapProbe) error {
	err := s.inner.SaveCapProbe(ctx, probe)
	_ = chmodDBFiles(s.path) // best-effort: a loosened sidecar is worse than a surfaced save error
	return err
}

func (s sidecarSecuringCapStore) DeleteCapProbes(ctx context.Context, backendID, modelName string) error {
	err := s.inner.DeleteCapProbes(ctx, backendID, modelName)
	_ = chmodDBFiles(s.path) // checkpoint can recreate sidecars here too
	return err
}

// capProbeStorePath resolves the shared fingerprint/capability DB path:
// $XDG_DATA_HOME/golem/fingerprints.db (else ~/.local/share/golem/...).
// Mirrors memoryDBPath's use of dataDirBase.
func capProbeStorePath(getenv func(string) string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "golem", "fingerprints.db"), nil
}

// openCapProbeStore opens (creating if needed) the fingerprint/capability store
// with the same hardening as golem's sibling DBs: outside-workspace path
// validation, 0700 dir / 0600 file, WAL single-conn open, and post-migration
// sidecar re-chmod (shared memory.OpenHardenedDB primitives). On any failure
// it degrades to an in-memory store for this run and returns a warning —
// probe verdicts still apply, persistence is lost. Never fatal.
func openCapProbeStore(ctx context.Context, getenv func(string) string, root string) (*capProbeHandle, string) {
	h, err := openCapProbeStoreFile(ctx, getenv, root)
	if err == nil {
		return h, ""
	}
	db, merr := sql.Open("sqlite", ":memory:")
	if merr == nil {
		// database/sql pools connections and each :memory: connection is a
		// separate database; clamp to one so migrations and queries agree.
		db.SetMaxOpenConns(1)
		var s *fingerprint.SQLiteStore
		if s, merr = fingerprint.NewStore(ctx, db); merr == nil {
			return &capProbeHandle{store: s, profiles: s, db: db}, fmt.Sprintf("fingerprint cache degraded to in-memory: %v", err)
		}
		_ = db.Close()
	}
	return nil, fmt.Sprintf("fingerprint cache disabled: %v (memory fallback failed: %v)", err, merr)
}

// openCapProbeStoreFile is the file-backed happy path behind openCapProbeStore.
func openCapProbeStoreFile(ctx context.Context, getenv func(string) string, root string) (*capProbeHandle, error) {
	path, err := capProbeStorePath(getenv)
	if err != nil {
		return nil, err
	}
	if err := validatePathOutsideWorkspace(path, root); err != nil {
		return nil, err
	}
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, err
	}
	store, err := fingerprint.NewStore(ctx, db)
	if err != nil {
		_ = db.Close()
		_ = chmodDBFiles(path) // best-effort: failed migrations may leave loose sidecars
		return nil, err
	}
	if err := chmodDBFiles(path); err != nil { // migrations may create -wal/-shm
		_ = db.Close()
		return nil, err
	}
	// Wrap so every mid-session SaveCapProbe/DeleteCapProbes re-secures sidecars.
	// The in-memory fallback stays unwrapped: it has no file path to chmod.
	return &capProbeHandle{store: sidecarSecuringCapStore{inner: store, path: path}, profiles: store, db: db}, nil
}
