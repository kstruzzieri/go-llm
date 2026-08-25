//go:build linux || darwin

package tools

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startBackground starts a helper spec through the real platform starter with
// fresh tail rings as the stdout/stderr writers, exercising the actual
// background wiring.
func startBackground(t *testing.T, spec execSpec) (backgroundProcess, *tailRing, *tailRing) {
	t.Helper()
	outRing := newTailRing(64 * 1024)
	errRing := newTailRing(64 * 1024)
	proc, err := newPlatformStarter().Start(spec, outRing, errRing)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return proc, outRing, errRing
}

// waitForPidfile polls until the helper-written pidfile appears and parses it,
// bounded at 5s.
func waitForPidfile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pidfile %s", path)
	return 0
}

// waitProcessGone polls until the PID no longer exists (Kill(pid,0) == ESRCH),
// bounded at 5s.
func waitProcessGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// TestBackgroundProcessNilStdinEOF proves direct execution with the nil-stdin
// contract: the child sees /dev/null and reads immediate EOF instead of
// hanging on input.
func TestBackgroundProcessNilStdinEOF(t *testing.T) {
	proc, outRing, _ := startBackground(t, helperSpec(t, "stdinprobe"))
	if proc.PID() <= 0 {
		t.Errorf("PID = %d, want > 0", proc.PID())
	}
	code, _, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v), want (0, nil)", code, err)
	}
	out, _, _ := outRing.Read(nil, 1024)
	if !strings.Contains(string(out), "stdin:0") {
		t.Errorf("stdout = %q, want immediate stdin EOF (stdin:0)", out)
	}
}

// TestBackgroundProcessSeparateStreams proves stdout and stderr are captured
// into their own writers without crosstalk.
func TestBackgroundProcessSeparateStreams(t *testing.T) {
	proc, outRing, errRing := startBackground(t, helperSpec(t, "bothstreams"))
	code, _, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v), want (0, nil)", code, err)
	}
	out, _, _ := outRing.Read(nil, 1024)
	errOut, _, _ := errRing.Read(nil, 1024)
	if !strings.Contains(string(out), "out-stream") || strings.Contains(string(out), "err-stream") {
		t.Errorf("stdout ring = %q, want out-stream only", out)
	}
	if !strings.Contains(string(errOut), "err-stream") || strings.Contains(string(errOut), "out-stream") {
		t.Errorf("stderr ring = %q, want err-stream only", errOut)
	}
}

// TestBackgroundProcessNonZeroExit proves a non-zero exit is a normal
// completion (real code, nil error), never an infra error.
func TestBackgroundProcessNonZeroExit(t *testing.T) {
	proc, _, _ := startBackground(t, helperSpec(t, "fail"))
	code, _, err := proc.Wait()
	if err != nil {
		t.Fatalf("non-zero exit must not be an error: %v", err)
	}
	if code != 3 {
		t.Errorf("code = %d, want 3", code)
	}
}

// TestBackgroundProcessGroupAssignmentAndKill proves the leader runs in its
// own process group, Kill terminates it (reported as -1, nil), and a second
// Kill on the already-gone group still succeeds (idempotency).
func TestBackgroundProcessGroupAssignmentAndKill(t *testing.T) {
	proc, _, _ := startBackground(t, helperSpec(t, "justsleep"))
	t.Cleanup(func() { _ = proc.Kill() })
	pgid, err := syscall.Getpgid(proc.PID())
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", proc.PID(), err)
	}
	if pgid != proc.PID() {
		t.Errorf("pgid = %d, want %d (leader of its own group)", pgid, proc.PID())
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	code, _, err := proc.Wait()
	if err != nil {
		t.Fatalf("Wait after Kill: %v", err)
	}
	if code != -1 {
		t.Errorf("code = %d, want -1 (killed by signal)", code)
	}
	if err := proc.Kill(); err != nil {
		t.Errorf("second Kill on gone group = %v, want nil", err)
	}
}

// TestBackgroundProcessGroupKillReapsChild proves Kill reaches same-group
// descendants, not just the leader.
func TestBackgroundProcessGroupKillReapsChild(t *testing.T) {
	pidfile := t.TempDir() + "/grandchild.pid"
	proc, _, _ := startBackground(t, helperSpec(t, "groupkill", pidfile))
	t.Cleanup(func() { _ = proc.Kill() })
	grandchildPID := waitForPidfile(t, pidfile)
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Assert the grandchild died BEFORE calling Wait: Wait's own residual
	// group cleanup also group-kills, and checking afterwards would mask a
	// leader-only Kill.
	if !waitProcessGone(grandchildPID) {
		t.Errorf("grandchild pid %d still alive after group kill", grandchildPID)
	}
	if code, _, err := proc.Wait(); err != nil || code != -1 {
		t.Errorf("Wait = (%d, %v), want (-1, nil)", code, err)
	}
}

// TestBackgroundProcessKillZombieWindow reproduces the darwin stop-vs-reap
// window: the leader is dead but not yet reaped (no Wait call yet), so the
// group still exists with no signalable member and kill(-pgid) returns EPERM
// on darwin (linux returns 0 there). Kill must treat that as "nothing left to
// kill" and report success.
func TestBackgroundProcessKillZombieWindow(t *testing.T) {
	proc, _, _ := startBackground(t, helperSpec(t, "justsleep"))
	// Kill the leader directly (positive pid) so it becomes a zombie: dead,
	// unreaped, sole member of its group.
	if err := syscall.Kill(proc.PID(), syscall.SIGKILL); err != nil {
		t.Fatalf("kill leader: %v", err)
	}
	// Poll until the group probe reports the zombie-only state (non-nil from
	// kill(-pgid, 0)). On linux the probe stays nil for a zombie-only group;
	// the deadline fall-through keeps the Kill assertion meaningful there too.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-proc.PID(), 0) != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill in zombie window = %v, want nil", err)
	}
	if code, _, err := proc.Wait(); err != nil || code != -1 {
		t.Errorf("Wait = (%d, %v), want (-1, nil)", code, err)
	}
}

// TestBackgroundProcessKillConcurrentWithWait pins the manager's primary
// usage under -race: Kill racing Wait is safe (Kill touches only the
// remembered pgid, never cmd internals) and the killed leader reports
// (-1, nil). The second Kill may land in the zombie or reaped window.
func TestBackgroundProcessKillConcurrentWithWait(t *testing.T) {
	proc, _, _ := startBackground(t, helperSpec(t, "justsleep"))
	type waitResult struct {
		code int
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		code, _, err := proc.Wait()
		done <- waitResult{code, err}
	}()
	if err := proc.Kill(); err != nil {
		t.Errorf("first Kill: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Errorf("second Kill: %v", err)
	}
	r := <-done
	if r.err != nil || r.code != -1 {
		t.Errorf("Wait = (%d, %v), want (-1, nil)", r.code, r.err)
	}
}

// TestBackgroundProcessNaturalLeaderExitResidualCleanup proves the
// managed-process-group containment policy:
// when the leader exits naturally but a same-group descendant lives on with
// released pipes, Wait reports the leader's real exit (0, nil) and its
// internal residual group cleanup reaps the descendant — the test never calls
// Kill on the handle.
func TestBackgroundProcessNaturalLeaderExitResidualCleanup(t *testing.T) {
	pidfile := t.TempDir() + "/child.pid"
	proc, _, _ := startBackground(t, helperSpec(t, "orphanleave", pidfile))
	// Learn the grandchild PID and register its safety net BEFORE the Wait
	// assertions, so a Fatalf there cannot leak the 30s sleeper. The net is
	// used only after a failed assertion; the assertion path must be covered
	// by Wait's internal residual cleanup alone.
	childPID := waitForPidfile(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	code, _, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v), want (0, nil) — natural exit must not be rewritten", code, err)
	}
	if !waitProcessGone(childPID) {
		t.Errorf("residual child pid %d still alive after Wait", childPID)
	}
}

// TestBackgroundProcessHeldPipeCleanedBeforeReap proves natural-exit cleanup
// signals the still-pinned group before cmd.Wait reaps the leader. Killing the
// same-group pipe holder lets Wait return without reaching its 2s WaitDelay.
func TestBackgroundProcessHeldPipeCleanedBeforeReap(t *testing.T) {
	pidfile := t.TempDir() + "/holder.pid"
	proc, _, _ := startBackground(t, execSpec{
		Path: "/bin/sh",
		Argv: []string{"/bin/sh", "-c", `sleep 30 & printf '%s' "$!" > "$1"`, "holdpipe", pidfile},
		Dir:  t.TempDir(),
		Env:  []string{"PATH=/usr/bin:/bin"},
	})
	t.Cleanup(func() { _ = proc.Kill() })
	holderPID := waitForPidfile(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(holderPID, syscall.SIGKILL) })

	type waitResult struct {
		code int
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		code, _, err := proc.Wait()
		done <- waitResult{code, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Errorf("Wait err = %v, want nil after pre-reap group cleanup", r.err)
		}
		if r.code != 0 {
			t.Errorf("Wait code = %d, want 0 (leader exited cleanly)", r.code)
		}
	case <-time.After(execWaitDelay / 2):
		t.Fatal("Wait reached the held-pipe delay; residual cleanup happened after reap")
	}
}

func TestBackgroundProcessEscapedPipeStillHonorsWaitDelay(t *testing.T) {
	pidfile := t.TempDir() + "/escaped.pid"
	proc, _, _ := startBackground(t, helperSpec(t, "holdpipeescape", pidfile))
	escapedPID := waitForPidfile(t, pidfile)
	t.Cleanup(func() { _ = syscall.Kill(escapedPID, syscall.SIGKILL) })

	code, _, err := proc.Wait()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("Wait err = %v, want exec.ErrWaitDelay for an escaped pipe holder", err)
	}
	if code != 0 {
		t.Errorf("Wait code = %d, want 0 (leader exited cleanly)", code)
	}
}
