package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/projectcontext"
)

// newRefreshSession is a mount session over a real repository whose startup
// Git state is installed the way main.go installs it: captured, composed into
// sysInputs, published through the mount seam, retained on the session.
func newRefreshSession(t *testing.T, caller agent.ModelCaller) (*replSession, string) {
	t.Helper()
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	sess := newMountSession(t, caller, root)
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatal(err)
	}
	in := sess.sysInputs
	in.gitContext = snap.Block
	if err := sess.mount(sess.mountAt, nil, in); err != nil {
		t.Fatal(err)
	}
	sess.gitSnapshot = snap
	if !strings.Contains(sess.baseSystem, "branch: main\n") {
		t.Fatalf("startup state not installed: %q", sess.baseSystem)
	}
	return sess, root
}

func gitFences(s string) int {
	return strings.Count(s, gitContextOpen) + strings.Count(s, gitContextClose)
}

func refresh(t *testing.T, sess *replSession) string {
	t.Helper()
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/git-context refresh")
	return out.String()
}

func TestGitContextRefreshReplacesOnce(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	sess, root := newRefreshSession(t, caller)
	gitContextTestRun(t, root, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(root, "tracked.go"), []byte("package y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := refresh(t, sess); got != "git context refreshed: feature, 1 status entry, 1 recent commit\n" {
		t.Fatalf("refresh output = %q", got)
	}
	// Runtime request: the next turn carries the new block, exactly once.
	if _, err := runOnce(context.Background(), io.Discard, nil, sess, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if caller.system != sess.baseSystem || gitFences(caller.system) != 2 || !strings.Contains(caller.system, "branch: feature\n") || strings.Contains(caller.system, "branch: main\n") {
		t.Fatalf("runtime request after refresh:\n%s", caller.system)
	}
	// Planner request: the -goal planner reads the same refreshed inputs.
	planner := &captureCaller{answer: "no submission"}
	sess.orch = agent.New(planner, agent.ContextManager{})
	err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("planner: %v", err)
	}
	if gitFences(planner.system) != 2 || !strings.Contains(planner.system, "branch: feature\n") {
		t.Fatalf("planner request after refresh:\n%s", planner.system)
	}
	if sess.gitSnapshot.State.Branch != "feature" || sess.gitSnapshot.Block != sess.sysInputs.gitContext || sess.baseSystem != composeSystem(sess.sysInputs) {
		t.Fatal("session snapshot, inputs, and baseSystem disagree after refresh")
	}
}

// Identical capture bytes skip the runtime write entirely: the tool slice is
// the very same backing array (a mount always clones it).
func TestGitContextRefreshUnchanged(t *testing.T) {
	sess, _ := newRefreshSession(t, &captureCaller{answer: "ok"})
	before, tools := sess.baseSystem, reflect.ValueOf(sess.tools).Pointer()
	if got := refresh(t, sess); got != "git context unchanged\n" {
		t.Fatalf("refresh output = %q", got)
	}
	if sess.baseSystem != before || reflect.ValueOf(sess.tools).Pointer() != tools {
		t.Fatal("an unchanged refresh performed a runtime replacement")
	}
}

func TestGitContextRefreshRepeated(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	sess, root := newRefreshSession(t, caller)
	prev := "main"
	for _, branch := range []string{"r1", "r2", "r3"} {
		gitContextTestRun(t, root, "checkout", "-q", "-b", branch)
		if got := refresh(t, sess); !strings.HasPrefix(got, "git context refreshed: "+branch+",") {
			t.Fatalf("round %s: %q", branch, got)
		}
		if gitFences(sess.baseSystem) != 2 || !strings.Contains(sess.baseSystem, "branch: "+branch+"\n") || strings.Contains(sess.baseSystem, "branch: "+prev+"\n") {
			t.Fatalf("round %s: stale or duplicated block:\n%s", branch, sess.baseSystem)
		}
		prev = branch
	}
}

// Absence clears the fragment and re-renders the retained project documents
// at the full budget; nothing is reread from disk.
func TestGitContextRefreshClearsOnNonRepo(t *testing.T) {
	sess, root := newRefreshSession(t, &captureCaller{answer: "ok"})
	docs := []projectcontext.Document{{Source: "workspace", Path: filepath.Join(root, "AGENTS.md"), Content: strings.Repeat("rule line\n", 3000)}}
	sess.projectDocs = docs
	in := sess.sysInputs
	in.projectContext = projectContextBlock(docs, projectContextBudget(sess.gitSnapshot.PayloadBytes))
	if err := sess.mount(sess.mountAt, nil, in); err != nil {
		t.Fatal(err)
	}
	if in.projectContext == projectContextBlock(docs, projectContextMaxBytes) {
		t.Fatal("fixture: the Git payload must actually reduce the project budget")
	}
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := refresh(t, sess); got != "git context cleared: not a repository\n" {
		t.Fatalf("refresh output = %q", got)
	}
	if sess.sysInputs.gitContext != "" || gitFences(sess.baseSystem) != 0 || sess.gitSnapshot.Block != "" || sess.gitSnapshot.Absence != gitContextNotRepository {
		t.Fatalf("stale Git state after clearing: inputs=%q snapshot=%+v", sess.sysInputs.gitContext, sess.gitSnapshot)
	}
	if sess.sysInputs.projectContext != projectContextBlock(docs, projectContextMaxBytes) || !strings.Contains(sess.baseSystem, "rule line") {
		t.Fatal("project context was not re-rendered at the full budget after the Git block cleared")
	}
	if sess.baseSystem != composeSystem(sess.sysInputs) {
		t.Fatal("baseSystem != composeSystem(sysInputs)")
	}
}

func TestGitContextRefreshClearsWhenGitUnavailable(t *testing.T) {
	sess, _ := newRefreshSession(t, &captureCaller{answer: "ok"})
	t.Setenv("PATH", t.TempDir())
	if got := refresh(t, sess); got != "git context cleared: git unavailable\n" {
		t.Fatalf("refresh output = %q", got)
	}
	if sess.sysInputs.gitContext != "" || gitFences(sess.baseSystem) != 0 || sess.gitSnapshot.Absence != gitContextGitUnavailable {
		t.Fatalf("stale Git state after clearing: %q %+v", sess.sysInputs.gitContext, sess.gitSnapshot)
	}
}

func TestGitContextRefreshUsageAndHelp(t *testing.T) {
	sess := newMountSession(t, &captureCaller{answer: "ok"}, t.TempDir())
	for _, line := range []string{"/git-context", "/git-context refresh now", "/git-context reload", "/git-context REFRESH"} {
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, line)
		if out.String() != "usage: /git-context refresh\n" {
			t.Fatalf("%q -> %q", line, out.String())
		}
	}
	if !strings.Contains(golemHelp, "/git-context refresh") {
		t.Fatal("/help does not list /git-context refresh")
	}
}

// Refresh after both capability mounts keeps the mounted tool set
// byte-identical and the write/exec prompt fragments in place; a mount after
// a refresh keeps the refreshed Git block.
func TestGitContextRefreshAfterCapabilityMounts(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	sess, root := newRefreshSession(t, caller)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	if !strings.Contains(out.String(), "writes enabled") || !strings.Contains(out.String(), "exec enabled") {
		t.Fatalf("mounts: %s", out.String())
	}
	wantNames, wantHash := strings.Join(names(sess.tools), ","), toolSchemaHash(sess.tools)
	gitContextTestRun(t, root, "checkout", "-q", "-b", "feature")
	if got := refresh(t, sess); !strings.HasPrefix(got, "git context refreshed: feature,") {
		t.Fatalf("refresh: %q", got)
	}
	if got := strings.Join(names(sess.tools), ","); got != wantNames || toolSchemaHash(sess.tools) != wantHash {
		t.Fatalf("refresh changed the mounted tools: %s != %s", got, wantNames)
	}
	if !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(true, true)) || gitFences(sess.baseSystem) != 2 || !strings.Contains(sess.baseSystem, "branch: feature\n") {
		t.Fatalf("prompt after mounts+refresh:\n%s", sess.baseSystem)
	}
	if _, err := runOnce(context.Background(), io.Discard, nil, sess, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if caller.system != sess.baseSystem || strings.Join(caller.tools, ",") != wantNames {
		t.Fatalf("runtime disagrees with the session after refresh: tools=%v", caller.tools)
	}

	// The other order: refresh first, then a mount keeps the Git block.
	fresh := &captureCaller{answer: "ok"}
	sess2, root2 := newRefreshSession(t, fresh)
	gitContextTestRun(t, root2, "checkout", "-q", "-b", "feature")
	_ = refresh(t, sess2)
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess2, "/allow-write")
	if !strings.Contains(out.String(), "writes enabled") || gitFences(sess2.baseSystem) != 2 || !strings.Contains(sess2.baseSystem, "branch: feature\n") || !strings.HasPrefix(sess2.baseSystem, buildSystemPrompt(true, false)) {
		t.Fatalf("mount after refresh lost the Git block or the write fragment:\n%s\n%s", out.String(), sess2.baseSystem)
	}
}

// A rejected replacement leaves every session field exactly as it was and
// reports the failure.
func TestGitContextRefreshFailedReplaceLeavesSessionUnchanged(t *testing.T) {
	sess, root := newRefreshSession(t, &captureCaller{answer: "ok"})
	gitContextTestRun(t, root, "checkout", "-q", "-b", "feature") // a change is pending
	in, system, snap, docs := sess.sysInputs, sess.baseSystem, sess.gitSnapshot, sess.projectDocs
	if err := sess.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	got := refresh(t, sess)
	if !strings.HasPrefix(got, "git context refresh failed: runtime: ") || !strings.Contains(got, "closed") {
		t.Fatalf("refresh output = %q", got)
	}
	if sess.sysInputs != in || sess.baseSystem != system || !reflect.DeepEqual(sess.gitSnapshot, snap) || !reflect.DeepEqual(sess.projectDocs, docs) {
		t.Fatal("a failed replacement changed session state")
	}
}

// Trace metadata records the very System the runtime sent after a refresh:
// the invariant baseSystem == composeSystem(sysInputs) is what both read.
func TestGitContextRefreshRuntimeSystemMatchesTrace(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	sess, root := newRefreshSession(t, caller)
	obs, err := newObserv(os.Getenv, root, true, false, time.Now)
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	sess.obs = obs
	gitContextTestRun(t, root, "checkout", "-q", "-b", "feature")
	_ = refresh(t, sess)
	if _, err := runOnce(context.Background(), io.Discard, nil, sess, "hello", nil); err != nil {
		t.Fatal(err)
	}
	sent := caller.system
	if !strings.Contains(sent, "branch: feature\n") {
		t.Fatalf("runtime did not send the refreshed block: %q", sent)
	}
	files, _ := filepath.Glob(filepath.Join(obs.traceDir, "*.json"))
	if len(files) == 0 {
		t.Fatalf("no trace written under %s", obs.traceDir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var rec agenttrace.TraceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if rec.Request.System != sent {
			t.Fatalf("%s records a different System than the runtime sent after refresh", f)
		}
	}
}
