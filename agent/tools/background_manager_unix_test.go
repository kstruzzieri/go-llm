//go:build unix

package tools

import (
	"context"
	"crypto/rand"
	"syscall"
	"testing"
	"time"
)

// waitForManagerExit polls status until the job leaves the running state,
// bounded at 10s.
func waitForManagerExit(t *testing.T, m *BackgroundManager, handle string) JobStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, ok := m.status(handle)
		if !ok {
			t.Fatalf("job %q disappeared while awaiting exit", handle)
		}
		if st.State != backgroundStateRunning {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %q still running after 10s", handle)
	return JobStatus{}
}

// TestBackgroundManagerUnixNaturalExitReapsResidualChild drives the real
// platform starter through the manager: the leader exits naturally while a
// same-group child lives on, and the manager must publish the leader's real
// exit while Wait's residual-group cleanup reaps the child.
func TestBackgroundManagerUnixNaturalExitReapsResidualChild(t *testing.T) {
	m := newBackgroundManager(newPlatformStarter(), rand.Reader)
	t.Cleanup(m.Shutdown)
	pidfile := t.TempDir() + "/child.pid"
	spec := helperSpec(t, "orphanleave", pidfile)

	st, err := m.start(context.Background(), spec, spec.Dir)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	childPID := waitForPidfile(t, pidfile)
	// Safety net only; the assertion path must be covered by the manager alone.
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	final := waitForManagerExit(t, m, st.Handle)
	if final.State != backgroundStateExited || !final.ExitKnown || final.ExitCode != 0 {
		t.Errorf("final = %+v, want exited/0/known — natural exit must not be rewritten", final)
	}
	if !waitProcessGone(childPID) {
		t.Errorf("residual child pid %d still alive after natural leader exit", childPID)
	}
}

// TestBackgroundManagerUnixShutdownKillsActiveGroup proves Shutdown kills an
// active real process group — leader and same-group descendant — and returns
// only after the job's completion is published as killed.
func TestBackgroundManagerUnixShutdownKillsActiveGroup(t *testing.T) {
	m := newBackgroundManager(newPlatformStarter(), rand.Reader)
	pidfile := t.TempDir() + "/grandchild.pid"
	spec := helperSpec(t, "groupkill", pidfile)

	st, err := m.start(context.Background(), spec, spec.Dir)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	grandchildPID := waitForPidfile(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(grandchildPID, syscall.SIGKILL) })

	m.Shutdown()
	// Shutdown returned, so completion must already be published: no polling.
	final, ok := m.status(st.Handle)
	if !ok {
		t.Fatal("job missing after Shutdown")
	}
	if final.State != backgroundStateKilled || !final.ExitKnown || final.ExitCode != -1 {
		t.Errorf("final = %+v, want killed/-1/known", final)
	}
	if !waitProcessGone(st.PID) {
		t.Errorf("leader pid %d still alive after Shutdown", st.PID)
	}
	if !waitProcessGone(grandchildPID) {
		t.Errorf("grandchild pid %d still alive after Shutdown", grandchildPID)
	}
}
