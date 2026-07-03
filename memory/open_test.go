package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRecordStoreCreatesHardenedDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "memories.db")

	rt, err := OpenRecordStore(context.Background(), path)
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

	rt, err := OpenRecordStore(context.Background(), path)
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

func TestPrepareDBFileRechmodsExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir loose: %v", err)
	}
	path := filepath.Join(dir, "memories.db")
	if err := PrepareDBFile(path); err != nil {
		t.Fatalf("PrepareDBFile() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("existing dir perm = %o, want re-chmodded 0700", perm)
	}
}

func TestOpenRecordStoreFailurePaths(t *testing.T) {
	// Parent "dir" is actually a file -> MkdirAll fails -> error, no panic.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := OpenRecordStore(context.Background(), filepath.Join(blocker, "memories.db"))
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
