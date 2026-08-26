package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// newJournalFixture builds a checkpointJournal over a real Workspace and a
// fresh test store, plus the REAL mutating tools bound to that journal: the
// lifecycle tests drive write_file/edit_file, not synthetic Record calls.
func newJournalFixture(t *testing.T) (*checkpointJournal, []agent.Tool, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	s := openTestStore(t, root)
	j := newCheckpointJournal(ws, s)
	return j, agenttools.NewMutatingTools(ws, j), root
}

func toolByName(t *testing.T, tools []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

// applyTool runs Plan then Invoke on the named tool with args, mirroring
// dispatch, and returns the tool result (internal Go errors fail the test
// unless wantInternal).
func applyTool(t *testing.T, tools []agent.Tool, name string, args map[string]any) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	tool := toolByName(t, tools, name)
	pt, ok := tool.(agent.PlanningTool)
	if !ok {
		t.Fatalf("%s is not a PlanningTool", name)
	}
	if _, perr := pt.Plan(context.Background(), raw); perr != nil {
		t.Fatalf("%s Plan: %v", name, perr)
	}
	res, ierr := tool.Invoke(context.Background(), raw)
	if ierr != nil {
		t.Fatalf("%s Invoke internal error: %v", name, ierr)
	}
	return res
}

// beginTestTurn arms the journal with a cancellable run context and fails the
// test if the turn is refused.
func beginTestTurn(t *testing.T, j *checkpointJournal, goal string) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := j.beginTurn(context.Background(), goal, cancel); err != nil {
		t.Fatalf("beginTurn: %v", err)
	}
	return ctx, cancel
}

func mustSealTurn(t *testing.T, j *checkpointJournal) {
	t.Helper()
	if err := j.sealTurn(context.Background()); err != nil {
		t.Fatalf("sealTurn: %v", err)
	}
}

func TestCheckpointJournalGroupsRealWrites(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "one turn, three writes")
	if res := applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"}); res.IsError {
		t.Fatalf("write a.txt: %s", res.Content)
	}
	if res := applyTool(t, tools, "write_file", map[string]any{"path": "b.txt", "content": "B1\n"}); res.IsError {
		t.Fatalf("write b.txt: %s", res.Content)
	}
	if res := applyTool(t, tools, "edit_file", map[string]any{"path": "a.txt", "old_string": "A1", "new_string": "A2"}); res.IsError {
		t.Fatalf("edit a.txt: %s", res.Content)
	}
	mustSealTurn(t, j)

	infos, err := j.store.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 3 {
		t.Fatalf("infos = %+v, want one completed checkpoint with 3 files", infos)
	}
	groups, err := j.store.newestCompleted(context.Background(), 1)
	if err != nil {
		t.Fatalf("newestCompleted: %v", err)
	}
	g := groups[0]
	// Reverse mutation order: the edit of a.txt is newest.
	if g.files[0].path != "a.txt" || g.files[1].path != "b.txt" || g.files[2].path != "a.txt" {
		t.Fatalf("order = %s,%s,%s", g.files[0].path, g.files[1].path, g.files[2].path)
	}
	if string(g.files[2].priorContent) != "A0\n" || !g.files[2].existed {
		t.Fatalf("first write prior = %+v", g.files[2])
	}
	if g.files[1].existed {
		t.Fatalf("b.txt create marked existed")
	}
	if string(g.files[0].priorContent) != "A1\n" {
		t.Fatalf("edit prior = %q, want the intermediate content", g.files[0].priorContent)
	}
	for _, f := range g.files {
		if !f.applied {
			t.Fatalf("file %s not applied after commit", f.path)
		}
	}
}

func TestCheckpointJournalMutationFreeTurnLeavesNoRow(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "just chatting")
	mustSealTurn(t, j)
	infos, err := j.store.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("infos = %+v, want none for a mutation-free turn", infos)
	}
}

func TestCheckpointJournalSeparateTurnsSeparateCheckpoints(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "turn one")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	_, _ = beginTestTurn(t, j, "turn two")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A2\n"})
	mustSealTurn(t, j)

	infos, err := j.store.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 2 || infos[0].goal != "turn two" || infos[1].goal != "turn one" {
		t.Fatalf("infos = %+v", infos)
	}
}

func TestCheckpointJournalCanonicalAliasSharesChain(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "aliases")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "./a.txt", "content": "A2\n"})
	mustSealTurn(t, j)
	groups, err := j.store.newestCompleted(context.Background(), 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v %d", err, len(groups))
	}
	for _, f := range groups[0].files {
		if f.path != "a.txt" {
			t.Fatalf("stored path = %q, want canonical a.txt", f.path)
		}
	}
	if len(groups[0].files) != 2 {
		t.Fatalf("files = %d, want 2 records on one chain", len(groups[0].files))
	}
}

func TestCheckpointJournalRecordCompatibilityStillJournals(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "compat")
	j.Record(testRec("a.txt", "A0", true))
	mustSealTurn(t, j)
	groups, err := j.store.newestCompleted(context.Background(), 1)
	if err != nil || len(groups) != 1 || len(groups[0].files) != 1 || !groups[0].files[0].applied {
		t.Fatalf("Record bypassed the write-ahead store: %v %+v", err, groups)
	}
}

func TestCheckpointPrepareSerializesThroughResolution(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "serialize")

	p1, err := j.Prepare(testRec("a.txt", "A0", true))
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	done := make(chan int64, 1)
	entered := make(chan struct{})
	go func() {
		close(entered) // handshake: the goroutine is about to call Prepare
		p2, err := j.Prepare(testRec("b.txt", "B0", true))
		if err != nil {
			done <- -1
			return
		}
		if err := p2.Commit(); err != nil {
			done <- -1
			return
		}
		done <- 1
	}()
	<-entered
	// The second Prepare must block until p1 resolves. A negative assertion
	// needs a grace window: a non-serializing implementation finishes its
	// SQLite insert well inside 100ms of polling, while a correct one stays
	// blocked on the serialization slot for the entire window.
	for i := 0; i < 100; i++ {
		select {
		case <-done:
			t.Fatal("second Prepare completed before the first resolved")
		case <-time.After(time.Millisecond):
		}
	}
	if err := p1.Commit(); err != nil {
		t.Fatalf("commit p1: %v", err)
	}
	if got := <-done; got != 1 {
		t.Fatalf("second prepare/commit failed")
	}
	mustSealTurn(t, j)
	groups, err := j.store.newestCompleted(context.Background(), 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	// Reverse order: b.txt (second) first — DB order matched resolution order.
	if groups[0].files[0].path != "b.txt" || groups[0].files[1].path != "a.txt" {
		t.Fatalf("order = %s,%s", groups[0].files[0].path, groups[0].files[1].path)
	}
}

func TestCheckpointJournalPrepareFailureLatchesAndCancels(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	ctx, _ := beginTestTurn(t, j, "doomed")
	if err := j.store.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	res := applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	if !res.IsError {
		t.Fatal("want tool error when prepare fails")
	}
	if ctx.Err() == nil {
		t.Fatal("prepare failure must cancel the run context (D7)")
	}
	if err := j.sealTurn(context.Background()); err == nil {
		t.Fatal("sealTurn must surface the latched failure")
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("beginTurn must refuse while the journal is latched")
	}
}

func TestCheckpointJournalCommitFailureLatchesAndCancels(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	ctx, _ := beginTestTurn(t, j, "doomed")
	p, err := j.Prepare(testRec("a.txt", "A0", true))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := j.store.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := p.Commit(); err == nil {
		t.Fatal("want commit error on a dead store")
	}
	if ctx.Err() == nil {
		t.Fatal("commit failure must cancel the run context (D7)")
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("beginTurn must refuse while latched")
	}
}

func TestCheckpointJournalSealFailureLatches(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	if err := j.store.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := j.sealTurn(context.Background()); err == nil {
		t.Fatal("want seal failure on a dead store")
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("beginTurn must refuse after a seal failure")
	}
}

func TestCheckpointJournalHardeningFailureLatches(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	ctx, _ := beginTestTurn(t, j, "turn")
	// Point the store's hardening target at a directory: SecureDBFiles then
	// fails on every mutation, and per the spec a hardening failure is an
	// operation failure.
	j.store.dbPath = t.TempDir()
	res := applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	if !res.IsError {
		t.Fatal("want tool error when hardening fails")
	}
	if ctx.Err() == nil {
		t.Fatal("hardening failure must cancel the run context")
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("beginTurn must refuse after a hardening failure")
	}
}

func TestCheckpointQuotaCancelsTurnWithoutPoisoningJournal(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	j.store.maxPriorBytes = 100
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, _ := beginTestTurn(t, j, "quota turn")
	if res := applyTool(t, tools, "write_file", map[string]any{"path": "small.txt", "content": "s\n"}); res.IsError {
		t.Fatalf("small write: %s", res.Content)
	}
	// Overwriting big.txt needs 200 prior bytes; the cap is 100 and nothing
	// is prunable -> expected policy refusal.
	res := applyTool(t, tools, "write_file", map[string]any{"path": "big.txt", "content": "new\n"})
	if !res.IsError {
		t.Fatal("want quota refusal")
	}
	if ctx.Err() == nil {
		t.Fatal("quota refusal must cancel the current turn")
	}
	// big.txt untouched: the refusal happened before the workspace write.
	got, err := os.ReadFile(filepath.Join(root, "big.txt"))
	if err != nil || len(got) != 200 {
		t.Fatalf("big.txt = %d bytes,%v want untouched 200", len(got), err)
	}
	serr := j.sealTurn(context.Background())
	if serr == nil || !errors.Is(serr, errCheckpointQuota) {
		t.Fatalf("sealTurn = %v, want the quota refusal surfaced", serr)
	}
	// The applied small write is sealed and the journal is NOT poisoned.
	infos, err := j.store.list(context.Background())
	if err != nil || len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 1 {
		t.Fatalf("infos = %+v, %v — want the applied work sealed", infos, err)
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err != nil {
		t.Fatalf("beginTurn after quota refusal must be allowed: %v", err)
	}
}

func TestCheckpointJournalBeginTurnRefusedWhileUndoing(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	groups, err := j.store.newestCompleted(context.Background(), 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	if err := j.store.markUndoing(context.Background(), []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	err = j.beginTurn(context.Background(), "blocked", func() {})
	if !errors.Is(err, errInterruptedUndoPending) {
		t.Fatalf("beginTurn = %v, want errInterruptedUndoPending", err)
	}
}

func TestCheckpointRecoveryForwardWindows(t *testing.T) {
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	s := openTestStore(t, root)
	ctx := context.Background()

	// Stage one crashed turn with three windows:
	// a.txt: prepared, write never landed (live == prior).
	// b.txt: prepared create, rename landed (live == new content), no commit.
	// c.txt: prepared + committed, seal never ran.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareIntent(ctx, "crashed", testNow, testRec("a.txt", "A0", true)); err != nil {
		t.Fatalf("prepare a: %v", err)
	}
	if _, _, err := s.prepareIntent(ctx, "crashed", testNow, testRec("b.txt", "", false)); err != nil {
		t.Fatalf("prepare b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("B1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fc, err := s.prepareIntent(ctx, "crashed", testNow, testRec("c.txt", "", false))
	if err != nil {
		t.Fatalf("prepare c: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("C1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.commitIntent(ctx, fc); err != nil {
		t.Fatalf("commit c: %v", err)
	}

	j := newCheckpointJournal(ws, s)
	notice, err := j.recoverStartup(ctx)
	if err != nil {
		t.Fatalf("recoverStartup: %v", err)
	}
	if notice == "" {
		t.Fatal("want a startup notice for a recovered checkpoint")
	}
	groups, err := s.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v %d", err, len(groups))
	}
	var paths []string
	for _, f := range groups[0].files {
		paths = append(paths, f.path)
		if !f.applied {
			t.Errorf("%s not applied after recovery", f.path)
		}
	}
	// a.txt's never-landed intent is dropped; b and c survive as applied.
	if len(paths) != 2 || paths[0] != "c.txt" || paths[1] != "b.txt" {
		t.Fatalf("recovered rows = %v, want [c.txt b.txt]", paths)
	}
}

func TestCheckpointRecoveryDropsAllNeverLandedIntents(t *testing.T) {
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	s := openTestStore(t, root)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareIntent(ctx, "crashed", testNow, testRec("a.txt", "A0", true)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	j := newCheckpointJournal(ws, s)
	notice, err := j.recoverStartup(ctx)
	if err != nil {
		t.Fatalf("recoverStartup: %v", err)
	}
	if notice != "" {
		t.Fatalf("notice = %q, want none when the recovered checkpoint is empty", notice)
	}
	if ids := listIDs(t, s); len(ids) != 0 {
		t.Fatalf("ids = %v, want the empty recovered checkpoint deleted", ids)
	}
}

func TestCheckpointRecoveryReadErrorFailsStartup(t *testing.T) {
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	s := openTestStore(t, root)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareIntent(ctx, "crashed", testNow, testRec("a.txt", "A0", true)); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Replace the file with a symlink: classification must refuse to guess.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	j := newCheckpointJournal(ws, s)
	if _, err := j.recoverStartup(ctx); err == nil {
		t.Fatal("recoverStartup must fail on an unclassifiable path")
	}
	// The row stays open for a later, informed decision.
	groups, gerr := s.loadGroups(ctx, checkpointOpen, 0, false)
	if gerr != nil || len(groups) != 1 || len(groups[0].files) != 1 {
		t.Fatalf("open groups after failed recovery = %+v, %v", groups, gerr)
	}
}
