//go:build linux || darwin

package main

import (
	"context"
	"strings"
	"testing"
	"time"
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

// Reachable D9 state: startup was -allow-exec -scratch without -allow-write,
// so the exec set carries scratch tools and no promotion. A later
// /allow-write keeps that set and its manager exactly, inserts the write
// tools before it, and says promotion stays startup-bound.
func TestAllowWriteKeepsStartupScratchExecSet(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	opts, _ := scratchExecOptions(true, nil)
	mgr, execTools, err := buildExecMount(root, opts)
	if err != nil {
		t.Fatalf("scratch exec set: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	sess := newMountSession(t, caller, root, execTools...)
	sess.allowExec, sess.bgManager, sess.scratch = true, mgr, true
	sess.sysInputs.allowExec = true
	sess.baseSystem = composeSystem(sess.sysInputs)
	before := strings.Join(names(execTools), ",")
	if !strings.Contains(before, "scratch_changes") || strings.Contains(before, "promote_artifact") {
		t.Fatalf("precondition: scratch set without promotion expected, got %s", before)
	}
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes enabled") || !strings.Contains(out.String(), "scratch: promote_artifact stays unavailable") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.bgManager != mgr {
		t.Fatal("/allow-write replaced the startup background manager")
	}
	got := strings.Join(names(sess.tools[sess.readToolCount:]), ",")
	if got != "write_file,edit_file,"+before {
		t.Fatalf("gated tools = %s, want write tools inserted before the untouched scratch exec set", got)
	}
	if !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(true, true)) {
		t.Fatalf("prompt = %q", sess.baseSystem)
	}
}
