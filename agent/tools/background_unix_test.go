//go:build unix

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
	code, err := proc.Wait()
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
	code, err := proc.Wait()
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
	code, err := proc.Wait()
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
	code, err := proc.Wait()
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
	// Assert the grandchild died BEFORE calling Wait: Wait's own C1 residual
	// cleanup also group-kills, and checking afterwards would mask a
	// leader-only Kill.
	if !waitProcessGone(grandchildPID) {
		t.Errorf("grandchild pid %d still alive after group kill", grandchildPID)
	}
	if code, err := proc.Wait(); err != nil || code != -1 {
		t.Errorf("Wait = (%d, %v), want (-1, nil)", code, err)
	}
}

// TestBackgroundProcessNaturalLeaderExitResidualCleanup proves the C1 policy:
// when the leader exits naturally but a same-group descendant lives on with
// released pipes, Wait reports the leader's real exit (0, nil) and its
// internal residual group cleanup reaps the descendant — the test never calls
// Kill on the handle.
func TestBackgroundProcessNaturalLeaderExitResidualCleanup(t *testing.T) {
	pidfile := t.TempDir() + "/child.pid"
	proc, _, _ := startBackground(t, helperSpec(t, "orphanleave", pidfile))
	code, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("Wait = (%d, %v), want (0, nil) — natural exit must not be rewritten", code, err)
	}
	childPID := waitForPidfile(t, pidfile)
	// Safety net only, used after a failed assertion; the assertion path must
	// be covered by Wait's internal residual cleanup alone.
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !waitProcessGone(childPID) {
		t.Errorf("residual child pid %d still alive after Wait", childPID)
	}
}

// TestBackgroundProcessHeldPipeWaitDelay proves WaitDelay bounds Wait when a
// same-group descendant (alive for 30s) holds the leader's stdout pipe open:
// Wait must return within 5s with the leader's real exit code and
// exec.ErrWaitDelay preserved.
func TestBackgroundProcessHeldPipeWaitDelay(t *testing.T) {
	proc, _, _ := startBackground(t, helperSpec(t, "holdpipe"))
	// Registered immediately, but relied on only after a failed assertion.
	t.Cleanup(func() { _ = proc.Kill() })

	type waitResult struct {
		code int
		err  error
	}
	done := make(chan waitResult, 1)
	go func() {
		code, err := proc.Wait()
		done <- waitResult{code, err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, exec.ErrWaitDelay) {
			t.Errorf("Wait err = %v, want exec.ErrWaitDelay preserved", r.err)
		}
		if r.code != 0 {
			t.Errorf("Wait code = %d, want 0 (leader exited cleanly)", r.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s (WaitDelay not honored)")
	}
}
