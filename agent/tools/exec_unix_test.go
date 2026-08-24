//go:build unix

package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a hermetic helper binary: when invoked with the sentinel first
// arg, it behaves as the child process the runner spawns, then exits.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__golem_exec_helper__" {
		os.Exit(helperMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// helperMain handles hermetic helper sub-commands:
//
//	["echo", args...]              -> print args to stdout
//	["errto", args...]             -> print args to stderr
//	["fail"]                       -> exit with code 3
//	["dumpenv"]                    -> print env, one key=value per line
//	["sleep"]                      -> sleep 30s (plain sleeper, e.g. for timeout tests)
//	["groupkill", <pidfile>]       -> spawn a grandchild justsleep, write its PID to
//	                                  <pidfile>, then sleep 30s; used to verify that
//	                                  a group-kill reaps the grandchild too
//	["justsleep"]                  -> sleep 30s (grandchild target for groupkill test)
//	["stdinprobe"]                 -> read stdin to EOF, print "stdin:<bytes>"
//	["bothstreams"]                -> print "out-stream" to stdout, "err-stream" to stderr
//	["orphanleave", <pidfile>]     -> spawn a same-group justsleep grandchild with
//	                                  detached stdio (pipes released), write its PID
//	                                  to <pidfile>, exit 0 immediately
//	["holdpipe"]                   -> spawn a same-group justsleep grandchild that
//	                                  INHERITS this stdout pipe, exit 0 immediately
func helperMain(args []string) int {
	if len(args) == 0 {
		return 0
	}
	switch args[0] {
	case "echo":
		_, _ = fmt.Fprintln(os.Stdout, strings.Join(args[1:], " "))
		return 0
	case "errto":
		_, _ = fmt.Fprintln(os.Stderr, strings.Join(args[1:], " "))
		return 0
	case "fail":
		return 3
	case "dumpenv":
		for _, e := range os.Environ() {
			_, _ = fmt.Fprintln(os.Stdout, e)
		}
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	case "groupkill":
		if len(args) < 2 {
			_, _ = fmt.Fprintln(os.Stderr, "groupkill: missing pidfile argument")
			return 1
		}
		pidfile := args[1]
		// Spawn a grandchild that will sleep in the same process group; a
		// group-kill of this process's group must also terminate it.
		self, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "groupkill: os.Executable:", err)
			return 1
		}
		child := exec.Command(self, "__golem_exec_helper__", "justsleep")
		// Explicitly do NOT set Setpgid on the grandchild so it stays in the
		// same process group as this helper and the group-kill from the runner
		// will reach it.
		if err := child.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "groupkill: child.Start:", err)
			return 1
		}
		// Write grandchild PID to the pidfile so the test can read it.
		if err := os.WriteFile(pidfile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "groupkill: WriteFile:", err)
			_ = child.Process.Kill()
			return 1
		}
		// Block until killed (along with the grandchild) by a group-kill.
		time.Sleep(30 * time.Second)
		return 0
	case "justsleep":
		// Grandchild target: just sleep; meant to be killed by a group SIGKILL.
		time.Sleep(30 * time.Second)
		return 0
	case "stdinprobe":
		// Reads stdin to EOF and reports the byte count; proves the nil-stdin
		// contract (os/exec wires /dev/null, so this sees immediate EOF).
		n, err := io.Copy(io.Discard, os.Stdin)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "stdinprobe:", err)
			return 1
		}
		_, _ = fmt.Fprintf(os.Stdout, "stdin:%d\n", n)
		return 0
	case "bothstreams":
		_, _ = fmt.Fprintln(os.Stdout, "out-stream")
		_, _ = fmt.Fprintln(os.Stderr, "err-stream")
		return 0
	case "orphanleave":
		// Natural-leader-exit fixture: spawn a same-group child whose stdio is
		// detached (nil Stdout/Stderr -> /dev/null, so the inherited pipes are
		// released), write its PID to the pidfile, and exit 0 immediately. The
		// child outlives this leader; only a residual group kill can reap it.
		if len(args) < 2 {
			_, _ = fmt.Fprintln(os.Stderr, "orphanleave: missing pidfile argument")
			return 1
		}
		self, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "orphanleave: os.Executable:", err)
			return 1
		}
		child := exec.Command(self, "__golem_exec_helper__", "justsleep")
		// No Setpgid: the child stays in this leader's process group.
		if err := child.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "orphanleave: child.Start:", err)
			return 1
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "orphanleave: WriteFile:", err)
			_ = child.Process.Kill()
			return 1
		}
		return 0
	case "holdpipe":
		// Held-pipe fixture: spawn a same-group child that INHERITS this
		// process's stdout pipe and sleeps 30s, then exit 0 immediately. The
		// write end stays open past leader exit, so only WaitDelay unblocks
		// the parent's Wait.
		self, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "holdpipe: os.Executable:", err)
			return 1
		}
		child := exec.Command(self, "__golem_exec_helper__", "justsleep")
		child.Stdout = os.Stdout // hold the leader's stdout pipe open
		if err := child.Start(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "holdpipe: child.Start:", err)
			return 1
		}
		return 0
	}
	return 0
}

func helperSpec(t *testing.T, args ...string) execSpec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{self, "__golem_exec_helper__"}, args...)
	return execSpec{Path: self, Argv: argv, Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}
}

func TestUnixRunnerEchoAndExit(t *testing.T) {
	r := unixRunner{}
	res, err := r.Run(context.Background(), helperSpec(t, "echo", "hello", "world"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hello world") {
		t.Errorf("exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
}

func TestUnixRunnerNonZeroExit(t *testing.T) {
	r := unixRunner{}
	res, err := r.Run(context.Background(), helperSpec(t, "fail"))
	if err != nil {
		t.Fatalf("non-zero exit must not be a runner error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestUnixRunnerTimeoutKills(t *testing.T) {
	r := unixRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := r.Run(ctx, helperSpec(t, "sleep", "30"))
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !res.TimedOut {
		t.Error("want TimedOut=true")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("kill did not take effect promptly")
	}
}

func TestUnixRunnerEnvIsolation(t *testing.T) {
	r := unixRunner{}
	spec := helperSpec(t, "dumpenv")
	spec.Env = []string{"PATH=/usr/bin:/bin", "HOME=/home/x"}
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	dump := string(res.Stdout)
	if !strings.Contains(dump, "HOME=/home/x") || !strings.Contains(dump, "PATH=") {
		t.Errorf("allowlisted vars missing:\n%s", dump)
	}
	if strings.Contains(dump, "__golem_exec_helper__") {
		t.Error("sentinel leaked into env (should be argv only)")
	}
}

func TestUnixRunnerOutputCap(t *testing.T) {
	r := unixRunner{}
	big := strings.Repeat("x", execStdoutCap+5000)
	res, err := r.Run(context.Background(), helperSpec(t, "echo", big))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > execStdoutCap || !res.StdoutTruncated {
		t.Errorf("len=%d truncated=%v want <=%d and truncated", len(res.Stdout), res.StdoutTruncated, execStdoutCap)
	}
}

// TestUnixRunnerGroupKillReapsChild verifies that the Setpgid + group-kill machinery
// in unixRunner actually reaps grandchildren that share the leader's process group,
// not just the leader itself.
func TestUnixRunnerGroupKillReapsChild(t *testing.T) {
	pidfile := t.TempDir() + "/grandchild.pid"

	r := unixRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Run the "groupkill" helper in the background so we can poll the pidfile
	// while it runs.
	type result struct {
		res execResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.Run(ctx, helperSpec(t, "groupkill", pidfile))
		done <- result{res, err}
	}()

	// Poll for the pidfile (written by the helper after the grandchild starts).
	var grandchildPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidfile)
		if err == nil && len(data) > 0 {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				grandchildPID = pid
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grandchildPID == 0 {
		t.Fatal("timed out waiting for grandchild pidfile to appear")
	}

	// Wait for the runner to finish (context timeout fires, group-kill issued).
	r2 := <-done
	if !r2.res.TimedOut {
		t.Errorf("want TimedOut=true, got TimedOut=%v err=%v", r2.res.TimedOut, r2.err)
	}

	// Poll up to 5s confirming the grandchild is gone (SIGKILL to group must have
	// reached it).  syscall.Kill(pid, 0) returns ESRCH when the process no longer
	// exists (or has been fully reaped by its own parent).
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(grandchildPID, 0)
		if err == syscall.ESRCH {
			return // grandchild is gone — test passes
		}
		time.Sleep(20 * time.Millisecond)
	}
	// One last check to produce a clear failure message.
	if err := syscall.Kill(grandchildPID, 0); err != syscall.ESRCH {
		t.Errorf("grandchild pid %d still alive after group-kill (Kill(pid,0) err=%v)", grandchildPID, err)
	}
}
