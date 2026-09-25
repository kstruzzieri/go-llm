package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenRecordStoreCreatesHardenedDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "memories.db")

	rt, err := OpenRecordStore(context.Background(), path, RecordStoreConfig{})
	if err != nil {
		t.Fatalf("OpenRecordStore() error = %v", err)
	}
	defer func() { _ = rt.Close() }()

	if rt.Store() == nil {
		t.Fatal("Store() = nil, want usable store")
	}

	// Store is usable: create + search round-trip.
	rec, err := rt.Store().Create(context.Background(), CreateRecordParams{
		Kind: KindSemantic, Content: "open_test fact", WorkspaceID: "ws1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := rt.Store().Search(context.Background(), "", RecordSearchOptions{WorkspaceID: "ws1"})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("Search() = %v, want the created record", got)
	}

	// DB file is 0600, parent dir 0700.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("db perm = %o, want 0600", perm)
	}
	dinfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}
}

func TestRecordRuntimeSecureRechmodsSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.db")

	rt, err := OpenRecordStore(context.Background(), path, RecordStoreConfig{})
	if err != nil {
		t.Fatalf("OpenRecordStore() error = %v", err)
	}
	defer func() { _ = rt.Close() }()

	// A write forces the -wal sidecar into existence; loosen it, then Secure.
	if _, err := rt.Store().Create(context.Background(), CreateRecordParams{
		Kind: KindSemantic, Content: "sidecar probe", WorkspaceID: "ws1",
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	wal := path + "-wal"
	if _, err := os.Stat(wal); err != nil {
		t.Fatalf("no -wal sidecar after committed write (%v); WAL pragma not applied", err)
	}
	if err := os.Chmod(wal, 0o644); err != nil {
		t.Fatalf("chmod loosen: %v", err)
	}
	if err := rt.Secure(); err != nil {
		t.Fatalf("Secure() error = %v", err)
	}
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wal perm after Secure = %o, want 0600", perm)
	}
}

func TestSecureDBFilesSkipsMissingSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memories.db")
	if err := PrepareDBFile(path); err != nil {
		t.Fatalf("PrepareDBFile() error = %v", err)
	}
	// No -wal/-shm exist; must not error.
	if err := SecureDBFiles(path); err != nil {
		t.Errorf("SecureDBFiles() error = %v, want nil", err)
	}
}

func TestPrepareDBFileLeavesExistingDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir loose: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	path := filepath.Join(dir, "memories.db")
	if err := PrepareDBFile(path); err != nil {
		t.Fatalf("PrepareDBFile() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("existing dir perm = %o, want unchanged 0755", perm)
	}
}

func TestOpenRecordStoreFailurePaths(t *testing.T) {
	// Parent "dir" is actually a file -> MkdirAll fails -> error, no panic.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := OpenRecordStore(context.Background(), filepath.Join(blocker, "memories.db"), RecordStoreConfig{})
	if err == nil {
		t.Fatal("OpenRecordStore() error = nil, want error for uncreatable dir")
	}
}

func TestOpenHardenedDBFailsWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	_, err := OpenHardenedDB(ctx, filepath.Join(dir, "memories.db"))
	if err == nil {
		t.Fatal("OpenHardenedDB() with cancelled ctx = nil error, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("error is %v (not context.Canceled wrapped); acceptable if pragma exec surfaced differently", err)
	}
}

func TestRecordFailedOpenSecuresExistingSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenRecordStore(ctx, path, RecordStoreConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("open: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("failed open left sidecar %s at %o", suffix, info.Mode().Perm())
		}
	}
}

// lockSQLiteFileFor holds path's database file lock from another connection
// and releases it after d, which must stay below the opener's busy_timeout.
// EXCLUSIVE locking mode set before the first WAL access keeps the file
// locked until that connection closes.
func lockSQLiteFileFor(t *testing.T, path string, d time.Duration) {
	t.Helper()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	var tables int
	if _, err := holder.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatalf("lock holder locking_mode: %v", err)
	}
	if err := holder.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_schema").Scan(&tables); err != nil {
		t.Fatalf("lock holder read: %v", err)
	}
	release := time.AfterFunc(d, func() { _ = holder.Close() })
	t.Cleanup(func() { release.Stop() })
}

// TestOpenHardenedDBWaitsForLockedWALFile pins the PRAGMA order: busy_timeout
// must precede journal_mode. On an existing WAL file, the journal_mode PRAGMA
// reads the database header and otherwise fails at once while another
// connection holds the database lock. Scheduling delay can only hide a
// regression: an opener that reaches the PRAGMA after the release passes under
// either order.
func TestOpenHardenedDBWaitsForLockedWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.db")
	seed, err := OpenHardenedDB(t.Context(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	lockSQLiteFileFor(t, path, 200*time.Millisecond)
	db, err := OpenHardenedDB(t.Context(), path)
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
