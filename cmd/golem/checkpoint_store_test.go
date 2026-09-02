package main

import (
	"context"
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

// testRec builds a MutationRecord for store tests. AfterHash is synthetic:
// the store persists, it never validates hashes.
func testRec(path, prior string, existed bool) agenttools.MutationRecord {
	var pc []byte
	if existed {
		pc = []byte(prior)
	}
	return agenttools.MutationRecord{
		Path: path, PriorContent: pc, Existed: existed,
		AfterHash: "after-" + path, Summary: "write " + path, At: testNow,
	}
}

// mustPrepare inserts one intent, failing the test on error.
func mustPrepare(t *testing.T, s *checkpointStore, goal string, rec agenttools.MutationRecord) (int64, int64) {
	t.Helper()
	cpID, fileID, err := s.prepareIntent(context.Background(), goal, testNow, rec)
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
	cpID, fileID, err := s.prepareIntent(ctx, goal, testNow, rec)
	if err != nil {
		t.Fatalf("prepareIntent: %v", err)
	}
	if err := s.commitIntent(ctx, fileID); err != nil {
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
	if err := s.commitIntent(ctx, f1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.commitIntent(ctx, f1); err == nil {
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
	if err := s.commitIntent(ctx, f2); err == nil {
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
	if err := s.commitIntent(ctx, f1); err != nil {
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
	if err := s.commitIntent(ctx, f1); err != nil {
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
	if err := s.commitIntent(ctx, f1); err != nil {
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
	if err := s.commitIntent(ctx, f1); err != nil {
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
	cp3, f3, err := s.prepareIntent(ctx, "three", testNow, testRec("c.txt", strings.Repeat("x", 100), true))
	if err != nil {
		t.Fatalf("prepareIntent: %v", err)
	}
	got := listIDs(t, s)
	want := []int64{cp3, second}
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v (oldest completed pruned at admission)", got, want)
	}
	if err := s.commitIntent(ctx, f3); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := s.seal(ctx, cp3); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// An intent that cannot fit even after pruning every completed checkpoint
	// is refused, and the rollback keeps existing history intact.
	_, _, err = s.prepareIntent(ctx, "huge", testNow, testRec("d.txt", strings.Repeat("x", 300), true))
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
	cpN, _, err := s2.prepareIntent(ctx, "new", testNow, testRec("n.txt", strings.Repeat("x", 100), true))
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
	_, _, err := s.prepareIntent(ctx, "pressure", testNow, testRec("p.txt", strings.Repeat("z", 100), true))
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
	cp, f1, err := s.prepareIntent(ctx, "g", testNow, testRec("a.go", "A0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertPrivate("prepareIntent")
	loosen()
	if err := s.commitIntent(ctx, f1); err != nil {
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
		cp, f, err := s.prepareIntent(context.Background(), c.in, testNow, testRec(fmt.Sprintf("f%d.txt", i), "p", true))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := s.commitIntent(context.Background(), f); err != nil {
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
	cp, f, err := s.prepareIntent(ctx, "g", testNow, testRec("./x/../a.go", "A0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := s.commitIntent(ctx, f); err != nil {
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
		if err := s.commitIntent(ctx, f); err != nil {
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
	if string(f.priorContent) != "A0" || !f.existed || f.afterHash != "after-a.go" ||
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
	if version != 2 {
		t.Fatalf("fresh store version = %d, want 2", version)
	}
	cpID, _, err := s.prepareIntent(ctx, "goal", time.Now(), agenttools.MutationRecord{
		Path: "promoted.txt", AfterHash: "abc", TrackedMode: true, AfterMode: 0o640, At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareIntent(ctx, "goal", time.Now(), agenttools.MutationRecord{
		Path: "legacy.txt", AfterHash: "def", At: time.Now(),
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
		t.Fatalf("v1 -> v2 migration failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("migrated version = %d, want 2", version)
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
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openCheckpointStore(ctx, getenv, root); err == nil {
		t.Fatal("a newer schema must fail closed")
	}
}
