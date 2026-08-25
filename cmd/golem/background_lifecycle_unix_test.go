//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s unavailable: %v", name, err)
	}
}

// startBackgroundJob launches argv through the REAL start_command
// agent.PlanningTool Plan/Invoke path — the exact path a model call takes,
// minus dispatch's approval prompt — and returns the manager's snapshot of the
// registered job. Handles come from manager.List, never from parsing the
// model-facing result string.
func startBackgroundJob(t *testing.T, mgr *agenttools.BackgroundManager, execTools []agent.Tool, argv ...string) agenttools.JobStatus {
	t.Helper()
	var start agent.Tool
	for _, tool := range execTools {
		if tool.Spec().Name == "start_command" {
			start = tool
			break
		}
	}
	if start == nil {
		t.Fatalf("start_command not registered; tools = %v", names(execTools))
	}
	pt, ok := start.(agent.PlanningTool)
	if !ok {
		t.Fatalf("start_command is %T, want agent.PlanningTool", start)
	}
	raw, err := json.Marshal(map[string]any{"argv": argv})
	if err != nil {
		t.Fatal(err)
	}
	before := len(mgr.List())
	if _, err := pt.Plan(context.Background(), raw); err != nil {
		t.Fatalf("start_command Plan: %v", err)
	}
	res, err := start.Invoke(context.Background(), raw)
	if err != nil || res.IsError {
		t.Fatalf("start_command Invoke: result=%+v err=%v", res, err)
	}
	list := mgr.List()
	if len(list) != before+1 {
		t.Fatalf("manager.List() has %d jobs after start, want %d", len(list), before+1)
	}
	st := list[len(list)-1]
	// Safety net only; the assertion paths must be covered by the wiring alone.
	t.Cleanup(func() { _ = syscall.Kill(-st.PID, syscall.SIGKILL) })
	return st
}

func waitJobFinished(t *testing.T, mgr *agenttools.BackgroundManager, handle string) agenttools.JobStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range mgr.List() {
			if st.Handle == handle && st.State != "running" {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s still running after 10s", handle)
	return agenttools.JobStatus{}
}

// groupGone reports whether the job's whole process group is gone (ESRCH).
func groupGone(pid int) bool {
	return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

func waitGroupGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if groupGone(pid) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process group %d still present after %v", pid, timeout)
}

func backgroundRunArgs(configPath, root string) []string {
	return []string{"-config", configPath, "-root", root, "-allow-exec",
		"-no-probe", "-no-cap-probe", "-no-session", "-no-memory",
		"-no-project-context", "-no-auto-index"}
}

// TestRunBackgroundShutdownOnEOF drives the real run(): the hook launches a
// long-lived job through the registered start_command tool, stdin hits EOF,
// and the process group must be gone before run returns (a closed("background")
// callback alone is not sufficient evidence).
func TestRunBackgroundShutdownOnEOF(t *testing.T) {
	requireBinary(t, "sleep")
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t) // empty stdin => immediate REPL EOF
	var events []string
	pid := 0
	err := run(backgroundRunArgs(configPath, root), stdin, stdout, stderr, runHooks{
		afterBackgroundReady: func(mgr *agenttools.BackgroundManager, execTools []agent.Tool, _ context.CancelFunc) {
			st := startBackgroundJob(t, mgr, execTools, "sleep", "30")
			if st.State != "running" {
				t.Fatalf("job state = %q, want running", st.State)
			}
			pid = st.PID
		},
		closed: func(name string) { events = append(events, name) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if pid == 0 {
		t.Fatal("afterBackgroundReady hook never ran")
	}
	if !groupGone(pid) {
		t.Fatalf("process group %d survived REPL EOF; run must not return before shutdown reaps it", pid)
	}
	if !strings.Contains(strings.Join(events, ","), "background") {
		t.Fatalf("shutdown events %v missing background close", events)
	}
}

// TestRunBackgroundShutdownOnREPLContextCancel proves the replCtx AfterFunc
// binding tears the group down on host-context cancellation: stdin is a pipe
// that never delivers data or EOF, so only cancelREPL can end the REPL, and
// the gate in the auto-index stop hook holds run inside the REPL branch until
// the group's death has been observed — i.e. BEFORE run's own deferred
// shutdown can act and before run returns.
func TestRunBackgroundShutdownOnREPLContextCancel(t *testing.T) {
	requireBinary(t, "sleep")
	configPath, root := writeRunLifecycleConfig(t)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	_, stdout, stderr := runTestFiles(t)
	gate := make(chan error, 1)
	var gateErr error
	gateRead := false
	pid := 0
	runErr := run(backgroundRunArgs(configPath, root), pr, stdout, stderr, runHooks{
		startAutoIndex: func() func() {
			return func() {
				// Runs during the REPL branch teardown, before run's own
				// deferred manager shutdown: an ESRCH observed here can only
				// have come through the replCtx binding.
				gateErr = <-gate
				gateRead = true
			}
		},
		afterBackgroundReady: func(mgr *agenttools.BackgroundManager, execTools []agent.Tool, cancelREPL context.CancelFunc) {
			st := startBackgroundJob(t, mgr, execTools, "sleep", "30")
			pid = st.PID
			go func() {
				cancelREPL()
				gate <- waitGroupGone(st.PID, 5*time.Second)
			}()
		},
	})
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if pid == 0 {
		t.Fatal("afterBackgroundReady hook never ran")
	}
	if !gateRead {
		t.Fatal("auto-index stop hook never ran: the gate proves nothing")
	}
	if gateErr != nil {
		t.Fatalf("replCtx cancellation did not tear the group down before run returned: %v", gateErr)
	}
	if !groupGone(pid) {
		t.Fatalf("process group %d present after run returned", pid)
	}
}

// TestJobsListAndStopWithRealJobs covers the /jobs surface over a real
// manager: running and finished listing, direct user stop with no approval
// prompt (there is no line source at all), and already-finished stop.
func TestJobsListAndStopWithRealJobs(t *testing.T) {
	requireBinary(t, "sleep")
	requireBinary(t, "true")
	root := t.TempDir()
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	execTools, err := buildExecTools(root, mgr)
	if err != nil {
		t.Fatal(err)
	}
	sess := &replSession{bgManager: mgr}

	running := startBackgroundJob(t, mgr, execTools, "sleep", "30")
	finished := startBackgroundJob(t, mgr, execTools, "true")
	finishedFinal := waitJobFinished(t, mgr, finished.Handle)
	if finishedFinal.State != "exited" || !finishedFinal.ExitKnown || finishedFinal.ExitCode != 0 {
		t.Fatalf("finished job = %+v, want exited/0/known", finishedFinal)
	}

	var out strings.Builder
	if _, exit := dispatchSlash(context.Background(), &out, sess, "/jobs"); exit {
		t.Fatal("/jobs must not exit")
	}
	got := out.String()
	for _, want := range []string{
		running.Handle, "running", fmt.Sprintf("pid=%d", running.PID), "sleep 30",
		finished.Handle, "exited", "exit=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("/jobs listing missing %q:\n%s", want, got)
		}
	}

	// Direct stop: user-initiated, so no approval prompt is possible — the
	// session has no line source to answer one.
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/jobs stop "+running.Handle)
	if !strings.Contains(out.String(), "stopped "+running.Handle) || !strings.Contains(out.String(), "killed") {
		t.Fatalf("/jobs stop must report the stop and final state:\n%s", out.String())
	}
	if err := waitGroupGone(running.PID, 5*time.Second); err != nil {
		t.Fatalf("stopped job's group survived: %v", err)
	}

	// Stopping an already-finished job reports its exit, kills nothing new.
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/jobs stop "+finished.Handle)
	if !strings.Contains(out.String(), "already finished: "+finished.Handle) ||
		!strings.Contains(out.String(), "exit 0") {
		t.Fatalf("/jobs stop on a finished job must report already finished with its exit code:\n%s", out.String())
	}
}

// TestJobsStopCanceledContextReportsReaping covers context expiry: when
// Stop returns (snapshot, ctx.Err()) — kill issued, reaping continues — /jobs
// stop must report the stop as requested, not print the raw context error.
// The context is canceled BEFORE the call, so Stop's ctx.Done() arm is ready
// while the just-killed job's completion is still being reaped.
func TestJobsStopCanceledContextReportsReaping(t *testing.T) {
	requireBinary(t, "sleep")
	root := t.TempDir()
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	execTools, err := buildExecTools(root, mgr)
	if err != nil {
		t.Fatal(err)
	}
	sess := &replSession{bgManager: mgr}
	job := startBackgroundJob(t, mgr, execTools, "sleep", "30")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	if _, exit := dispatchSlash(canceled, &out, sess, "/jobs stop "+job.Handle); exit {
		t.Fatal("/jobs stop must not exit")
	}
	got := out.String()
	if !strings.Contains(got, "stop requested for "+job.Handle+"; still reaping") &&
		!strings.Contains(got, "stopped "+job.Handle+" (killed)") {
		t.Fatalf("canceled-context stop must report either in-progress or completed stop:\n%s", got)
	}
	if strings.Contains(got, context.Canceled.Error()) {
		t.Fatalf("canceled-context stop must not print the raw context error:\n%s", got)
	}
	// The contract behind the message: the kill was issued and the manager
	// finishes reaping without any further user action.
	if err := waitGroupGone(job.PID, 5*time.Second); err != nil {
		t.Fatalf("stop with canceled context must still have issued the kill: %v", err)
	}
}

// TestJobsListSanitizesArgvFromRealJob proves the /jobs listing routes REAL
// job argv through the control-safe renderer, not just that the renderer works
// in isolation.
func TestJobsListSanitizesArgvFromRealJob(t *testing.T) {
	requireBinary(t, "echo")
	root := t.TempDir()
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	execTools, err := buildExecTools(root, mgr)
	if err != nil {
		t.Fatal(err)
	}
	sess := &replSession{bgManager: mgr}
	job := startBackgroundJob(t, mgr, execTools, "echo", "\x1b[2J\u009b\u202evil")
	waitJobFinished(t, mgr, job.Handle)

	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/jobs")
	got := out.String()
	for _, raw := range []string{"\x1b", "\u009b", "\u202e"} {
		if strings.Contains(got, raw) {
			t.Fatalf("/jobs listing leaked raw control %q:\n%q", raw, got)
		}
	}
	for _, escaped := range []string{`\x1b`, `\u009b`, `\u202e`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("/jobs listing missing visible escape %q:\n%q", escaped, got)
		}
	}
}

// TestJobsSurviveSessionBoundariesWhileGrantsClear pins the approved
// process-scope policy: /new, /clear, and successful /resume leave manager
// jobs and handles untouched while the session's approval grants still clear.
func TestJobsSurviveSessionBoundariesWhileGrantsClear(t *testing.T) {
	requireBinary(t, "sleep")
	root := t.TempDir()
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	execTools, err := buildExecTools(root, mgr)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSessionedTestSession(t, &captureCaller{answer: "x"}, root, "workspace:a1")
	sess.bgManager = mgr
	if err := sess.session.store.Save(context.Background(), conversation.Conversation{
		ID:    "user:other",
		Title: "other",
		Messages: []conversation.Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "a"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	job := startBackgroundJob(t, mgr, execTools, "sleep", "60")

	for _, cmd := range []string{"/new", "/clear", "/resume user:other"} {
		sess.grants = newApprovalGrants()
		sess.grants.grant(grantScopeExec, "exec-bg:v1:abc")
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, cmd)
		if sess.grants.count() != 0 {
			t.Fatalf("%s must clear approval grants, count = %d\n%s", cmd, sess.grants.count(), out.String())
		}
		list := mgr.List()
		if len(list) != 1 || list[0].Handle != job.Handle || list[0].State != "running" {
			t.Fatalf("%s must leave background jobs untouched (process-scope policy): %+v\n%s", cmd, list, out.String())
		}
	}
}
