//go:build darwin

package tools

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startZombieChild starts a command that writes to both streams and exits 3,
// and returns only once it is exited-but-unreaped. That is the state a fast
// background command reaches while unixStarter.Start is still on its way to
// registering the exit observer, and the state darwin's EVFILT_PROC refuses to
// attach to. The returned cmd is unreaped: the caller owns its Wait.
func startZombieChild(t *testing.T, stdout, stderr *tailRing) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "echo out-stream; echo err-stream >&2; exit 3")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	// Mirror unixStarter.Start: own process group, non-*os.File stdio so
	// os/exec wires real pipes, bounded Wait.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	// SZOMB from <sys/proc.h>; x/sys/unix does not export the p_stat values.
	// Polling kill(pid, 0) would not do: it reports nil for a zombie, which is
	// precisely the state kqueue disagrees about.
	const szomb = 5
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := unix.SysctlKinfoProc("kern.proc.pid", cmd.Process.Pid)
		if err == nil && info.Proc.P_stat == szomb {
			return cmd
		}
		if err != nil {
			t.Fatalf("sysctl kern.proc.pid %d: %v", cmd.Process.Pid, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Still running: reap it here rather than leaking a live process. No
	// cleanup is registered for the zombie path — signalling the group after
	// the reap could reach a recycled PGID.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	t.Fatalf("timed out waiting for pid %d to become a zombie", cmd.Process.Pid)
	return nil
}

// TestProcessExitObserverAlreadyExitedChildRecovers pins the #481 fix: a child
// that exits before its observer registers must not fail the start, and both
// its real exit status and its buffered output must survive the normal reap.
// Registration reports ESRCH there, which used to fail the whole start closed
// with "observe background process exit: no such process".
func TestProcessExitObserverAlreadyExitedChildRecovers(t *testing.T) {
	outRing, errRing := newTailRing(64*1024), newTailRing(64*1024)
	cmd := startZombieChild(t, outRing, errRing)
	pid := cmd.Process.Pid

	observer, err := newProcessExitObserver(pid)
	if err != nil {
		t.Fatalf("newProcessExitObserver on an already-exited child = %v, want the already-exited resolution", err)
	}

	// The exit this observer would report already happened, so Wait must
	// return rather than block on a notification that can no longer arrive.
	proc := &unixProcess{cmd: cmd, pgid: pid, observer: observer, group: processGroupGuard{pgid: pid}}
	type result struct {
		code          int
		managerKilled bool
		err           error
	}
	done := make(chan result, 1)
	go func() {
		code, managerKilled, waitErr := proc.Wait()
		done <- result{code, managerKilled, waitErr}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Wait err = %v, want nil", got.err)
		}
		if got.code != 3 {
			t.Errorf("Wait code = %d, want 3 (the child's real exit status)", got.code)
		}
		if got.managerKilled {
			t.Error("managerKilled = true, want false for a natural exit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked on an exit that had already happened")
	}

	out, _, _ := outRing.Read(nil, 1024)
	errOut, _, _ := errRing.Read(nil, 1024)
	if !strings.Contains(string(out), "out-stream") {
		t.Errorf("stdout ring = %q, want the child's output preserved through the recovered start", out)
	}
	if !strings.Contains(string(errOut), "err-stream") {
		t.Errorf("stderr ring = %q, want the child's output preserved through the recovered start", errOut)
	}
}

// TestProcessExitObserverRegistrationFailsClosed pins the other half of the
// contract: only ESRCH means already-exited. Every other registration errno
// must still fail the start, so no background process is ever published with
// nothing watching its exit.
func TestProcessExitObserverRegistrationFailsClosed(t *testing.T) {
	for _, errno := range []syscall.Errno{
		unix.EACCES, unix.EPERM, unix.EINVAL, unix.EBADF, unix.ENOMEM, unix.EMFILE,
	} {
		observer, err := observerForRegistrationError(errno)
		if !errors.Is(err, errno) {
			t.Errorf("observerForRegistrationError(%v) err = %v, want %v", errno, err, errno)
		}
		if observer != nil {
			t.Errorf("observerForRegistrationError(%v) observer = %v, want nil", errno, observer)
		}
	}
	observer, err := observerForRegistrationError(unix.ESRCH)
	if err != nil || observer == nil {
		t.Fatalf("observerForRegistrationError(ESRCH) = (%v, %v), want a resolved observer and nil error", observer, err)
	}
	if waitErr := observer.Wait(); waitErr != nil {
		t.Errorf("resolved observer Wait = %v, want nil", waitErr)
	}
	if closeErr := observer.Close(); closeErr != nil {
		t.Errorf("resolved observer Close = %v, want nil", closeErr)
	}
}
