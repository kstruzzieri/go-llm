//go:build linux || darwin

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// The observable guarantee is that the process is gone, not that a callback
// ran: both tests start a real job through the mounted start_command tool
// and wait for its process group to disappear.

// /allow-exec binds the manager to the REPL context like a startup manager
// (#346): cancelling it tears every job down.
func TestAllowExecShutsDownJobsOnREPLContextCancel(t *testing.T) {
	requireBinary(t, "sleep")
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	_, _ = dispatchSlash(ctx, &out, sess, "/allow-exec")
	if sess.bgManager == nil {
		t.Fatalf("mount failed: %q", out.String())
	}
	st := startBackgroundJob(t, sess.bgManager, sess.tools, "sleep", "30")
	cancel()
	if err := waitGroupGone(st.PID, 5*time.Second); err != nil {
		t.Fatalf("REPL context cancellation did not tear the late manager's jobs down: %v", err)
	}
}

// Session shutdown releases a late manager through closeLateMounts.
func TestCloseLateMountsShutsDownJobs(t *testing.T) {
	requireBinary(t, "sleep")
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	if sess.bgManager == nil {
		t.Fatalf("mount failed: %q", out.String())
	}
	st := startBackgroundJob(t, sess.bgManager, sess.tools, "sleep", "30")
	if err := sess.closeLateMounts(); err != nil {
		t.Fatalf("closeLateMounts: %v", err)
	}
	if err := waitGroupGone(st.PID, 5*time.Second); err != nil {
		t.Fatalf("closeLateMounts did not tear the late manager's jobs down: %v", err)
	}
}

// closeLateMounts owns only what the commands opened: a startup-owned
// manager keeps serving after it runs.
func TestCloseLateMountsLeavesStartupManagerAlone(t *testing.T) {
	requireBinary(t, "true")
	root := t.TempDir()
	mgr, execTools, err := buildExecMount(root, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatalf("exec set: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root, execTools...)
	sess.allowExec, sess.bgManager = true, mgr
	if err := sess.closeLateMounts(); err != nil {
		t.Fatalf("closeLateMounts: %v", err)
	}
	st := startBackgroundJob(t, mgr, execTools, "true")
	waitJobFinished(t, mgr, st.Handle)
}
