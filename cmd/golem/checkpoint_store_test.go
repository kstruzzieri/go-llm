package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/memory"
)

// testGetenv fakes the environment so the data dir lands in a test temp dir.
func testGetenv(dataDir string) func(string) string {
	return func(k string) string {
		if k == "XDG_DATA_HOME" {
			return dataDir
		}
		return ""
	}
}

// openTestStore opens a checkpoint store for root under a fresh temp data
// dir, failing the test on error and closing it on cleanup.
func openTestStore(t *testing.T, root string) *checkpointStore {
	t.Helper()
	s, err := openCheckpointStore(context.Background(), testGetenv(t.TempDir()), root)
	if err != nil {
		t.Fatalf("openCheckpointStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCheckpointStoreOpenCreatesHardenedPerWorkspaceDB(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	dataDir := t.TempDir()
	ctx := context.Background()

	sa, err := openCheckpointStore(ctx, testGetenv(dataDir), rootA)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer func() { _ = sa.Close() }()

	hashA := strings.TrimPrefix(workspaceID(rootA), "workspace:")
	wantPath := filepath.Join(dataDir, "golem", "checkpoints", hashA+".db")
	if sa.dbPath != wantPath {
		t.Fatalf("dbPath = %q, want %q", sa.dbPath, wantPath)
	}
	dirInfo, err := os.Stat(filepath.Dir(wantPath))
	if err != nil {
		t.Fatalf("stat leaf dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("leaf dir mode = %o, want 700", got)
	}
	for _, p := range []string{wantPath, wantPath + ".lock"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, got)
		}
	}

	sb, err := openCheckpointStore(ctx, testGetenv(dataDir), rootB)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer func() { _ = sb.Close() }()
	if sb.dbPath == sa.dbPath {
		t.Fatalf("two workspaces share one DB file: %q", sb.dbPath)
	}
}

func TestCheckpointStoreRechmodsLooseLeafDir(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	leaf := filepath.Join(dataDir, "golem", "checkpoints")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(leaf, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := openCheckpointStore(context.Background(), testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()
	info, err := os.Stat(leaf)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("loose leaf dir mode = %o, want re-secured 700", got)
	}
}

func TestCheckpointStoreFreshSidecarsPrivate(t *testing.T) {
	root := t.TempDir()
	s := openTestStore(t, root)
	// The v1 migration writes through WAL, so the -wal sidecar must exist.
	if _, err := os.Stat(s.dbPath + "-wal"); err != nil {
		t.Fatalf("stat -wal: %v", err)
	}
	for _, p := range []string{s.dbPath, s.dbPath + "-wal", s.dbPath + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue // -shm may not exist on every platform
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, got)
		}
	}
}

func TestCheckpointStoreRejectsDBInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "data")
	if _, err := openCheckpointStore(context.Background(), testGetenv(inside), root); err == nil {
		t.Fatal("want error for data dir inside the workspace, got nil")
	}
	if _, err := os.Stat(filepath.Join(inside, "golem")); !os.IsNotExist(err) {
		t.Fatalf("refused open still created files under the workspace: %v", err)
	}
}

func TestCheckpointStoreMigratesToV1(t *testing.T) {
	root := t.TempDir()
	s := openTestStore(t, root)
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
	for _, table := range []string{"checkpoints", "checkpoint_files"} {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("sqlite_master %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='checkpoints_one_open'`).Scan(&n); err != nil {
		t.Fatalf("index query: %v", err)
	}
	if n != 1 {
		t.Error("partial unique index checkpoints_one_open missing")
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Error("foreign_keys pragma is off; ON DELETE CASCADE would silently not run")
	}
}

func TestCheckpointStoreRejectsNewerSchema(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	ctx := context.Background()
	s, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dbPath := s.dbPath
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := memory.OpenHardenedDB(ctx, dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}
	if _, err := openCheckpointStore(ctx, testGetenv(dataDir), root); err == nil {
		t.Fatal("want error for schema newer than this binary supports")
	}
}

// TestCheckpointLeaseHelper is the subprocess body for the cross-process lease
// test: it attempts a full store open and reports the outcome via exit code.
func TestCheckpointLeaseHelper(t *testing.T) {
	if os.Getenv("GO_LLM_CHECKPOINT_LEASE_HELPER") != "1" {
		return
	}
	root := os.Getenv("GO_LLM_CHECKPOINT_LEASE_ROOT")
	dataDir := os.Getenv("GO_LLM_CHECKPOINT_LEASE_DATA")
	s, err := openCheckpointStore(context.Background(), testGetenv(dataDir), root)
	if errors.Is(err, errCheckpointLeaseHeld) {
		os.Exit(3)
	}
	if err != nil {
		os.Exit(4)
	}
	if err := s.Close(); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func runCheckpointLeaseHelper(t *testing.T, root, dataDir string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointLeaseHelper$")
	cmd.Env = append(os.Environ(),
		"GO_LLM_CHECKPOINT_LEASE_HELPER=1",
		"GO_LLM_CHECKPOINT_LEASE_ROOT="+root,
		"GO_LLM_CHECKPOINT_LEASE_DATA="+dataDir)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func TestCheckpointLeaseContendsAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	ctx := context.Background()
	s, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := runCheckpointLeaseHelper(t, root, dataDir); got != 3 {
		t.Fatalf("contending process exit = %d, want 3 (lease held)", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := runCheckpointLeaseHelper(t, root, dataDir); got != 0 {
		t.Fatalf("process after release exit = %d, want 0", got)
	}
}
