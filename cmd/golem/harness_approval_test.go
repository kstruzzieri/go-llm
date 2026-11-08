package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// harness_approval_test.go pins #725 in Golem's approvers: a write_file or
// edit_file to an agent instruction or harness configuration file plans with
// an empty key, so the REPL prompts with a notice and remembers nothing,
// while headless -allow-tool and Agentflow -approve-plan-edits deny it.
// Every "not written" claim pre-creates the parent and asserts Denied.

const harnessNoticeLine = "harness file: " + agenttools.HarnessApprovalNotice

func writeFixtureFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirFixture(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func requireDenied(t *testing.T, res agent.Result, i int) {
	t.Helper()
	if rec := res.ToolCalls[i]; !rec.Denied || rec.Invoked || rec.AutoApproved {
		t.Fatalf("record %d = %+v, want an approver denial", i, rec)
	}
}

func requireInvoked(t *testing.T, res agent.Result, i int, viaGrant bool) {
	t.Helper()
	if rec := res.ToolCalls[i]; !rec.Invoked || rec.Denied || rec.IsError || rec.AutoApproved != viaGrant {
		t.Fatalf("record %d = %+v, want invoked with AutoApproved=%v", i, rec, viaGrant)
	}
}

// TestHarnessApprovalPromptsDespiteFileGrant: with /auto-edits on, every
// harness call prompts with the notice and no "a" legend, "a" denies, "y"
// approves once without a grant, and an ordinary write still rides the grant.
func TestHarnessApprovalPromptsDespiteFileGrant(t *testing.T) {
	f := newFixture(t, fixtureOpts{autoEdits: true})
	writeFixtureFile(t, f.Root, ".claude/settings.json", "{}\n")
	writeFixtureFile(t, f.Root, "models.json", "{}\n")

	res := f.run(t, []agent.ModelResult{
		call("write_file", map[string]string{"path": "CLAUDE.md", "content": "pwned\n"}),
		call("edit_file", map[string]string{"path": ".claude/settings.json", "old_string": "{}", "new_string": `{"a":1}`}),
		call("write_file", map[string]string{"path": "models.json", "content": "{\"b\":1}\n"}),
		call("write_file", map[string]string{"path": "notes.txt", "content": "n\n"}),
	}, "a\ny\ny\n")

	requireRecords(t, res, 4)
	out := f.Out.String()
	if got := f.prompts(); got != 3 {
		t.Fatalf("prompts = %d, want 3:\n%s", got, out)
	}
	if got := strings.Count(out, harnessNoticeLine); got != 3 {
		t.Fatalf("notice lines = %d, want 3:\n%s", got, out)
	}
	if strings.Contains(out, "a=all edits this session") {
		t.Fatalf("a harness prompt offered the grant legend:\n%s", out)
	}
	requireDenied(t, res, 0)
	if exists(t, filepath.Join(f.Root, "CLAUDE.md")) {
		t.Fatal("CLAUDE.md written after 'a'")
	}
	requireInvoked(t, res, 1, false)
	requireInvoked(t, res, 2, false)
	requireInvoked(t, res, 3, true)
	if got := readFile(t, filepath.Join(f.Root, ".claude/settings.json")); got != "{\"a\":1}\n" {
		t.Fatalf(".claude/settings.json = %q", got)
	}
	if got := readFile(t, filepath.Join(f.Root, "notes.txt")); got != "n\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	if got := f.Grants.count(); got != 1 {
		t.Fatalf("grants = %d, want only the seeded /auto-edits grant", got)
	}
}

// TestHarnessApprovalEmptyStoreLeavesNoGrant: from an empty store, neither
// "y" nor "a" on a harness write leaves a grant, so the next ordinary write
// still prompts (the store is a map, so a pre-held grant would hide one).
func TestHarnessApprovalEmptyStoreLeavesNoGrant(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	res := f.run(t, []agent.ModelResult{
		call("write_file", map[string]string{"path": "AGENTS.md", "content": "a\n"}),
		call("write_file", map[string]string{"path": "notes.txt", "content": "n\n"}),
	}, "y\nN\n")
	requireRecords(t, res, 2)
	requireInvoked(t, res, 0, false)
	requireDenied(t, res, 1)
	if exists(t, filepath.Join(f.Root, "notes.txt")) {
		t.Fatal("notes.txt written after N")
	}
	if got := f.prompts(); got != 2 {
		t.Fatalf("prompts = %d, want 2:\n%s", got, f.Out.String())
	}
	if got := f.Grants.count(); got != 0 {
		t.Fatalf("grants after harness y = %d, want 0", got)
	}

	f = newFixture(t, fixtureOpts{})
	res = f.run(t, []agent.ModelResult{
		call("write_file", map[string]string{"path": "AGENTS.md", "content": "a\n"}),
	}, "a\n")
	requireRecords(t, res, 1)
	requireDenied(t, res, 0)
	if exists(t, filepath.Join(f.Root, "AGENTS.md")) {
		t.Fatal("AGENTS.md written after 'a'")
	}
	if got := f.Grants.count(); got != 0 {
		t.Fatalf("grants after harness a = %d, want 0", got)
	}
}

// TestHarnessApprovalRepositoryPrefix: a root inside a repository's .claude
// still classifies its files as harness files, while a worktree rooted below
// .claude (its own .git file) writes ordinary files through the grant.
func TestHarnessApprovalRepositoryPrefix(t *testing.T) {
	tmp := t.TempDir()
	mkdirFixture(t, tmp, "repo/.git")

	f := newFixtureAt(t, filepath.Join(tmp, "repo", ".claude"), fixtureOpts{autoEdits: true})
	res := f.run(t, []agent.ModelResult{
		call("write_file", map[string]string{"path": "settings.json", "content": "{}\n"}),
	}, "N\n")
	requireRecords(t, res, 1)
	requireDenied(t, res, 0)
	if got := f.prompts(); got != 1 {
		t.Fatalf("prompts = %d, want 1:\n%s", got, f.Out.String())
	}
	if exists(t, filepath.Join(f.Root, "settings.json")) {
		t.Fatal("settings.json written after N")
	}

	wt := filepath.Join(tmp, "repo", ".claude", "worktrees", "w")
	writeFixtureFile(t, wt, ".git", "gitdir: ../../../.git/worktrees/w\n")
	f = newFixtureAt(t, wt, fixtureOpts{autoEdits: true})
	res = f.run(t, []agent.ModelResult{
		call("write_file", map[string]string{"path": "notes.txt", "content": "n\n"}),
	}, "")
	requireRecords(t, res, 1)
	requireInvoked(t, res, 0, true)
	if got := f.prompts(); got != 0 {
		t.Fatalf("prompts = %d, want 0:\n%s", got, f.Out.String())
	}
}

// TestHarnessApprovalHeadlessDenies: -allow-tool write_file/edit_file approves
// ordinary calls and denies harness calls with one stderr line each; the
// scope exclusion keeps stop_command approvable with its empty key.
func TestHarnessApprovalHeadlessDenies(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mkdirFixture(t, f.Root, ".claude")
	writeFixtureFile(t, f.Root, "AGENTS.md", "keep\n")
	writeFixtureFile(t, f.Root, "notes2.txt", "old\n")

	var buf strings.Builder
	ap := newHeadlessApprover(allowToolSet{names: map[string]struct{}{"write_file": {}, "edit_file": {}}}).withNotices(&buf)
	caller := &scriptCaller{responses: []agent.ModelResult{
		call("write_file", map[string]string{"path": ".claude/settings.json", "content": "{}\n"}),
		call("edit_file", map[string]string{"path": "AGENTS.md", "old_string": "keep", "new_string": "gone"}),
		call("write_file", map[string]string{"path": "notes.txt", "content": "n\n"}),
		call("edit_file", map[string]string{"path": "notes2.txt", "old_string": "old", "new_string": "new"}),
		answerStep("done"),
	}}
	res, err := f.runWith(t, caller, ap, 5)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	requireRecords(t, res, 4)
	requireDenied(t, res, 0)
	requireDenied(t, res, 1)
	requireInvoked(t, res, 2, false)
	requireInvoked(t, res, 3, false)
	if exists(t, filepath.Join(f.Root, ".claude/settings.json")) {
		t.Fatal(".claude/settings.json written headlessly")
	}
	if got := readFile(t, filepath.Join(f.Root, "AGENTS.md")); got != "keep\n" {
		t.Fatalf("AGENTS.md = %q, want unchanged", got)
	}
	if got := readFile(t, filepath.Join(f.Root, "notes.txt")); got != "n\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(f.Root, "notes2.txt")); got != "new\n" {
		t.Fatalf("notes2.txt = %q, want the ordinary edit applied", got)
	}
	line := func(tool string) string {
		return "golem: denied " + tool + ": harness file, -allow-tool does not cover it (" + agenttools.HarnessApprovalNotice + ")\n"
	}
	if got, want := buf.String(), line("write_file")+line("edit_file"); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	ctx := context.Background()
	other := newHeadlessApprover(allowToolSet{names: map[string]struct{}{"stop_command": {}, "run_command": {}}})
	stop := provider.ToolCall{Function: provider.ToolCallFunction{Name: "stop_command"}}
	if d, err := other.ApproveKeyed(ctx, stop, "", ""); err != nil || !d.Approved {
		t.Fatalf("stop_command with its empty key: d=%+v err=%v, want approved", d, err)
	}
	if d, err := other.ApproveKeyed(ctx, execCall(), "", "exec:abc"); err != nil || !d.Approved {
		t.Fatalf("run_command: d=%+v err=%v, want approved", d, err)
	}
	var none *headlessApprover
	if got := none.withNotices(&buf); got != nil {
		t.Fatalf("withNotices on nil = %v, want nil", got)
	}
}

// TestHarnessApprovalTaskStepDenies: under -approve-plan-edits a task step
// whose scope covers CLAUDE.md cannot write it; an ordinary in-scope write
// still lands and is the only file change Agentflow records.
func TestHarnessApprovalTaskStepDenies(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "CLAUDE.md", "keep\n")
	plan := &agentflow.Plan{AllowedFiles: []string{"CLAUDE.md", "notes.txt"}, Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"CLAUDE.md", "notes.txt"},
	}}}
	caller := &scriptCaller{responses: []agent.ModelResult{
		toolStep("w-1", "write_file", `{"path":"CLAUDE.md","content":"pwned\n"}`),
		toolStep("w-2", "write_file", `{"path":"notes.txt","content":"worker\n"}`),
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	orch := agent.New(caller, agent.ContextManager{})
	af := &fakeAF{}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, af, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: af, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	const denied = "tool call denied by approver"
	if got := toolObservation(t, caller.lastRequest.Messages, "w-1").Content; !strings.Contains(got, denied) {
		t.Fatalf("w-1 observation = %q, want the approver denial", got)
	}
	if got := toolObservation(t, caller.lastRequest.Messages, "w-2").Content; strings.Contains(got, denied) {
		t.Fatalf("w-2 observation = %q, want the ordinary write approved", got)
	}
	if got := readFile(t, filepath.Join(root, "CLAUDE.md")); got != "keep\n" {
		t.Fatalf("CLAUDE.md = %q, want unchanged", got)
	}
	if got := readFile(t, filepath.Join(root, "notes.txt")); got != "worker\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	want := []string{"claim:P1", "record-file-change:P1:A-P1:notes.txt", "finish-step:P1:A-P1"}
	if !equalSeq(af.seq, want) {
		t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
	}
}

// TestHarnessApprovalTaskApproverPlainPath: the plain entry point carries no
// key, so a file-tool call through it fails closed; the keyed path approves
// ordinary writes and leaves empty-key tools outside the files scope alone.
func TestHarnessApprovalTaskApproverPlainPath(t *testing.T) {
	ctx := context.Background()
	write := newCall()
	if ok, err := taskApprover(true).Approve(ctx, write, ""); err != nil || ok {
		t.Fatalf("plain Approve(write_file) = %v, %v, want denied", ok, err)
	}
	if d, err := taskApprover(true).ApproveKeyed(ctx, write, "", agenttools.WriteClassApprovalKey); err != nil || !d.Approved {
		t.Fatalf("ApproveKeyed(write_file, class key) = %+v, %v, want approved", d, err)
	}
	if d, err := taskApprover(true).ApproveKeyed(ctx, execCall(), "", ""); err != nil || !d.Approved {
		t.Fatalf("ApproveKeyed(run_command, \"\") = %+v, %v, want approved", d, err)
	}
}

// TestHarnessApprovalPlanLockStillWritesAgentDir pins the one named
// exception: under -approve-plan-lock, submit_plan writes .agent/ natively.
func TestHarnessApprovalPlanLockStillWritesAgentDir(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, &autoPlanApprover{out: &out})
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"golem-task-brief-*.json", "golem-workflow-handoff-*.json"} {
		got, err := filepath.Glob(filepath.Join(root, ".agent", pattern))
		if err != nil || len(got) != 1 {
			t.Fatalf(".agent/%s = %v, %v, want one file", pattern, got, err)
		}
	}
}
