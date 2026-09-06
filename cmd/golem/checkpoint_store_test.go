package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/signing"
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

	canonicalA, err := agenttools.CanonicalWorkspaceRoot(rootA)
	if err != nil {
		t.Fatalf("canonical root A: %v", err)
	}
	if sa.workspaceHash != agenttools.ContentHash([]byte(canonicalA)) {
		t.Fatalf("workspace receipt hash = %q, want full canonical-root hash", sa.workspaceHash)
	}
	hashA := strings.TrimPrefix(workspaceID(canonicalA), "workspace:")
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

func TestCheckpointStoreCaseAliasSharesLease(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "Workspace")
	alias := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem is case-sensitive")
	}

	ctx := context.Background()
	getenv := testGetenv(t.TempDir())
	first, err := openCheckpointStore(ctx, getenv, root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := openCheckpointStore(ctx, getenv, alias)
	if second != nil {
		defer func() { _ = second.Close() }()
	}
	if !errors.Is(err, errCheckpointLeaseHeld) {
		t.Fatalf("open case alias = %v, want shared lease refusal", err)
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

func TestCheckpointStoreMigratesToCurrent(t *testing.T) {
	root := t.TempDir()
	s := openTestStore(t, root)
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != checkpointSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, checkpointSchemaVersion)
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

var testNow = time.Date(2026, 8, 25, 21, 0, 0, 0, time.UTC)

// testRec builds a valid synthetic transition for signed store fixtures.
func testRec(path, prior string, existed bool) agenttools.MutationRecord {
	var pc []byte
	if existed {
		pc = []byte(prior)
	}
	return agenttools.MutationRecord{
		Path: path, PriorContent: pc, Existed: existed,
		AfterHash: agenttools.ContentHash([]byte("after-" + path)), Summary: "write " + path, At: testNow,
	}
}

// mustPrepare inserts one intent, failing the test on error.
func mustPrepare(t *testing.T, s *checkpointStore, goal string, rec agenttools.MutationRecord) (int64, int64) {
	t.Helper()
	cpID, fileID, err := s.testPrepareIntent(context.Background(), goal, testNow, rec)
	if err != nil {
		t.Fatalf("prepareIntent(%s): %v", rec.Path, err)
	}
	return cpID, fileID
}

// sealAppliedCheckpoint runs one full prepared+committed+sealed checkpoint
// carrying a single file with priorBytes bytes of prior content.
func sealAppliedCheckpoint(t *testing.T, s *checkpointStore, goal string, priorBytes int) int64 {
	t.Helper()
	ctx := context.Background()
	rec := testRec("f.txt", strings.Repeat("x", priorBytes), true)
	cpID, fileID, err := s.testPrepareIntent(ctx, goal, testNow, rec)
	if err != nil {
		t.Fatalf("prepareIntent: %v", err)
	}
	if err := s.testCommitIntent(ctx, fileID); err != nil {
		t.Fatalf("commitIntent: %v", err)
	}
	if err := s.seal(ctx, cpID); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return cpID
}

func listIDs(t *testing.T, s *checkpointStore) []int64 {
	t.Helper()
	infos, err := s.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]int64, 0, len(infos))
	for _, in := range infos {
		ids = append(ids, in.id)
	}
	return ids
}

func TestCheckpointStorePrepareLazyCreatesOneOpenCheckpoint(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	cp1, f1 := mustPrepare(t, s, "turn goal", testRec("a.go", "A0", true))
	cp2, f2 := mustPrepare(t, s, "turn goal", testRec("b.go", "", false))
	if cp1 != cp2 {
		t.Fatalf("two intents in one turn split checkpoints: %d vs %d", cp1, cp2)
	}
	if f2 <= f1 {
		t.Fatalf("file ids not ordered: %d then %d", f1, f2)
	}
	infos, err := s.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].state != checkpointOpen || infos[0].files != 2 {
		t.Fatalf("infos = %+v, want one open checkpoint with 2 files", infos)
	}
	if infos[0].goal != "turn goal" {
		t.Errorf("goal = %q", infos[0].goal)
	}
	if infos[0].bytes != 2 { // "A0" only; the create carries no prior content
		t.Errorf("bytes = %d, want 2", infos[0].bytes)
	}
}

func TestCheckpointStorePrepareAfterSealStartsNewCheckpoint(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	first := sealAppliedCheckpoint(t, s, "turn one", 2)
	cp2, _ := mustPrepare(t, s, "turn two", testRec("b.go", "B0", true))
	if cp2 == first {
		t.Fatalf("intent appended to a sealed checkpoint %d", first)
	}
	infos, err := s.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 2 || infos[0].id != cp2 || infos[0].state != checkpointOpen ||
		infos[1].id != first || infos[1].state != checkpointCompleted {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestCheckpointStoreCommitAbortOnlyUnapplied(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	_, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.testCommitIntent(ctx, f1); err == nil {
		t.Fatal("second commit of an applied row must fail")
	}
	if err := s.abortIntent(ctx, f1); err == nil {
		t.Fatal("abort of an applied row must fail")
	}
	_, f2 := mustPrepare(t, s, "g", testRec("b.go", "B0", true))
	if err := s.abortIntent(ctx, f2); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := s.abortIntent(ctx, f2); err == nil {
		t.Fatal("second abort of a deleted row must fail")
	}
	if err := s.testCommitIntent(ctx, f2); err == nil {
		t.Fatal("commit of an aborted row must fail")
	}
}

func TestCheckpointStoreSealRequiresZeroUnapplied(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.seal(ctx, cp); err == nil {
		t.Fatal("seal with an unapplied intent must fail")
	}
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.seal(ctx, cp); err == nil {
		t.Fatal("sealing a completed checkpoint again must fail")
	}
}

func TestCheckpointStoreSealDeletesEmptyCheckpoint(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.abortIntent(ctx, f1); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal of empty checkpoint: %v", err)
	}
	if ids := listIDs(t, s); len(ids) != 0 {
		t.Fatalf("ids = %v, want empty (all intents aborted publishes nothing)", ids)
	}
}

func TestCheckpointStoreMarkUndoingOnlyCompleted(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.markUndoing(ctx, []int64{cp}); err == nil {
		t.Fatal("markUndoing on an open checkpoint must fail")
	}
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := s.markUndoing(ctx, []int64{cp}); err == nil {
		t.Fatal("markUndoing twice must fail (already undoing)")
	}
}

func TestCheckpointStoreMarkRestoredRequiresUndoing(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.markRestored(ctx, f1); err == nil {
		t.Fatal("markRestored while completed must fail; only an undoing checkpoint restores files")
	}
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := s.markRestored(ctx, f1); err != nil {
		t.Fatalf("markRestored: %v", err)
	}
	if err := s.markRestored(ctx, f1); err == nil {
		t.Fatal("markRestored twice must fail")
	}
}

func TestCheckpointStoreDeleteRestoredGuards(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := s.deleteRestored(ctx, cp); err == nil {
		t.Fatal("deleteRestored on a completed checkpoint must fail")
	}
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := s.deleteRestored(ctx, cp); err == nil {
		t.Fatal("deleteRestored with an unrestored applied row must fail")
	}
	if ids := listIDs(t, s); len(ids) != 1 {
		t.Fatalf("refused delete removed the checkpoint: %v", ids)
	}
	if err := s.markRestored(ctx, f1); err != nil {
		t.Fatalf("markRestored: %v", err)
	}
	if err := s.deleteRestored(ctx, cp); err != nil {
		t.Fatalf("deleteRestored: %v", err)
	}
	if ids := listIDs(t, s); len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
}

func TestCheckpointRetentionCount50And51(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	var ids []int64
	for i := 0; i < 50; i++ {
		ids = append(ids, sealAppliedCheckpoint(t, s, fmt.Sprintf("turn %d", i), 1))
	}
	if got := listIDs(t, s); len(got) != 50 {
		t.Fatalf("at cap: %d checkpoints, want all 50 retained", len(got))
	}
	newest := sealAppliedCheckpoint(t, s, "turn 50", 1)
	got := listIDs(t, s)
	if len(got) != 50 {
		t.Fatalf("after 51st: %d checkpoints, want 50", len(got))
	}
	if got[0] != newest {
		t.Fatalf("newest %d missing from head: %v", newest, got[0])
	}
	if got[len(got)-1] != ids[1] {
		t.Fatalf("oldest survivor = %d, want %d (only the very oldest pruned)", got[len(got)-1], ids[1])
	}
}

func TestCheckpointRetentionSizeBoundaryAndRefusal(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	s.maxPriorBytes = 250
	ctx := context.Background()

	first := sealAppliedCheckpoint(t, s, "one", 100)
	second := sealAppliedCheckpoint(t, s, "two", 100)

	// Admission of a third 100-byte intent must prune the oldest completed
	// checkpoint (200+100 > 250 -> drop first -> 200 <= 250).
	cp3, f3, err := s.testPrepareIntent(ctx, "three", testNow, testRec("c.txt", strings.Repeat("x", 100), true))
	if err != nil {
		t.Fatalf("prepareIntent: %v", err)
	}
	got := listIDs(t, s)
	want := []int64{cp3, second}
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v (oldest completed pruned at admission)", got, want)
	}
	if err := s.testCommitIntent(ctx, f3); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp3); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// An intent that cannot fit even after pruning every completed checkpoint
	// is refused, and the rollback keeps existing history intact.
	_, _, err = s.testPrepareIntent(ctx, "huge", testNow, testRec("d.txt", strings.Repeat("x", 300), true))
	if !errors.Is(err, errCheckpointQuota) {
		t.Fatalf("err = %v, want errCheckpointQuota", err)
	}
	after := listIDs(t, s)
	if !slices.Equal(after, []int64{cp3, second}) {
		t.Fatalf("refused admission mutated history: %v", after)
	}
	_ = first

	// Strict D8: even the NEWEST completed checkpoint is prunable at
	// admission — a lone 200-byte checkpoint yields to a 100-byte intent.
	s2 := openTestStore(t, t.TempDir())
	s2.maxPriorBytes = 250
	only := sealAppliedCheckpoint(t, s2, "only", 200)
	cpN, _, err := s2.testPrepareIntent(ctx, "new", testNow, testRec("n.txt", strings.Repeat("x", 100), true))
	if err != nil {
		t.Fatalf("admission must prune the newest completed too (no D8 exemption): %v", err)
	}
	if got := listIDs(t, s2); !slices.Equal(got, []int64{cpN}) {
		t.Fatalf("ids = %v, want only the new open checkpoint %d (%d pruned)", got, cpN, only)
	}
}

func TestCheckpointRetentionProtectsActiveStates(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	s.maxPriorBytes = 250
	s.maxCheckpoints = 1
	ctx := context.Background()

	undoing := sealAppliedCheckpoint(t, s, "to undo", 100)
	if err := s.markUndoing(ctx, []int64{undoing}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	openCP, _ := mustPrepare(t, s, "live turn", testRec("live.txt", strings.Repeat("y", 100), true))

	// Size pressure: 200 held by protected states + 100 new > 250, and no
	// completed victim exists -> refusal, both protected rows survive.
	_, _, err := s.testPrepareIntent(ctx, "pressure", testNow, testRec("p.txt", strings.Repeat("z", 100), true))
	if !errors.Is(err, errCheckpointQuota) {
		t.Fatalf("err = %v, want errCheckpointQuota (open/undoing are never pruned)", err)
	}
	got := listIDs(t, s)
	if !slices.Equal(got, []int64{openCP, undoing}) {
		t.Fatalf("ids = %v, want open %d and undoing %d protected", got, openCP, undoing)
	}
	_ = openCP
}

func TestCheckpointFilesStayPrivateAfterEveryMutation(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	wal := s.dbPath + "-wal"

	loosen := func() {
		t.Helper()
		if err := os.Chmod(wal, 0o644); err != nil {
			t.Fatalf("loosen: %v", err)
		}
	}
	assertPrivate := func(op string) {
		t.Helper()
		info, err := os.Stat(wal)
		if err != nil {
			t.Fatalf("stat after %s: %v", op, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("wal mode after %s = %o, want 600 (store mutations must re-secure)", op, got)
		}
	}

	loosen()
	cp, f1, err := s.testPrepareIntent(ctx, "g", testNow, testRec("a.go", "A0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertPrivate("prepareIntent")
	loosen()
	if err := s.testCommitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertPrivate("commitIntent")
	loosen()
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	assertPrivate("seal")
	loosen()
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	assertPrivate("markUndoing")
	loosen()
	if err := s.markRestored(ctx, f1); err != nil {
		t.Fatalf("markRestored: %v", err)
	}
	assertPrivate("markRestored")
	loosen()
	if err := s.deleteRestored(ctx, cp); err != nil {
		t.Fatalf("deleteRestored: %v", err)
	}
	assertPrivate("deleteRestored")
}

func TestCheckpointStoreSanitizesGoal(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	cases := []struct{ in, want string }{
		{"a\nb\x1b[31mc", "a b[31mc"},                        // newline flattened, ESC stripped
		{"x\u202eevil", `x\u202eevil`},                       // bidi control escaped, cannot reorder
		{strings.Repeat("é", 200), strings.Repeat("é", 160)}, // rune-safe truncation
	}
	for i, c := range cases {
		cp, f, err := s.testPrepareIntent(context.Background(), c.in, testNow, testRec(fmt.Sprintf("f%d.txt", i), "p", true))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.testCommitIntent(context.Background(), f); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.seal(context.Background(), cp); err != nil {
			t.Fatalf("seal: %v", err)
		}
		infos, err := s.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if infos[0].goal != c.want {
			t.Errorf("case %d: goal = %q, want %q", i, infos[0].goal, c.want)
		}
	}
}

func TestCheckpointStoreCanonicalizesStoredPath(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f, err := s.testPrepareIntent(ctx, "g", testNow, testRec("./x/../a.go", "A0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := s.testCommitIntent(ctx, f); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	groups, err := s.newestCompleted(ctx, 1)
	if err != nil {
		t.Fatalf("newestCompleted: %v", err)
	}
	if len(groups) != 1 || len(groups[0].files) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if got := groups[0].files[0].path; got != "a.go" {
		t.Errorf("stored path = %q, want canonical a.go", got)
	}
}

func TestCheckpointStoreLoadsAppliedRowsNewestFirst(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f1 := mustPrepare(t, s, "g", testRec("a.go", "A0", true))
	_, f2 := mustPrepare(t, s, "g", testRec("b.go", "B0", true))
	_, f3 := mustPrepare(t, s, "g", testRec("c.go", "", false))
	for _, f := range []int64{f1, f2} {
		if err := s.testCommitIntent(ctx, f); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	if err := s.abortIntent(ctx, f3); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatalf("seal: %v", err)
	}
	groups, err := s.newestCompleted(ctx, 1)
	if err != nil {
		t.Fatalf("newestCompleted: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	g := groups[0]
	if g.id != cp || len(g.files) != 2 {
		t.Fatalf("group = %+v, want 2 applied files (aborted c.go excluded)", g)
	}
	// Reverse mutation order: newest write first.
	if g.files[0].path != "b.go" || g.files[1].path != "a.go" {
		t.Fatalf("order = %s, %s want b.go, a.go", g.files[0].path, g.files[1].path)
	}
	f := g.files[1]
	if string(f.priorContent) != "A0" || !f.existed || f.afterHash != agenttools.ContentHash([]byte("after-a.go")) ||
		f.priorHash != agenttools.ContentHash([]byte("A0")) || !f.applied || f.restored {
		t.Fatalf("file = %+v", f)
	}
}

// --- #443 Task 8: schema v2 (nullable after_mode) ---

func TestCheckpointStoreSchemaV2TrackedModeRoundTrip(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("fresh store version = %d, want 3", version)
	}
	cpID, _, err := s.testPrepareIntent(ctx, "goal", time.Now(), agenttools.MutationRecord{
		Path: "promoted.txt", AfterHash: agenttools.ContentHash([]byte("abc")), TrackedMode: true, AfterMode: 0o640, At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.testPrepareIntent(ctx, "goal", time.Now(), agenttools.MutationRecord{
		Path: "legacy.txt", AfterHash: agenttools.ContentHash([]byte("def")), At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	files, err := s.loadFiles(ctx, cpID, false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]checkpointFile{}
	for _, f := range files {
		byPath[f.path] = f
	}
	tracked := byPath["promoted.txt"]
	if !tracked.trackedMode || tracked.afterMode != 0o640 {
		t.Fatalf("tracked row lost its mode: %+v", tracked)
	}
	legacy := byPath["legacy.txt"]
	if legacy.trackedMode || legacy.afterMode != 0 {
		t.Fatalf("legacy row must stay untracked: %+v", legacy)
	}
}

// buildV1CheckpointDB creates a version-1 database with one applied legacy
// row, exactly as a pre-#443 binary would have left it.
func buildV1CheckpointDB(t *testing.T, getenv func(string) string, root string) string {
	t.Helper()
	path, err := checkpointDBPath(getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE checkpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL,
			goal TEXT NOT NULL, state TEXT NOT NULL CHECK (state IN ('open','completed','undoing')))`,
		`CREATE UNIQUE INDEX checkpoints_one_open ON checkpoints(state) WHERE state = 'open'`,
		`CREATE INDEX checkpoints_state_id ON checkpoints(state, id DESC)`,
		`CREATE TABLE checkpoint_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			checkpoint_id INTEGER NOT NULL REFERENCES checkpoints(id) ON DELETE CASCADE,
			path TEXT NOT NULL, prior_content BLOB, prior_hash TEXT NOT NULL,
			existed INTEGER NOT NULL CHECK (existed IN (0,1)), after_hash TEXT NOT NULL,
			summary TEXT NOT NULL, at TEXT NOT NULL,
			applied INTEGER NOT NULL DEFAULT 0 CHECK (applied IN (0,1)),
			restored INTEGER NOT NULL DEFAULT 0 CHECK (restored IN (0,1)),
			CHECK (restored = 0 OR applied = 1))`,
		`CREATE INDEX checkpoint_files_checkpoint ON checkpoint_files(checkpoint_id, id DESC)`,
		`INSERT INTO checkpoints (created_at, goal, state) VALUES ('2026-01-01T00:00:00Z', 'old goal', 'completed')`,
		`INSERT INTO checkpoint_files
			(checkpoint_id, path, prior_content, prior_hash, existed, after_hash, summary, at, applied)
			VALUES (1, 'old.txt', X'6f6c64', 'ph', 1, 'ah', 's', '2026-01-01T00:00:00Z', 1)`,
		`PRAGMA user_version = 1`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("v1 fixture %q: %v", stmt[:30], err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckpointStoreV1MigratesTransactionally(t *testing.T) {
	root := t.TempDir()
	getenv := testGetenv(t.TempDir())
	buildV1CheckpointDB(t, getenv, root)
	s, err := openCheckpointStore(context.Background(), getenv, root)
	if err != nil {
		t.Fatalf("v1 -> v3 migration failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("migrated version = %d, want 3", version)
	}
	files, err := s.loadFiles(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("migration lost rows: %d", len(files))
	}
	f := files[0]
	if f.path != "old.txt" || string(f.priorContent) != "old" || !f.existed || f.afterHash != "ah" {
		t.Fatalf("migration mangled the legacy row: %+v", f)
	}
	if f.trackedMode || f.afterMode != 0 {
		t.Fatalf("NULL after_mode must load untracked: %+v", f)
	}
}

func TestCheckpointStoreNewerSchemaFailsClosed(t *testing.T) {
	root := t.TempDir()
	getenv := testGetenv(t.TempDir())
	path := buildV1CheckpointDB(t, getenv, root)
	ctx := context.Background()
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openCheckpointStore(ctx, getenv, root); err == nil {
		t.Fatal("a newer schema must fail closed")
	}
}

func checkpointSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}

func TestCheckpointStoreV3MigrationPreservesLegacy(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			root, getenv := t.TempDir(), testGetenv(t.TempDir())
			path := buildV1CheckpointDB(t, getenv, root)
			db, err := memory.OpenHardenedDB(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			checkpointSQL(t, db, `INSERT INTO checkpoint_files (id, checkpoint_id, path, prior_content, prior_hash, existed, after_hash, summary, at, applied, restored) VALUES (17, 1, 'second', X'0001ff', 'before', 1, 'after', 's2', '2026-01-01T00:00:00Z', 1, 1)`)
			if version == 2 {
				checkpointSQL(t, db, `ALTER TABLE checkpoint_files ADD COLUMN after_mode INTEGER`)
				checkpointSQL(t, db, `UPDATE checkpoint_files SET after_mode = 0 WHERE id = 17`)
				checkpointSQL(t, db, `PRAGMA user_version = 2`)
			}
			var oldRoot int
			if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 'checkpoint_files'`).Scan(&oldRoot); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := openCheckpointStore(context.Background(), getenv, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			var newRoot, gotVersion int
			if err := s.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 'checkpoint_files'`).Scan(&newRoot); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&gotVersion); err != nil {
				t.Fatal(err)
			}
			if oldRoot != newRoot || gotVersion != 3 {
				t.Fatalf("migration rebuilt table or wrong version: roots %d/%d version %d", oldRoot, newRoot, gotVersion)
			}
			files, err := s.loadFiles(context.Background(), 1, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 2 || files[0].id != 17 || files[0].path != "second" || string(files[0].priorContent) != "\x00\x01\xff" || files[0].priorHash != "before" || files[0].afterHash != "after" || !files[0].existed || !files[0].applied || !files[0].restored || files[0].trackedMode != (version == 2) || files[0].afterMode != 0 || files[1].id != 1 || string(files[1].priorContent) != "old" {
				t.Fatalf("migration changed legacy rows: %+v", files)
			}
			var nullRefs int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_files WHERE forward_mutation_id IS NULL AND inverse_mutation_id IS NULL`).Scan(&nullRefs); err != nil {
				t.Fatal(err)
			}
			if nullRefs != 2 {
				t.Fatalf("legacy NULL refs = %d, want 2", nullRefs)
			}
			infos, err := s.list(context.Background())
			if err != nil || len(infos) != 1 || infos[0].id != 1 || infos[0].state != checkpointCompleted || infos[0].goal != "old goal" || infos[0].bytes != 6 {
				t.Fatalf("legacy listing = %+v, %v", infos, err)
			}
			for _, column := range []string{"forward_mutation_id", "inverse_mutation_id"} {
				if _, err := s.db.Exec(`UPDATE checkpoint_files SET ` + column + ` = 'missing' WHERE id = 1`); err == nil {
					t.Fatalf("%s accepted dangling reference", column)
				}
				checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES (?, '{}')`, column)
				checkpointSQL(t, s.db, `UPDATE checkpoint_files SET `+column+` = ? WHERE id = 1`, column)
				if _, err := s.db.Exec(`UPDATE checkpoint_files SET `+column+` = ? WHERE id = 17`, column); err == nil {
					t.Fatalf("%s accepted duplicate reference", column)
				}
			}
		})
	}
}

func TestCheckpointStoreV3MigrationRefusesInterruptedLegacy(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, state := range []string{"open", "undoing"} {
			t.Run(fmt.Sprintf("v%d/%s", version, state), func(t *testing.T) {
				root, getenv := t.TempDir(), testGetenv(t.TempDir())
				path := buildV1CheckpointDB(t, getenv, root)
				db, err := memory.OpenHardenedDB(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				checkpointSQL(t, db, `UPDATE checkpoints SET state = ?`, state)
				if version == 2 {
					checkpointSQL(t, db, `ALTER TABLE checkpoint_files ADD COLUMN after_mode INTEGER`)
					checkpointSQL(t, db, `PRAGMA user_version = 2`)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				s, err := openCheckpointStore(context.Background(), getenv, root)
				if s != nil {
					_ = s.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "old binary") {
					t.Fatalf("interrupted upgrade = %v, want old-binary recovery guidance", err)
				}
				db, err = memory.OpenHardenedDB(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				var gotVersion, ledger int
				if err := db.QueryRow(`PRAGMA user_version`).Scan(&gotVersion); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'mutation_receipts'`).Scan(&ledger); err != nil {
					t.Fatal(err)
				}
				var gotState, content string
				if err := db.QueryRow(`SELECT c.state, f.prior_content FROM checkpoints c JOIN checkpoint_files f ON f.checkpoint_id = c.id`).Scan(&gotState, &content); err != nil {
					t.Fatal(err)
				}
				if gotVersion != version || ledger != 0 || gotState != state || content != "old" {
					t.Fatalf("refused migration changed DB: %d, %d, %q, %q", gotVersion, ledger, gotState, content)
				}
			})
		}
	}
}

// This literal whole envelope uses Task 1's independently calculated RFC 8032
// test vector. Expectations never call the canonical encoder under test.
const storedReceiptGolden = `{"body":{"after_hash":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","after_mode":420,"agent_id":"b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00","before_hash":"absent","kind":"intent","mutation_id":"AAAAAAAAAAAAAAAAAAAAAAAAAA","path":"src/main.go","timestamp":"2026-09-05T12:34:56.123456789Z","undo_of":"","workspace_hash":"c52ddf65534b7b46035084358ab7902be4bfef220bdb503ac7039cc861905b05"},"signature":{"alg":"ed25519","kid":"b0f0e4099cd739f05a2defb07e1940a08ffabcfc8ce64a4e6deeaa9365b4bf00","sig":"8nl//cjzEis32LmLH+TK+Vej5oIf+So670gRzhG/13K2/QtUcXxka/Zysbg/m/I5iACtIU7HWHxrHlysOYgkAg=="}}`

func storeReceiptJSON(t *testing.T, body agenttools.MutationReceiptBody) []byte {
	t.Helper()
	signer, err := signing.NewHMAC(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	body.AgentID = signer.KeyID()
	receipt, err := agenttools.SignMutationReceipt(context.Background(), signer, body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signing.MarshalCanonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func storeForward(t *testing.T, s *checkpointStore, id string, prior string) (agenttools.MutationRecord, agenttools.MutationReceiptBody, []byte) {
	t.Helper()
	rec := agenttools.MutationRecord{Path: "a.txt", PriorContent: []byte(prior), Existed: true, AfterHash: agenttools.ContentHash([]byte("new")), At: testNow}
	body := agenttools.MutationReceiptBody{Kind: "intent", MutationID: id, WorkspaceHash: s.workspaceHash, Path: "a.txt", BeforeHash: agenttools.ContentHash([]byte(prior)), AfterHash: rec.AfterHash, Timestamp: "2026-08-25T21:00:00Z"}
	return rec, body, storeReceiptJSON(t, body)
}

func storeApplied(t *testing.T, body agenttools.MutationReceiptBody) []byte {
	t.Helper()
	body.Kind = "applied"
	body.Timestamp = "2026-08-25T20:00:00Z" // Backward clocks do not order the ledger.
	return storeReceiptJSON(t, body)
}

func storeInverse(body agenttools.MutationReceiptBody, id string) agenttools.MutationReceiptBody {
	body.MutationID, body.UndoOf = id, body.MutationID
	body.BeforeHash, body.AfterHash = body.AfterHash, body.BeforeHash
	body.AfterMode = nil
	return body
}

func TestCheckpointStoreSignedForwardAtomic(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
	cp, f, err := s.prepareSignedIntent(ctx, "signed", testNow, rec, intent)
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.loadFileMetadata(ctx, cp, false)
	if err != nil || len(files) != 1 || files[0].forwardMutationID != (sql.NullString{String: body.MutationID, Valid: true}) || files[0].inverseMutationID.Valid || files[0].priorContent != nil || files[0].applied {
		t.Fatalf("prepared metadata = %+v, %v", files, err)
	}
	entry, err := s.loadReceipt(ctx, body.MutationID)
	if err != nil || !bytes.Equal(entry.intentJSON, intent) || entry.appliedJSON != nil || entry.sequence != 1 {
		t.Fatalf("prepared evidence = %+v, %v", entry, err)
	}
	applied := storeApplied(t, body)
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_forward BEFORE UPDATE OF applied ON checkpoint_files BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if err := s.commitSignedIntent(ctx, f, applied); err == nil {
		t.Fatal("commit ignored flag write failure")
	}
	entry, err = s.loadReceipt(ctx, body.MutationID)
	if err != nil || entry.appliedJSON != nil {
		t.Fatalf("failed flag update leaked evidence: %+v, %v", entry, err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_forward`)
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_receipt BEFORE UPDATE OF applied_json ON mutation_receipts BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if err := s.commitSignedIntent(ctx, f, applied); err == nil {
		t.Fatal("commit ignored receipt write failure")
	}
	files, err = s.loadFiles(ctx, cp, false)
	if err != nil || files[0].applied {
		t.Fatalf("failed evidence update leaked flag: %+v, %v", files, err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_receipt`)
	if err := s.commitSignedIntent(ctx, f, applied); err != nil {
		t.Fatal(err)
	}
	entry, err = s.loadReceipt(ctx, body.MutationID)
	files, ferr := s.loadFiles(ctx, cp, false)
	if err != nil || ferr != nil || !bytes.Equal(entry.appliedJSON, applied) || !files[0].applied || string(files[0].priorContent) != "old" {
		t.Fatalf("committed evidence/flag = %+v, %+v, %v, %v", entry, files, err, ferr)
	}
	if err := s.commitSignedIntent(ctx, f, applied); err == nil {
		t.Fatal("duplicate commit changed immutable evidence")
	}
	if err := s.abortIntent(ctx, f); err == nil {
		t.Fatal("abort removed applied evidence")
	}
	if _, _, err := s.prepareSignedIntent(ctx, "duplicate", testNow, rec, intent); err == nil {
		t.Fatal("duplicate mutation identity accepted")
	}
}

func TestCheckpointStoreSignedPrepareRollsBackAndAbortRetainsNoUnusedIntent(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_file BEFORE INSERT ON checkpoint_files BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if _, _, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent); err == nil {
		t.Fatal("prepare ignored snapshot failure")
	}
	entries, err := s.scanReceipts(ctx, 0, 10)
	if err != nil || len(entries) != 0 || len(listIDs(t, s)) != 0 {
		t.Fatalf("prepare leaked ledger/checkpoint rows: %+v, %v", entries, err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_file`)
	_, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_abort BEFORE DELETE ON mutation_receipts BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if err := s.abortIntent(ctx, f); err == nil {
		t.Fatal("abort ignored ledger deletion failure")
	}
	var refs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_files WHERE forward_mutation_id = ?`, body.MutationID).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("failed abort leaked reference deletion: %d, %v", refs, err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_abort`)
	if err := s.abortIntent(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadReceipt(ctx, body.MutationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("definite abort retained intent: %v", err)
	}
}

func TestCheckpointStoreSignedBindingRefusals(t *testing.T) {
	for _, field := range []string{"kind", "id", "workspace", "path", "before", "after", "mode", "undo"} {
		t.Run(field, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
			_, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
			if err != nil {
				t.Fatal(err)
			}
			bad := body
			switch field {
			case "kind":
				bad.Kind = "applied"
			case "id":
				bad.MutationID = strings.Repeat("B", 26)
			case "workspace":
				bad.WorkspaceHash = strings.Repeat("0", 64)
			case "path":
				bad.Path = "other.txt"
			case "before":
				bad.BeforeHash = "absent"
			case "after":
				bad.AfterHash = strings.Repeat("0", 64)
			case "mode":
				rec.Existed = false
				rec.PriorContent = nil
				bad.BeforeHash = "absent"
				mode := uint32(0)
				bad.AfterMode = &mode
			case "undo":
				bad.UndoOf = strings.Repeat("B", 26)
			}
			if field != "id" { // A distinct valid identity has no checkpoint binding until prepare.
				prepareBad := bad
				prepareBad.MutationID = strings.Repeat("C", 26)
				if _, _, err := s.prepareSignedIntent(ctx, "bad", testNow, rec, storeReceiptJSON(t, prepareBad)); err == nil {
					t.Fatal("prepare accepted mismatching signed metadata")
				}
			}
			bad.Kind = "applied"
			if field == "kind" {
				bad.Kind = "intent"
			}
			if err := s.commitSignedIntent(ctx, f, storeReceiptJSON(t, bad)); err == nil {
				t.Fatal("commit accepted mismatching evidence")
			}
			entry, err := s.loadReceipt(ctx, body.MutationID)
			if err != nil || entry.appliedJSON != nil {
				t.Fatalf("refusal changed evidence: %+v, %v", entry, err)
			}
		})
	}
}

func TestCheckpointStoreStoredEnvelopeCanonicalBoundary(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	const id = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES (?, ?)`, id, storedReceiptGolden)
	entry, err := s.loadReceipt(ctx, id)
	if err != nil || string(entry.intentJSON) != storedReceiptGolden {
		t.Fatalf("whole envelope changed: %s, %v", entry.intentJSON, err)
	}
	var envelope agenttools.MutationReceipt
	if err := json.Unmarshal([]byte(storedReceiptGolden), &envelope); err != nil {
		t.Fatal(err)
	}
	ordered, err := json.Marshal(envelope) // struct order differs from canonical key order
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{append([]byte(storedReceiptGolden), '\n'), ordered, []byte(strings.Repeat(" ", agenttools.MaxMutationReceiptBytes+1))} {
		if len(raw) <= agenttools.MaxMutationReceiptBytes {
			if _, err := agenttools.DecodeMutationReceipt(raw); err != nil {
				t.Fatalf("portable equivalent form refused: %v", err)
			}
		}
		checkpointSQL(t, s.db, `UPDATE mutation_receipts SET intent_json = ?`, string(raw))
		if _, err := s.loadReceipt(ctx, id); err == nil {
			t.Fatal("lookup repaired noncanonical stored envelope")
		}
		if _, err := s.scanReceipts(ctx, 0, 10); err == nil {
			t.Fatal("scan repaired noncanonical stored envelope")
		}
	}
	checkpointSQL(t, s.db, `UPDATE mutation_receipts SET intent_json = ?, mutation_id = ?`, storedReceiptGolden, strings.Repeat("B", 26))
	if _, err := s.scanReceipts(ctx, 0, 10); err == nil {
		t.Fatal("ledger ID mismatch accepted")
	}
	rec, _, intent := storeForward(t, s, strings.Repeat("C", 26), "old")
	if _, _, err := s.prepareSignedIntent(ctx, "g", testNow, rec, append(intent, '\n')); err == nil {
		t.Fatal("prepare repaired noncanonical envelope")
	}
}

func TestCheckpointStoreSignedInverseAtomicAndRecovery(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
	cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.commitSignedIntent(ctx, f, storeApplied(t, body)); err != nil {
		t.Fatal(err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatal(err)
	}
	inverse := storeInverse(body, strings.Repeat("B", 26))
	inverseRaw := storeReceiptJSON(t, inverse)
	if err := s.prepareInverseIntent(ctx, f, inverseRaw); err == nil {
		t.Fatal("inverse prepared outside undoing state")
	}
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatal(err)
	}
	if err := s.markRestored(ctx, f); err == nil {
		t.Fatal("unsigned restore accepted signed row")
	}
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_inverse_ref BEFORE UPDATE OF inverse_mutation_id ON checkpoint_files BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if err := s.prepareInverseIntent(ctx, f, inverseRaw); err == nil {
		t.Fatal("inverse ignored reference failure")
	}
	if _, err := s.loadReceipt(ctx, inverse.MutationID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("inverse leaked orphan intent: %v", err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_inverse_ref`)
	if err := s.prepareInverseIntent(ctx, f, inverseRaw); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareInverseIntent(ctx, f, inverseRaw); err == nil {
		t.Fatal("duplicate inverse identity accepted")
	}
	checkpointSQL(t, s.db, `CREATE TRIGGER fail_restored BEFORE UPDATE OF restored ON checkpoint_files BEGIN SELECT RAISE(ABORT, 'test rollback'); END`)
	if err := s.commitInverseIntent(ctx, f, storeApplied(t, inverse)); err == nil {
		t.Fatal("inverse ignored flag failure")
	}
	entry, err := s.loadReceipt(ctx, inverse.MutationID)
	if err != nil || entry.appliedJSON != nil {
		t.Fatalf("failed inverse leaked applied evidence: %+v, %v", entry, err)
	}
	checkpointSQL(t, s.db, `DROP TRIGGER fail_restored`)
	if err := s.commitInverseIntent(ctx, f, storeApplied(t, inverse)); err != nil {
		t.Fatal(err)
	}
	files, err := s.loadFileMetadata(ctx, cp, true)
	if err != nil || !files[0].restored || files[0].inverseMutationID != (sql.NullString{String: inverse.MutationID, Valid: true}) {
		t.Fatalf("inverse progress = %+v, %v", files, err)
	}
	if err := s.commitInverseIntent(ctx, f, storeApplied(t, inverse)); err == nil {
		t.Fatal("duplicate inverse commit accepted")
	}
	if err := s.deleteRestored(ctx, cp); err != nil {
		t.Fatal(err)
	}
	entries, err := s.scanReceipts(ctx, 0, 10)
	if err != nil || len(entries) != 2 || !bytes.Equal(entries[1].intentJSON, inverseRaw) || !bytes.Equal(entries[1].appliedJSON, storeApplied(t, inverse)) {
		t.Fatalf("completed undo lost ledger: %+v, %v", entries, err)
	}
}

func TestCheckpointStoreRecoveryPreservesUncertainty(t *testing.T) {
	for _, action := range []string{"drop", "keep", "already_target", "inverse_target", "completed_inverse"} {
		t.Run(action, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
			cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
			if err != nil {
				t.Fatal(err)
			}
			if action == "drop" {
				if err := s.recoverDropIntent(ctx, f); err != nil {
					t.Fatal(err)
				}
				if err := s.seal(ctx, cp); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.recoverCommitIntent(ctx, f); err != nil {
					t.Fatal(err)
				}
				if err := s.seal(ctx, cp); err != nil {
					t.Fatal(err)
				}
				if action != "keep" {
					if err := s.markUndoing(ctx, []int64{cp}); err != nil {
						t.Fatal(err)
					}
					if action == "inverse_target" || action == "completed_inverse" {
						inverse := storeInverse(body, strings.Repeat("B", 26))
						if err := s.prepareInverseIntent(ctx, f, storeReceiptJSON(t, inverse)); err != nil {
							t.Fatal(err)
						}
						if action == "completed_inverse" {
							completed := storeInverse(body, strings.Repeat("C", 26))
							checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id, intent_json, applied_json) VALUES (?, ?, ?)`, completed.MutationID, string(storeReceiptJSON(t, completed)), string(storeApplied(t, completed)))
							if err := s.reconcileInverseIntent(ctx, f, completed.MutationID); err != nil {
								t.Fatal(err)
							}
						}
					}
					if action != "completed_inverse" {
						if err := s.recoverRestored(ctx, f); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.deleteRestored(ctx, cp); err != nil {
						t.Fatal(err)
					}
				}
			}
			entries, err := s.scanReceipts(ctx, 0, 10)
			want := 1
			if action == "inverse_target" {
				want = 2
			}
			if action == "completed_inverse" {
				want = 3
			}
			if err != nil || len(entries) != want || !bytes.Equal(entries[0].intentJSON, intent) || entries[0].appliedJSON != nil {
				t.Fatalf("recovery fabricated/lost evidence: %+v, %v", entries, err)
			}
			if want > 1 && entries[1].appliedJSON != nil {
				t.Fatal("recovery fabricated inverse evidence")
			}
		})
	}
}

func TestCheckpointStoreReceiptScanSurvivesPruningAndSequenceGaps(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	s.maxCheckpoints, s.maxPriorBytes = 1, 3
	ctx := context.Background()
	for i, id := range []string{strings.Repeat("A", 26), strings.Repeat("B", 26), strings.Repeat("C", 26)} {
		rec, body, intent := storeForward(t, s, id, "old")
		if i == 1 { // Committed sequence gap from a definite abort.
			_, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.abortIntent(ctx, f); err != nil {
				t.Fatal(err)
			}
			continue
		}
		cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := s.recoverCommitIntent(ctx, f); err != nil {
				t.Fatal(err)
			}
		} else if err := s.commitSignedIntent(ctx, f, storeApplied(t, body)); err != nil {
			t.Fatal(err)
		}
		if err := s.seal(ctx, cp); err != nil {
			t.Fatal(err)
		}
	}
	if got := listIDs(t, s); len(got) != 1 {
		t.Fatalf("snapshot pruning failed: %v", got)
	}
	page, err := s.scanReceipts(ctx, 0, 1)
	if err != nil || len(page) != 1 || page[0].sequence != 1 || page[0].mutationID != strings.Repeat("A", 26) || page[0].appliedJSON != nil {
		t.Fatalf("first retained unresolved page = %+v, %v", page, err)
	}
	page, err = s.scanReceipts(ctx, page[0].sequence, 1)
	if err != nil || len(page) != 1 || page[0].sequence != 3 || page[0].mutationID != strings.Repeat("C", 26) || page[0].appliedJSON == nil {
		t.Fatalf("gapped second page = %+v, %v", page, err)
	}
	page, err = s.scanReceipts(ctx, 3, 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("scan beyond end = %+v, %v", page, err)
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := s.scanReceipts(ctx, 0, limit); err == nil {
			t.Fatalf("scan accepted unbounded limit %d", limit)
		}
	}
}

func TestCheckpointStoreSignedHardeningErrorAfterCommit(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
	cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
	if err != nil {
		t.Fatal(err)
	}
	originalPath := s.dbPath
	s.dbPath = t.TempDir()
	if err := s.commitSignedIntent(ctx, f, storeApplied(t, body)); err == nil {
		t.Fatal("hardening failure hidden")
	}
	s.dbPath = originalPath
	entry, err := s.loadReceipt(ctx, body.MutationID)
	files, ferr := s.loadFiles(ctx, cp, false)
	if err != nil || ferr != nil || entry.appliedJSON == nil || !files[0].applied {
		t.Fatalf("hardening failure lost committed facts: %+v, %+v, %v, %v", entry, files, err, ferr)
	}
}

func TestCheckpointStoreSignedInverseBindingRefusals(t *testing.T) {
	for _, field := range []string{"kind", "forward", "undo", "workspace", "path", "before", "after"} {
		t.Run(field, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			rec, body, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
			cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.recoverCommitIntent(ctx, f); err != nil {
				t.Fatal(err)
			}
			if err := s.seal(ctx, cp); err != nil {
				t.Fatal(err)
			}
			if err := s.markUndoing(ctx, []int64{cp}); err != nil {
				t.Fatal(err)
			}
			bad := storeInverse(body, strings.Repeat("B", 26))
			switch field {
			case "kind":
				bad.Kind = "applied"
			case "forward":
				bad.UndoOf = ""
			case "undo":
				bad.UndoOf = strings.Repeat("C", 26)
			case "workspace":
				bad.WorkspaceHash = strings.Repeat("0", 64)
			case "path":
				bad.Path = "other"
			case "before":
				bad.BeforeHash = strings.Repeat("0", 64)
			case "after":
				bad.AfterHash = strings.Repeat("0", 64)
			}
			if err := s.prepareInverseIntent(ctx, f, storeReceiptJSON(t, bad)); err == nil {
				t.Fatal("inverse accepted wrong lineage/transition")
			}
			entries, err := s.scanReceipts(ctx, 0, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("invalid inverse leaked ledger: %+v, %v", entries, err)
			}
		})
	}
}

func TestCheckpointStoreSignedCreatesBindExactModeAndAbsence(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprint(tracked), func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			rec, body, _ := storeForward(t, s, strings.Repeat("A", 26), "")
			rec.Existed, rec.PriorContent, rec.TrackedMode = false, nil, tracked
			body.BeforeHash = "absent"
			mode := uint32(0)
			if tracked {
				body.AfterMode = &mode
			}
			cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, storeReceiptJSON(t, body))
			if err != nil {
				t.Fatal(err)
			}
			bad := body
			bad.MutationID = strings.Repeat("B", 26)
			if tracked {
				bad.AfterMode = nil
			} else {
				bad.AfterMode = &mode
			}
			if _, _, err := s.prepareSignedIntent(ctx, "g", testNow, rec, storeReceiptJSON(t, bad)); err == nil {
				t.Fatal("prepare ignored null/0000 mode distinction")
			}
			bad = body
			if tracked {
				nonzero := uint32(0o600)
				bad.AfterMode = &nonzero
			} else {
				bad.AfterMode = &mode
			}
			if err := s.commitSignedIntent(ctx, f, storeApplied(t, bad)); err == nil {
				t.Fatal("commit ignored exact mode mismatch")
			}
			if err := s.commitSignedIntent(ctx, f, storeApplied(t, body)); err != nil {
				t.Fatal(err)
			}
			files, err := s.loadFiles(ctx, cp, true)
			if err != nil || len(files) != 1 || files[0].existed || files[0].priorHash != "" || files[0].trackedMode != tracked || files[0].afterMode != 0 {
				t.Fatalf("create round trip = %+v, %v", files, err)
			}
		})
	}
}

func TestCheckpointStoreReceiptCountPruningAndInvalidPresentEvidence(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	s.maxCheckpoints = 1
	ctx := context.Background()
	var firstIntent, firstApplied []byte
	for _, id := range []string{strings.Repeat("A", 26), strings.Repeat("B", 26)} {
		rec, body, intent := storeForward(t, s, id, "old")
		cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
		if err != nil {
			t.Fatal(err)
		}
		applied := storeApplied(t, body)
		if err := s.commitSignedIntent(ctx, f, applied); err != nil {
			t.Fatal(err)
		}
		if err := s.seal(ctx, cp); err != nil {
			t.Fatal(err)
		}
		if firstIntent == nil {
			firstIntent, firstApplied = intent, applied
		}
	}
	entries, err := s.scanReceipts(ctx, 0, 10)
	if err != nil || len(entries) != 2 || len(listIDs(t, s)) != 1 || !bytes.Equal(entries[0].intentJSON, firstIntent) || !bytes.Equal(entries[0].appliedJSON, firstApplied) {
		t.Fatalf("count pruning lost envelope: %+v, %v", entries, err)
	}
	for _, raw := range []string{"", string(firstApplied) + "\n", string(firstIntent)} {
		checkpointSQL(t, s.db, `UPDATE mutation_receipts SET applied_json = ? WHERE mutation_id = ?`, raw, strings.Repeat("A", 26))
		if _, err := s.loadReceipt(ctx, strings.Repeat("A", 26)); err == nil {
			t.Fatal("invalid present applied evidence accepted")
		}
		if _, err := s.scanReceipts(ctx, 0, 10); err == nil {
			t.Fatal("scan accepted invalid present applied evidence")
		}
	}
}

func TestCheckpointStoreSignedRecoveryRefusesMissingEvidence(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	_, f := mustPrepare(t, s, "legacy", testRec("a", "old", true))
	checkpointSQL(t, s.db, `UPDATE checkpoint_files SET forward_mutation_id = NULL WHERE id = ?`, f)
	for _, op := range []func(context.Context, int64) error{s.recoverCommitIntent, s.recoverDropIntent, s.recoverRestored} {
		if err := op(ctx, f); err == nil {
			t.Fatal("signed recovery inferred permission from legacy NULL reference")
		}
	}
}

func TestCheckpointStoreRecoveryRejectsPresentEmptyInverseReference(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	rec, _, intent := storeForward(t, s, strings.Repeat("A", 26), "old")
	cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recoverCommitIntent(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := s.seal(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := s.markUndoing(ctx, []int64{cp}); err != nil {
		t.Fatal(err)
	}
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES ('', '{}')`)
	checkpointSQL(t, s.db, `UPDATE checkpoint_files SET inverse_mutation_id = '' WHERE id = ?`, f)
	if err := s.recoverRestored(ctx, f); err == nil {
		t.Fatal("present invalid inverse treated as absent")
	}
}

func TestCheckpointStoreMetadataPreservesReferenceNullness(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	ctx := context.Background()
	cp, f := mustPrepare(t, s, "legacy", testRec("a", "old", true))
	checkpointSQL(t, s.db, `UPDATE checkpoint_files SET forward_mutation_id = NULL WHERE id = ?`, f)
	files, err := s.loadFileMetadata(ctx, cp, false)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].forwardMutationID != (sql.NullString{}) || files[0].inverseMutationID != (sql.NullString{}) {
		t.Fatalf("NULL references lost nullness: %+v", files[0])
	}
	checkpointSQL(t, s.db, `INSERT INTO mutation_receipts (mutation_id, intent_json) VALUES ('', '{}')`)
	checkpointSQL(t, s.db, `UPDATE checkpoint_files SET forward_mutation_id = '', inverse_mutation_id = '' WHERE id = ?`, f)
	files, err = s.loadFileMetadata(ctx, cp, false)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].forwardMutationID != (sql.NullString{Valid: true}) || files[0].inverseMutationID != (sql.NullString{Valid: true}) {
		t.Fatalf("present empty references became NULL: %+v", files[0])
	}
}

func TestCheckpointStoreRejectsInvalidStoredModesBeforeConversion(t *testing.T) {
	for _, mode := range []int64{-1, 0o1000, 1 << 32} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			ctx := context.Background()
			rec, body, _ := storeForward(t, s, strings.Repeat("A", 26), "")
			rec.Existed, rec.PriorContent, rec.TrackedMode = false, nil, true
			body.BeforeHash = "absent"
			zero := uint32(0)
			body.AfterMode = &zero
			cp, f, err := s.prepareSignedIntent(ctx, "g", testNow, rec, storeReceiptJSON(t, body))
			if err != nil {
				t.Fatal(err)
			}
			checkpointSQL(t, s.db, `UPDATE checkpoint_files SET after_mode = ? WHERE id = ?`, mode, f)
			if _, err := s.loadFileMetadata(ctx, cp, false); err == nil {
				t.Error("metadata silently converted invalid mode")
			}
			if err := s.commitSignedIntent(ctx, f, storeApplied(t, body)); err == nil {
				t.Error("invalid mode wrapped into matching signed bits")
			}
		})
	}
}

// testPrepareIntent/testCommitIntent are signed fixture builders, not production
// compatibility paths. Explicit migration fixtures above remain unsigned SQL.
func (s *checkpointStore) testPrepareIntent(ctx context.Context, goal string, at time.Time, rec agenttools.MutationRecord) (int64, int64, error) {
	signer, err := signing.NewHMAC(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		return 0, 0, err
	}
	before := "absent"
	if rec.Existed {
		before = agenttools.ContentHash(rec.PriorContent)
	}
	body := agenttools.MutationReceiptBody{Kind: "intent", MutationID: rand.Text(), WorkspaceHash: s.workspaceHash, Path: canonicalCheckpointPath(rec.Path), BeforeHash: before, AfterHash: rec.AfterHash, AgentID: signer.KeyID(), Timestamp: at.UTC().Format(time.RFC3339Nano)}
	if rec.TrackedMode {
		mode := uint32(rec.AfterMode)
		body.AfterMode = &mode
	}
	receipt, err := agenttools.SignMutationReceipt(ctx, signer, body)
	if err != nil {
		return 0, 0, err
	}
	raw, err := signing.MarshalCanonical(receipt)
	if err != nil {
		return 0, 0, err
	}
	return s.prepareSignedIntent(ctx, goal, at, rec, raw)
}
func (s *checkpointStore) testCommitIntent(ctx context.Context, fileID int64) error {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT forward_mutation_id FROM checkpoint_files WHERE id = ?`, fileID).Scan(&id); err != nil {
		return err
	}
	entry, err := s.loadReceipt(ctx, id)
	if err != nil {
		return err
	}
	intent, err := decodeStoredMutationReceipt(entry.intentJSON)
	if err != nil {
		return err
	}
	signer, err := signing.NewHMAC(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		return err
	}
	body := intent.Body
	body.Kind = "applied"
	receipt, err := agenttools.SignMutationReceipt(ctx, signer, body)
	if err != nil {
		return err
	}
	raw, err := signing.MarshalCanonical(receipt)
	if err != nil {
		return err
	}
	return s.commitSignedIntent(ctx, fileID, raw)
}

func testStoreGetenv(s *checkpointStore) func(string) string {
	return testGetenv(filepath.Dir(filepath.Dir(filepath.Dir(s.dbPath))))
}
