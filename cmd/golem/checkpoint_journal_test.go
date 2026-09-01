package main

import (
	"bytes"
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

func TestCheckpointMarkRestoredHardeningFailureLatches(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)

	ctx := context.Background()
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v, %d group(s)", err, len(groups))
	}
	group := groups[0]
	if err := j.store.markUndoing(ctx, []int64{group.id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}

	dbPath := j.store.dbPath
	j.store.dbPath = t.TempDir()
	var out bytes.Buffer
	if j.restoreFile(ctx, &out, group.files[0]) {
		t.Fatal("restoreFile reported success despite hardening failure")
	}
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A0\n" {
		t.Fatalf("a.txt = %q,%v, want restored A0", got, ok)
	}

	// markRestored committed before hardening failed. Clear the completed undo
	// so beginTurn can only refuse because the journal latched the failure.
	j.store.dbPath = dbPath
	if err := j.store.deleteRestored(ctx, group.id); err != nil {
		t.Fatalf("deleteRestored cleanup: %v", err)
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil ||
		!strings.Contains(err.Error(), "checkpoint journal disabled") {
		t.Fatalf("beginTurn = %v, want refusal from latched markRestored failure", err)
	}
}

func TestCheckpointDeleteRestoredHardeningFailureLatches(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	ctx := context.Background()
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v, %d group(s)", err, len(groups))
	}
	if err := j.store.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	for _, f := range groups[0].files {
		if err := j.store.markRestored(ctx, f.id); err != nil {
			t.Fatalf("markRestored: %v", err)
		}
	}
	groups, err = j.store.undoingGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("undoingGroups: %v, %d group(s)", err, len(groups))
	}

	j.store.dbPath = t.TempDir()
	var out bytes.Buffer
	if j.restoreGroups(ctx, &out, groups) {
		t.Fatal("restoreGroups reported success despite hardening failure")
	}
	if !strings.Contains(out.String(), "undo failed") {
		t.Fatalf("output = %q, want undo failure", out.String())
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Fatalf("delete did not commit before hardening failed: %v", ids)
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("post-delete hardening failure must latch")
	}
}

func TestCheckpointQuotaWithHardeningFailureLatches(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	j.store.maxPriorBytes = 100
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, _ := beginTestTurn(t, j, "quota and hardening")
	j.store.dbPath = t.TempDir()
	res := applyTool(t, tools, "write_file", map[string]any{"path": "big.txt", "content": "new\n"})
	if !res.IsError {
		t.Fatal("want quota refusal")
	}
	if ctx.Err() == nil {
		t.Fatal("failure must cancel the current turn")
	}
	if err := j.sealTurn(context.Background()); !errors.Is(err, errCheckpointQuota) {
		t.Fatalf("sealTurn = %v, want quota error retained", err)
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err == nil {
		t.Fatal("combined quota and hardening failure must latch")
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

func TestCheckpointJournalCanceledBeginTurnDoesNotLatch(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := j.beginTurn(ctx, "canceled", func() {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("beginTurn = %v, want context.Canceled", err)
	}
	if err := j.beginTurn(context.Background(), "next", func() {}); err != nil {
		t.Fatalf("fresh turn refused after transient cancellation: %v", err)
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

func TestCheckpointRecoveryPathErrorIsControlSafe(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	path := "safe.txt\nforged: recovery succeeded\x1b[31m"
	ctx := context.Background()
	if _, _, err := j.store.prepareIntent(ctx, "crashed", testNow, testRec(path, "A0", true)); err != nil {
		t.Fatalf("prepareIntent: %v", err)
	}
	j.ws.SetScopeGuard(func(rel string, _ bool) error {
		return &os.PathError{Op: "open", Path: rel, Err: os.ErrPermission}
	})

	_, err := j.recoverStartup(ctx)
	if err == nil {
		t.Fatal("recoverStartup succeeded despite the path error")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("recoverStartup error = %v, want wrapped permission error", err)
	}
	got := err.Error()
	if strings.Contains(got, "\nforged:") || strings.Contains(got, "\x1b") {
		t.Fatalf("recovery error contains a forged line or ANSI escape: %q", got)
	}
	if want := "safe.txt\\nforged: recovery succeeded\\x1b[31m"; !strings.Contains(got, want) {
		t.Fatalf("recoverStartup error = %q, want control-safe path %q", got, want)
	}
}

// runUndo executes /undo's engine directly, returning its printed output.
func runUndo(t *testing.T, j *checkpointJournal, n int) string {
	t.Helper()
	var out bytes.Buffer
	j.undo(context.Background(), &out, n)
	return out.String()
}

// readWorkspace returns file bytes, or nil,false when absent.
func readWorkspace(t *testing.T, root, rel string) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b, true
}

func TestCheckpointUndoMultiFileTurn(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "b.txt", "content": "B1\n"})
	mustSealTurn(t, j)

	out := runUndo(t, j, 1)
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A0\n" {
		t.Errorf("a.txt = %q,%v, want A0 restored; output: %s", got, ok, out)
	}
	if _, ok := readWorkspace(t, root, "b.txt"); ok {
		t.Errorf("b.txt still exists, want the created file removed")
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints after undo = %v, want none", ids)
	}
}

func TestCheckpointUndoPathOutputIsControlSafe(t *testing.T) {
	j, tools, _ := newJournalFixture(t)
	path := "safe.txt\nforged: undid checkpoint\x1b[31m"
	_, _ = beginTestTurn(t, j, "turn")
	if res := applyTool(t, tools, "write_file", map[string]any{"path": path, "content": "A1\n"}); res.IsError {
		t.Fatalf("write malicious path: %s", res.Content)
	}
	mustSealTurn(t, j)

	out := runUndo(t, j, 1)
	if strings.Contains(out, "\nforged:") || strings.Contains(out, "\x1b") {
		t.Fatalf("undo output contains a forged line or ANSI escape: %q", out)
	}
	if want := "undid safe.txt\\nforged: undid checkpoint\\x1b[31m\n"; !strings.Contains(out, want) {
		t.Fatalf("undo output = %q, want control-safe path line %q", out, want)
	}
}

func TestCheckpointUndoDoubleWrite(t *testing.T) {
	// Same path written twice in ONE turn: only reverse mutation order can
	// unwind it — forward order hits a hash mismatch on the older record.
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "double write")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A2\n"})
	mustSealTurn(t, j)

	runUndo(t, j, 1)
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A0\n" {
		t.Errorf("a.txt = %q,%v, want A0", got, ok)
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none", ids)
	}
}

func TestCheckpointUndoMultiTurnChain(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "t1")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	_, _ = beginTestTurn(t, j, "t2")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A2\n"})
	mustSealTurn(t, j)

	runUndo(t, j, 2)
	if _, ok := readWorkspace(t, root, "a.txt"); ok {
		t.Errorf("a.txt exists, want absent after unwinding both turns")
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none", ids)
	}
}

func TestCheckpointUndoCanonicalPathAliases(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "t1")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	_, _ = beginTestTurn(t, j, "t2")
	applyTool(t, tools, "write_file", map[string]any{"path": "./a.txt", "content": "A2\n"})
	mustSealTurn(t, j)

	runUndo(t, j, 2)
	if _, ok := readWorkspace(t, root, "a.txt"); ok {
		t.Errorf("a.txt exists, want absent (aliases must share one chain)")
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none", ids)
	}
}

func TestCheckpointUndoCaseAliasesShareChain(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	probe := filepath.Join(root, "CaseProbe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeInfo, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(filepath.Join(root, "caseprobe"))
	if err != nil || !os.SameFile(probeInfo, aliasInfo) {
		t.Skip("filesystem is case-sensitive")
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	_, _ = beginTestTurn(t, j, "t1")
	applyTool(t, tools, "write_file", map[string]any{"path": "Case.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	_, _ = beginTestTurn(t, j, "t2")
	applyTool(t, tools, "write_file", map[string]any{"path": "case.txt", "content": "A2\n"})
	mustSealTurn(t, j)

	out := runUndo(t, j, 2)
	if strings.Contains(out, "cannot undo") {
		t.Fatalf("case aliases split the undo chain: %s", out)
	}
	if _, ok := readWorkspace(t, root, "Case.txt"); ok {
		t.Error("Case.txt exists, want absent after unwinding both turns")
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none", ids)
	}
}

func TestCheckpointUndoCreatedFileAlreadyAbsent(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "create")
	applyTool(t, tools, "write_file", map[string]any{"path": "gone.txt", "content": "G1\n"})
	mustSealTurn(t, j)
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	runUndo(t, j, 1)
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none (already-absent create is the desired end state)", ids)
	}
}

func TestCheckpointUndoRefusesWhenTooFew(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "only")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)

	out := runUndo(t, j, 3)
	if !strings.Contains(out, "cannot undo 3: only 1 checkpoint(s) available") {
		t.Errorf("output = %q", out)
	}
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A1\n" {
		t.Errorf("a.txt = %q,%v, want untouched", got, ok)
	}
}

func TestCheckpointUndoNothingToUndo(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	if out := runUndo(t, j, 1); !strings.Contains(out, "nothing to undo") {
		t.Errorf("output = %q", out)
	}
}

func TestCheckpointUndoDivergenceZeroPartialWrites(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	// b.txt pre-exists so its record is a MODIFY: absence, symlink, and
	// directory replacement are then divergences, not the tolerated
	// created-file-already-absent case.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("B0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "b.txt", "content": "B1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "c.txt", "content": "C1\n"})
	mustSealTurn(t, j)

	divergences := []struct {
		name   string
		mutate func(t *testing.T)
		bState string // expected b.txt content after refused undo ("" = absent)
	}{
		{"rewritten", func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("B-ext\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "B-ext\n"},
		{"removed", func(t *testing.T) {
			if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"symlink", func(t *testing.T) {
			_ = os.Remove(filepath.Join(root, "b.txt"))
			if err := os.Symlink("/etc/hosts", filepath.Join(root, "b.txt")); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"directory", func(t *testing.T) {
			_ = os.Remove(filepath.Join(root, "b.txt"))
			if err := os.Mkdir(filepath.Join(root, "b.txt"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, ""},
	}
	for _, d := range divergences {
		t.Run(d.name, func(t *testing.T) {
			d.mutate(t)
			out := runUndo(t, j, 1)
			if !strings.Contains(out, "cannot undo b.txt: file changed since golem wrote it") {
				t.Errorf("output = %q, want the exact refusal for b.txt", out)
			}
			// ZERO partial writes on every divergence class.
			if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A1\n" {
				t.Errorf("a.txt = %q,%v, want untouched A1", got, ok)
			}
			if got, ok := readWorkspace(t, root, "c.txt"); !ok || string(got) != "C1\n" {
				t.Errorf("c.txt = %q,%v, want untouched C1", got, ok)
			}
			infos, err := j.store.list(context.Background())
			if err != nil || len(infos) != 1 || infos[0].state != checkpointCompleted {
				t.Errorf("infos = %+v, %v — records must stay completed", infos, err)
			}
			// wantB documents the staged state; symlink/dir classes leave
			// non-regular entries readWorkspace cannot read, so only check
			// the regular-file classes.
			if d.name == "rewritten" {
				if got, _ := readWorkspace(t, root, "b.txt"); string(got) != d.bState {
					t.Errorf("b.txt = %q, want %q untouched", got, d.bState)
				}
			}
			// Restore b.txt to the recorded after state for the next class.
			_ = os.RemoveAll(filepath.Join(root, "b.txt"))
			if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("B1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointUndoResumeSkipsRestoredEvenAfterUserEdit(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	applyTool(t, tools, "write_file", map[string]any{"path": "b.txt", "content": "B1\n"})
	mustSealTurn(t, j)
	ctx := context.Background()

	// Simulate a crash mid-undo: intent committed, the newest file (b.txt in
	// reverse order) restored on disk AND flagged, a.txt untouched.
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	if err := j.store.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	b := groups[0].files[0]
	if b.path != "b.txt" {
		t.Fatalf("reverse order: first file = %s, want b.txt", b.path)
	}
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := j.store.markRestored(ctx, b.id); err != nil {
		t.Fatalf("markRestored: %v", err)
	}
	// The user recreates b.txt after the crash: a restored row must be
	// skipped regardless of the file's current state.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("B-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runUndo(t, j, 1)
	if !strings.Contains(out, "resumed interrupted undo") {
		t.Errorf("output = %q, want resume message", out)
	}
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A0\n" {
		t.Errorf("a.txt = %q,%v, want A0 restored by resume", got, ok)
	}
	if got, ok := readWorkspace(t, root, "b.txt"); !ok || string(got) != "B-user\n" {
		t.Errorf("b.txt = %q,%v, want the user's post-crash content untouched", got, ok)
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none after completed resume", ids)
	}
}

func TestCheckpointUndoResumeIdempotentAfterContentRestore(t *testing.T) {
	// Crash BETWEEN the content restore and the progress flag: live state is
	// already the target; resume must mark, not rewrite or refuse.
	j, tools, root := newJournalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	ctx := context.Background()
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	if err := j.store.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runUndo(t, j, 1)
	if !strings.Contains(out, "resumed interrupted undo") {
		t.Errorf("output = %q", out)
	}
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A0\n" {
		t.Errorf("a.txt = %q,%v, want A0", got, ok)
	}
	if ids := listIDs(t, j.store); len(ids) != 0 {
		t.Errorf("checkpoints = %v, want none", ids)
	}
}

func TestCheckpointUndoInterruptedTakesPriorityOverN(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "t1")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	_, _ = beginTestTurn(t, j, "t2")
	applyTool(t, tools, "write_file", map[string]any{"path": "b.txt", "content": "B1\n"})
	mustSealTurn(t, j)
	ctx := context.Background()
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	if err := j.store.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}

	out := runUndo(t, j, 2) // n=2 must NOT touch t1 while a resume is pending
	if !strings.Contains(out, "resumed interrupted undo") {
		t.Errorf("output = %q", out)
	}
	if got, ok := readWorkspace(t, root, "a.txt"); !ok || string(got) != "A1\n" {
		t.Errorf("a.txt = %q,%v — /undo 2 touched t1 during a resume", got, ok)
	}
	if _, ok := readWorkspace(t, root, "b.txt"); ok {
		t.Errorf("b.txt exists, want the resume to have removed it")
	}
}

func TestCheckpointUndoResumeDivergenceRetainsRecord(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	_, _ = beginTestTurn(t, j, "turn")
	applyTool(t, tools, "write_file", map[string]any{"path": "a.txt", "content": "A1\n"})
	mustSealTurn(t, j)
	ctx := context.Background()
	groups, err := j.store.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v", err)
	}
	if err := j.store.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A-ext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runUndo(t, j, 1)
	if !strings.Contains(out, "cannot undo a.txt: file changed since golem wrote it") {
		t.Errorf("output = %q", out)
	}
	infos, err := j.store.list(ctx)
	if err != nil || len(infos) != 1 || infos[0].state != checkpointUndoing {
		t.Errorf("infos = %+v, %v — the undoing record must be retained", infos, err)
	}
	if got, _ := readWorkspace(t, root, "a.txt"); string(got) != "A-ext\n" {
		t.Errorf("a.txt = %q, want the divergent content untouched", got)
	}
}

// --- #443 Task 8: tracked-mode guard on durable undo ---

// prepareTrackedCreate simulates a promotion inside the open turn: the file
// lands with the recorded mode and the journal sees a tracked create.
func prepareTrackedCreate(t *testing.T, j *checkpointJournal, root, rel, content string, mode os.FileMode) {
	t.Helper()
	rec := agenttools.MutationRecord{
		Path:        rel,
		Existed:     false,
		AfterHash:   agenttools.ContentHash([]byte(content)),
		Summary:     "promote " + rel,
		At:          time.Now(),
		TrackedMode: true,
		AfterMode:   mode,
	}
	prepared, err := j.Prepare(rec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	p := filepath.Join(root, rel)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestCheckpointUndoTrackedModeGuard(t *testing.T) {
	j, _, root := newJournalFixture(t)
	beginTestTurn(t, j, "promote artifact")
	prepareTrackedCreate(t, j, root, "promoted.txt", "artifact", 0o640)
	mustSealTurn(t, j)

	p := filepath.Join(root, "promoted.txt")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	j.undo(context.Background(), &out, 1)
	if !strings.Contains(out.String(), "cannot undo") {
		t.Fatalf("mode drift with identical bytes must refuse before any change: %q", out.String())
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("refused undo must not delete the file")
	}

	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	j.undo(context.Background(), &out, 1)
	if !strings.Contains(out.String(), "undid checkpoint") {
		t.Fatalf("exact mode must permit undo: %q", out.String())
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("undo must delete the promoted create, err=%v", err)
	}
}

// TestCheckpointUndoTrackedModeChain covers the simulation case: a tracked
// create in one turn, then a legacy write_file update of the same path in a
// later turn. WriteFileAtomic preserves permission bits, so the simulated
// state after undoing the legacy update carries the live mode, and the
// tracked create's guard must accept it end to end.
func TestCheckpointUndoTrackedModeChain(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	beginTestTurn(t, j, "promote artifact")
	prepareTrackedCreate(t, j, root, "chain.txt", "first", 0o640)
	mustSealTurn(t, j)

	beginTestTurn(t, j, "legacy update")
	res := applyTool(t, tools, "write_file", map[string]any{"path": "chain.txt", "content": "second"})
	if res.IsError {
		t.Fatalf("legacy update failed: %s", res.Content)
	}
	mustSealTurn(t, j)

	fi, err := os.Lstat(filepath.Join(root, "chain.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("fixture assumption broken: WriteFileAtomic changed the mode to %v", fi.Mode().Perm())
	}

	var out strings.Builder
	j.undo(context.Background(), &out, 2)
	if strings.Contains(out.String(), "cannot undo") {
		t.Fatalf("chain undo must accept the carried mode: %q", out.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "chain.txt")); !os.IsNotExist(err) {
		t.Fatalf("chain undo must remove the created file, err=%v", err)
	}
}

func TestCheckpointUndoLegacyRecordsIgnoreMode(t *testing.T) {
	j, tools, root := newJournalFixture(t)
	beginTestTurn(t, j, "legacy create")
	res := applyTool(t, tools, "write_file", map[string]any{"path": "legacy.txt", "content": "bytes"})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	mustSealTurn(t, j)
	if err := os.Chmod(filepath.Join(root, "legacy.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	j.undo(context.Background(), &out, 1)
	if strings.Contains(out.String(), "cannot undo") {
		t.Fatalf("legacy records must keep byte-hash-only semantics: %q", out.String())
	}
}
