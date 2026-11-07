package fingerprint

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kstruzzieri/go-llm/internal/datadir"
	"github.com/kstruzzieri/go-llm/internal/pathguard"
	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
	"github.com/kstruzzieri/go-llm/memory"
)

// StoreStatus says what backs a SharedStore.
type StoreStatus string

const (
	// StoreFile means the store is the hardened file under the data directory.
	StoreFile StoreStatus = "file"
	// StoreMemoryOnly means the file could not be used; answers last for this run only.
	StoreMemoryOnly StoreStatus = "memory-only"
	// StoreDisabled means not even the in-memory store could be opened.
	StoreDisabled StoreStatus = "disabled"
)

// SharedStore is the capability store every host opens the same way: one
// hardened file, WAL, one connection, with an in-memory fallback.
type SharedStore struct {
	status  StoreStatus
	warning string
	db      *sql.DB
	sqlite  *SQLiteStore
	secured *securingStore // nil for memory-only and disabled
	pin     *memoryPin     // set for memory-only
}

// OpenSharedStore opens $XDG_DATA_HOME/golem/fingerprints.db (else
// ~/.local/share/golem/fingerprints.db) with outside-workspace path validation,
// a 0600 file and secured -wal/-shm sidecars; a directory it has to create is
// 0700, and one that exists keeps its mode. A path inside workspaceRoot, or any
// open failure, degrades to an in-memory store (Status memory-only), and failing
// that to Status disabled; Warning says why. The error is returned only for
// misuse: a nil ctx or getenv. Evidence rows past retention are pruned once at
// open, and only when there are some, so a quiet open writes nothing; a prune
// that fails for any reason but a cancelled open or a busy store leaves Status
// as it is and puts the failure in Warning, since it can mean a damaged file. An
// empty workspaceRoot means no workspace, and skips the outside-workspace check.
func OpenSharedStore(ctx context.Context, getenv func(string) string, workspaceRoot string) (*SharedStore, error) {
	if ctx == nil || getenv == nil {
		return nil, errors.New("fingerprint: open shared store: nil context or getenv")
	}
	s, err := openSharedFile(ctx, getenv, workspaceRoot)
	if err == nil {
		return s, nil
	}
	db, st, pin, merr := openMemoryStore(ctx)
	if merr == nil {
		return &SharedStore{
			status:  StoreMemoryOnly,
			warning: fmt.Sprintf("fingerprint cache degraded to in-memory: %v", err),
			db:      db, sqlite: st, pin: pin,
		}, nil
	}
	return &SharedStore{
		status:  StoreDisabled,
		warning: fmt.Sprintf("fingerprint cache disabled: %v (memory fallback failed: %v)", err, merr),
	}, nil
}

// memoryPin keeps a named in-memory database alive. database/sql discards the
// store's one connection after a cancelled read, and SQLite frees the database
// with its last connection.
type memoryPin struct {
	db   *sql.DB
	conn *sql.Conn
}

func (p *memoryPin) close() error {
	err := p.conn.Close()
	if errors.Is(err, sql.ErrConnDone) { // a second Close
		err = nil
	}
	return errors.Join(err, p.db.Close())
}

// openMemoryStore opens the in-memory fallback, under a random name because a
// shared-cache name is process-wide and every store stays private to its handle;
// a variable so a test can make it fail.
var openMemoryStore = func(ctx context.Context) (*sql.DB, *SQLiteStore, *memoryPin, error) {
	dsn := sqlitedsn.SharedMemoryURI("fp-mem-" + rand.Text())
	pinDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := pinDB.Conn(ctx)
	if err != nil {
		_ = pinDB.Close()
		return nil, nil, nil, err
	}
	pin := &memoryPin{db: pinDB, conn: conn}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = pin.close()
		return nil, nil, nil, err
	}
	db.SetMaxOpenConns(1)
	st, err := NewStore(ctx, db)
	if err != nil {
		_ = db.Close()
		_ = pin.close()
		return nil, nil, nil, err
	}
	return db, st, pin, nil
}

func openSharedFile(ctx context.Context, getenv func(string) string, root string) (*SharedStore, error) {
	base, err := datadir.Base(getenv)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(base, "golem", "fingerprints.db")
	if root != "" {
		if err := pathguard.ValidateOutside(path, root); err != nil {
			return nil, err
		}
	}
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(ctx, db)
	if err != nil {
		_ = db.Close()
		_ = memory.SecureDBFiles(path) // best-effort: failed migrations may leave loose sidecars
		return nil, err
	}
	warning := pruneWarning(ctx, store.Prune(ctx, time.Now()))
	if err := memory.SecureDBFiles(path); err != nil { // migrations and the prune may create -wal/-shm
		_ = db.Close()
		return nil, err
	}
	return &SharedStore{
		status: StoreFile, warning: warning, db: db, sqlite: store,
		secured: &securingStore{SQLiteStore: store, path: path},
	}, nil
}

// pruneWarning stays silent for a failed prune that only leaves old rows for the
// next open: a cancelled open, or another writer holding the store. Anything else
// may be a damaged file, and the prune is the first read that would notice.
func pruneWarning(ctx context.Context, err error) string {
	if err == nil || ctx.Err() != nil {
		return ""
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		if code := sqliteErr.Code() & 0xff; code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED {
			return ""
		}
	}
	return "fingerprint cache could not be pruned and may be damaged: " + err.Error()
}

// Status reports what backs the store.
func (s *SharedStore) Status() StoreStatus { return s.status }

// Warning is why the store is not file-backed, or why a file-backed store could
// not be pruned at open; empty when there is neither.
func (s *SharedStore) Warning() string { return s.warning }

// Log is the evidence log, or a nil interface when the store is disabled.
// Every write re-secures the file's sidecars.
func (s *SharedStore) Log() EvidenceStore {
	switch {
	case s.secured != nil:
		return s.secured
	case s.sqlite != nil:
		return s.sqlite
	}
	return nil
}

// Probes is the legacy capability_probes table, or a nil interface when the
// store is disabled. Every write re-secures the file's sidecars.
func (s *SharedStore) Probes() CapProbeStore {
	switch {
	case s.secured != nil:
		return s.secured
	case s.sqlite != nil:
		return s.sqlite
	}
	return nil
}

// Profiles is the fingerprint profile store, or a nil interface when the store
// is disabled. Every write re-secures the file's sidecars.
func (s *SharedStore) Profiles() Store {
	switch {
	case s.secured != nil:
		return s.secured
	case s.sqlite != nil:
		return s.sqlite
	}
	return nil
}

// Close closes the database. It is safe on a disabled store.
func (s *SharedStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.pin != nil {
		err = errors.Join(err, s.pin.close())
	}
	return err
}

// securingStore re-chmods the DB file and its -wal/-shm sidecars after every
// write. A sidecar left with looser bits by an older run keeps them, since
// SQLite only gives new sidecars the DB file's mode, so the invariant lives at
// the write seam. Reads pass through the embedded store.
type securingStore struct {
	*SQLiteStore
	path string
}

func (s *securingStore) secure() { _ = memory.SecureDBFiles(s.path) } // best-effort: a loosened sidecar is worse than a surfaced write error

func (s *securingStore) AppendEvidence(ctx context.Context, rows ...EvidenceRow) error {
	defer s.secure()
	return s.SQLiteStore.AppendEvidence(ctx, rows...)
}

func (s *securingStore) AppendCheckResult(ctx context.Context, endpoint, apiKey, model string, rows []EvidenceRow, access *ModelAccess) error {
	defer s.secure()
	return s.SQLiteStore.AppendCheckResult(ctx, endpoint, apiKey, model, rows, access)
}

func (s *securingStore) PublishListings(ctx context.Context, pubs []ListingPublish) error {
	defer s.secure()
	return s.SQLiteStore.PublishListings(ctx, pubs)
}

func (s *securingStore) AddNamedModel(ctx context.Context, endpoint, model string) error {
	defer s.secure()
	return s.SQLiteStore.AddNamedModel(ctx, endpoint, model)
}

func (s *securingStore) RemoveNamedModel(ctx context.Context, endpoint, model string) error {
	defer s.secure()
	return s.SQLiteStore.RemoveNamedModel(ctx, endpoint, model)
}

func (s *securingStore) RecordModelAccess(ctx context.Context, endpoint, apiKey, model string, state AccessState, status int, at time.Time) error {
	defer s.secure()
	return s.SQLiteStore.RecordModelAccess(ctx, endpoint, apiKey, model, state, status, at)
}

func (s *securingStore) SaveCapProbe(ctx context.Context, probe CapProbe) error {
	defer s.secure()
	return s.SQLiteStore.SaveCapProbe(ctx, probe)
}

func (s *securingStore) DeleteCapProbes(ctx context.Context, backendID, modelName string) error {
	defer s.secure()
	return s.SQLiteStore.DeleteCapProbes(ctx, backendID, modelName)
}

func (s *securingStore) Save(ctx context.Context, profile Profile) error {
	defer s.secure()
	return s.SQLiteStore.Save(ctx, profile)
}

func (s *securingStore) SaveFailure(ctx context.Context, backendID, modelName, modelDigest, errMsg string) error {
	defer s.secure()
	return s.SQLiteStore.SaveFailure(ctx, backendID, modelName, modelDigest, errMsg)
}
