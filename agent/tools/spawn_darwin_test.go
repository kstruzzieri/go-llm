//go:build darwin

package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The nativeCmd tests that seed or change Mach or signal state do it in a
// helper process (runMachHelper), so this test binary's own state is untouched.

func TestNativeCmdChildHasNoLaunchdConnection(t *testing.T) {
	runMachHelper(t, "nativedriver", "NATIVEDRIVER-OK")
}

func TestNativeCmdIgnoredSignalStaysIgnored(t *testing.T) {
	runMachHelper(t, "sigdriver", "SIGDRIVER-OK")
}

func TestNativeCmdUnblockedUSR1(t *testing.T) {
	// containment: LAUNCHD-SEATBELT
	runMachHelper(t, "usr1driver", "USR1DRIVER-OK")
}

func TestNativeCmdCloseOnExecDefault(t *testing.T) {
	// containment: LAUNCHD-SEATBELT
	runMachHelper(t, "fddriver", "FDDRIVER-OK")
}

func TestNativeCmdRejectsNUL(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *nativeCmd)
	}{
		{"argv", func(c *nativeCmd) { c.Args = []string{"true", "a\x00b"} }},
		{"env", func(c *nativeCmd) { c.Env = []string{"A=1", "B=x\x00y", "B=2"} }},
		{"path", func(c *nativeCmd) { c.Path = "/usr/bin/true\x00x" }},
		{"dir", func(c *nativeCmd) { c.Dir = t.TempDir() + "\x00x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newNativeCmd(context.Background(), "/usr/bin/true", []string{"true"})
			c.Env = []string{"PATH=/usr/bin:/bin"}
			tc.edit(c)
			err := c.Start()
			if !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("Start err = %v, want EINVAL", err)
			}
			if c.Pid() != 0 {
				t.Fatalf("Pid() = %d after a refused Start, want 0", c.Pid())
			}
		})
	}
}

func TestNativeCmdMissingDir(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	before := openFDs(t)
	c := newNativeCmd(context.Background(), "/usr/bin/true", []string{"true"})
	c.Dir = gone
	c.Stdout, c.Stderr = &bytes.Buffer{}, &bytes.Buffer{} // two pipes to clean up
	err := c.Start()
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("Start err = %v, want ENOENT", err)
	}
	if c.Pid() != 0 {
		t.Fatalf("Pid() = %d after a failed Start, want 0", c.Pid())
	}
	if after := openFDs(t); !slices.Equal(before, after) {
		t.Fatalf("open fds changed across a failed Start: before %v, after %v", before, after)
	}
}

func TestNativeCmdConcurrentSpawns(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			out, _, err := runNative(newNativeCmd(context.Background(), self, []string{self, "__golem_exec_helper__", "machstate"}))
			if err != nil {
				errs[i] = errors.Join(err, errors.New(out))
				return
			}
			st, err := machStateFromOutput(out)
			if err == nil {
				err = noLaunchdConnection(st)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("spawn %d: %v", i, err)
		}
	}
}

func TestNativeCmdStartErrorShape(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	c := newNativeCmd(context.Background(), missing, []string{"x"})
	err := c.Start()
	var pe *os.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("Start err = %#v, want *os.PathError", err)
	}
	if pe.Op != "fork/exec" || pe.Path != missing || pe.Err != syscall.ENOENT {
		t.Fatalf("Start err = {Op %q, Path %q, Err %v}, want {fork/exec, %q, ENOENT}", pe.Op, pe.Path, pe.Err, missing)
	}
	if c.Pid() != 0 {
		t.Fatalf("Pid() = %d after a failed Start, want 0", c.Pid())
	}
}

// A descendant holding stdout past the leader's exit must not hold Wait past
// WaitDelay; the output written before then still arrives.
func TestNativeCmdWaitDelayBoundsHeldPipe(t *testing.T) {
	c := newNativeCmd(context.Background(), "/bin/sh", []string{"sh", "-c", "sleep 30 & echo started"})
	var out bytes.Buffer
	c.Stdout = &out
	c.WaitDelay = 100 * time.Millisecond
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-c.Pid(), syscall.SIGKILL) })
	start := time.Now()
	err := c.Wait()
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Wait err = %v, want exec.ErrWaitDelay", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Wait took %v with a held pipe and a 100ms WaitDelay", d)
	}
	if out.String() != "started\n" || !c.State().Success() {
		t.Fatalf("output %q, state %v: want the leader's output and exit 0", out.String(), c.State())
	}
}

// A Cancel that misses the leader (one that left the group it signals) must
// not leave Wait to the leader's own exit: as with exec.CommandContext, the
// leader is killed once WaitDelay passes after cancellation.
func TestNativeCmdKillsLeaderWaitDelayAfterCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := newNativeCmd(ctx, "/bin/sh", []string{"sh", "-c", "sleep 30"})
	c.Cancel = func() error { return nil }
	c.WaitDelay = 100 * time.Millisecond
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-c.Pid(), syscall.SIGKILL) })
	start := time.Now()
	_ = c.Wait()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Wait took %v after cancel with a 100ms WaitDelay", d)
	}
	ws, ok := c.State().Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("state %v, want killed by SIGKILL", c.State())
	}
}

// Not parallel: it swaps the package-level guard.
func TestNativeCmdFailsClosedWhenRegisteredPortsStay(t *testing.T) {
	orig := registeredPortsGuard
	t.Cleanup(func() { registeredPortsGuard = orig })
	registeredPortsGuard = func() error { return errRegisteredPortsNotEmpty }

	before := openFDs(t)
	c := newNativeCmd(context.Background(), "/usr/bin/true", []string{"true"})
	c.Stdout, c.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := c.Start(); !errors.Is(err, errRegisteredPortsNotEmpty) {
		t.Fatalf("Start err = %v, want errRegisteredPortsNotEmpty", err)
	}
	if c.Pid() != 0 {
		t.Fatalf("Pid() = %d after a refused Start, want 0", c.Pid())
	}
	if after := openFDs(t); !slices.Equal(before, after) {
		t.Fatalf("open fds changed across a refused Start: before %v, after %v", before, after)
	}
}

// openFDs lists this process's open descriptors. A pipe is opened and closed
// first so the runtime's poller descriptor already exists before a count.
func openFDs(t *testing.T) []string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.Close(), w.Close()
	f, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}
