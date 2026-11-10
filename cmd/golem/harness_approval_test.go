package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	// containment: GRANT-HARNESS
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
	// containment: GRANT-HARNESS
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
	// containment: GRANT-HARNESS
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
	// containment: GRANT-HARNESS-UNATTENDED
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
	// containment: GRANT-HARNESS-UNATTENDED
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
	// containment: GRANT-HARNESS-UNATTENDED
	if ok, err := (taskApprover{edits: true}).Approve(ctx, write, ""); err != nil || ok {
		t.Fatalf("plain Approve(write_file) = %v, %v, want denied", ok, err)
	}
	if d, err := (taskApprover{edits: true}).ApproveKeyed(ctx, write, "", agenttools.WriteClassApprovalKey); err != nil || !d.Approved {
		t.Fatalf("ApproveKeyed(write_file, class key) = %+v, %v, want approved", d, err)
	}
	if d, err := (taskApprover{edits: true}).ApproveKeyed(ctx, execCall(), "", ""); err != nil || !d.Approved {
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

// The #784 tests below build their fixtures inside buildWriteTools, out of any
// seam's reach, so the reference scan reads the real ancestors of t.TempDir().
// CI temp roots hold no harness files; a developer's own harness files are
// guarded by TestHarnessPlanReferencesUserSettings and the Task 11 benchmark.

// refKinds are the six kinds of "a harness file loads, runs or links to this
// target" from issue acceptance 1, one write each.
var refKinds = []string{"docs/a.md", "scripts/h.sh", "docs/shared/x.md", "docs/oc.md", "docs/g.md", "scripts/server.py"}

// refFixture holds the harness files that cover every path in refKinds.
func refFixture(t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("creates a symlink")
	}
	f := newFixture(t, opts)
	writeFixtureFile(t, f.Root, "CLAUDE.md", "see @docs/a.md\n")
	writeFixtureFile(t, f.Root, ".claude/settings.json", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"scripts/h.sh"}]}]}}`+"\n")
	mkdirFixture(t, f.Root, "docs/shared")
	mkdirFixture(t, f.Root, ".claude/rules")
	if err := os.Symlink("../../docs/shared", filepath.Join(f.Root, ".claude/rules/shared")); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, f.Root, "opencode.json", `{"instructions":["docs/oc.md"]}`+"\n")
	writeFixtureFile(t, f.Root, "GEMINI.md", "@docs/g.md\n")
	writeFixtureFile(t, f.Root, ".mcp.json", `{"mcpServers":{"s":{"command":"python3","args":["scripts/server.py"]}}}`+"\n")
	mkdirFixture(t, f.Root, "scripts")
	return f
}

func refWrite(path string) agent.ModelResult {
	return call("write_file", map[string]string{"path": path, "content": "x\n"})
}

// TestHarnessApprovalRefPrompts is acceptance 5: each covered target prompts
// with the notice and the reason line, remembers nothing, and an ordinary
// file still rides the /auto-edits grant.
func TestHarnessApprovalRefPrompts(t *testing.T) {
	f := refFixture(t, fixtureOpts{autoEdits: true})
	var calls []agent.ModelResult
	for _, p := range refKinds {
		calls = append(calls, refWrite(p))
	}
	calls = append(calls, refWrite("notes.txt"))
	res := f.run(t, calls, "a\ny\nN\ny\ny\nN\n")

	requireRecords(t, res, 7)
	out := f.Out.String()
	// containment: GRANT-HARNESS-REF
	if got := f.prompts(); got != 6 {
		t.Fatalf("prompts = %d, want 6:\n%s", got, out)
	}
	if got := strings.Count(out, harnessNoticeLine); got != 6 {
		t.Fatalf("notice lines = %d, want 6:\n%s", got, out)
	}
	if got := strings.Count(out, agenttools.HarnessReferenceReasonPrefix); got != 6 {
		t.Fatalf("reason lines = %d, want 6:\n%s", got, out)
	}
	for _, i := range []int{0, 2, 5} {
		requireDenied(t, res, i)
		if exists(t, filepath.Join(f.Root, refKinds[i])) {
			t.Fatalf("%s written after a denial", refKinds[i])
		}
	}
	for _, i := range []int{1, 3, 4} {
		requireInvoked(t, res, i, false)
	}
	requireInvoked(t, res, 6, true)
	if got := f.Grants.count(); got != 1 {
		t.Fatalf("grants = %d, want only the seeded /auto-edits grant", got)
	}
}

// TestHarnessApprovalRefSameRun: a covered target is judged when the call is
// planned, so an import added earlier in the same run is already in force. The
// ordinary write first builds the set once before the edit, so a set kept
// from an earlier call would let docs/new.md ride the /auto-edits grant.
func TestHarnessApprovalRefSameRun(t *testing.T) {
	f := newFixture(t, fixtureOpts{autoEdits: true})
	writeFixtureFile(t, f.Root, "CLAUDE.md", "keep\n")
	mkdirFixture(t, f.Root, "docs")
	res := f.run(t, []agent.ModelResult{
		refWrite("notes.txt"),
		call("edit_file", map[string]string{"path": "CLAUDE.md", "old_string": "keep", "new_string": "keep @docs/new.md"}),
		refWrite("docs/new.md"),
	}, "y\n")
	requireRecords(t, res, 3)
	// containment: GRANT-HARNESS-REF
	requireInvoked(t, res, 0, true)
	requireInvoked(t, res, 1, false)
	requireDenied(t, res, 2)
	if got := f.prompts(); got != 2 {
		t.Fatalf("prompts = %d, want 2:\n%s", got, f.Out.String())
	}
	if exists(t, filepath.Join(f.Root, "docs/new.md")) {
		t.Fatal("docs/new.md written with no answer")
	}
}

// TestHarnessApprovalRefHeadless is acceptance 6: -allow-tool does not cover
// a covered target, and the denial line is followed by the reason.
func TestHarnessApprovalRefHeadless(t *testing.T) {
	f := refFixture(t, fixtureOpts{})
	var buf strings.Builder
	ap := newHeadlessApprover(allowToolSet{names: map[string]struct{}{"write_file": {}}}).withNotices(&buf)
	// An ordinary write follows each denial: the loop stops after three
	// denials in a row, and each ordinary write proves -allow-tool still works.
	var calls []agent.ModelResult
	for i, p := range refKinds {
		calls = append(calls, refWrite(p), refWrite("n"+strconv.Itoa(i)+".txt"))
	}
	calls = append(calls, answerStep("done"))
	res, err := f.runWith(t, &scriptCaller{responses: calls}, ap, len(calls))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	requireRecords(t, res, 2*len(refKinds))
	// containment: GRANT-HARNESS-REF-UNATTENDED
	for i, p := range refKinds {
		requireDenied(t, res, 2*i)
		requireInvoked(t, res, 2*i+1, false)
		if exists(t, filepath.Join(f.Root, p)) {
			t.Fatalf("%s written headlessly", p)
		}
	}
	if !strings.Contains(buf.String(), "golem: harness reference: CLAUDE.md imports docs/a.md\n") {
		t.Fatalf("notices = %q, want the reason line", buf.String())
	}
}

// TestHarnessApprovalRefTaskStep is acceptance 7: a task step cannot write an
// import target even with edits opted in, the denial says why, and the
// ordinary files of the same step land.
func TestHarnessApprovalRefTaskStep(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "CLAUDE.md", "see @docs/a.md\n")
	mkdirFixture(t, root, "docs")
	plan := &agentflow.Plan{AllowedFiles: []string{"docs/a.md", "notes.txt"}, Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"docs/a.md", "notes.txt"},
	}}}
	caller := &scriptCaller{responses: []agent.ModelResult{
		toolStep("w-1", "write_file", `{"path":"docs/a.md","content":"pwned\n"}`),
		toolStep("w-2", "write_file", `{"path":"notes.txt","content":"worker\n"}`),
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	orch := agent.New(caller, agent.ContextManager{})
	af := &fakeAF{}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	runStep, err := newTaskStepRunner(root, plan, af, orch, &replSession{maxSteps: 4}, true, &out, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: af, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	const denied = "tool call denied by approver"
	// containment: GRANT-HARNESS-REF-UNATTENDED
	if got := toolObservation(t, caller.lastRequest.Messages, "w-1").Content; !strings.Contains(got, denied) {
		t.Fatalf("w-1 observation = %q, want the approver denial", got)
	}
	if got := toolObservation(t, caller.lastRequest.Messages, "w-2").Content; strings.Contains(got, denied) {
		t.Fatalf("w-2 observation = %q, want the ordinary write approved", got)
	}
	if exists(t, filepath.Join(root, "docs/a.md")) {
		t.Fatal("docs/a.md written by a task step")
	}
	if got := readFile(t, filepath.Join(root, "notes.txt")); got != "worker\n" {
		t.Fatalf("notes.txt = %q", got)
	}
	for _, want := range []string{"golem: task step denied write_file", "golem: harness reference: CLAUDE.md imports docs/a.md"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("out = %q, want %q", out.String(), want)
		}
	}
}
