//go:build unix

package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustHome(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// waitForDeath fails unless the pid recorded in pidFile is gone within 2s.
func waitForDeath(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived the group kill", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunEnvironmentIsExactlyElevenNames(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-child")
	t.Setenv("USER", "wrong-user")
	check := `test "$USER" = "$(/usr/bin/id -un)" && test "$HOME" = "` + mustHome(t) + `" && test -d "$TMPDIR" && test -d "$XDG_CONFIG_HOME" && test -d "$XDG_CACHE_HOME" && test -d "$XDG_STATE_HOME" && test "${ANTHROPIC_API_KEY-unset}" = unset && test "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" = 1 && test "$CLAUDE_CODE_DISABLE_AUTO_MEMORY" = 1 && test "$ENABLE_CLAUDEAI_MCP_SERVERS" = false && test "$(env | cut -d= -f1 | grep -cEv '^(PATH|LC_ALL|HOME|TMPDIR|USER|XDG_CONFIG_HOME|XDG_CACHE_HOME|XDG_STATE_HOME|CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC|CLAUDE_CODE_DISABLE_AUTO_MEMORY|ENABLE_CLAUDEAI_MCP_SERVERS|PWD|SHLVL|_)$')" = 0`
	out, err := run(context.Background(), runSpec{command: script(t, check), timeout: 5 * time.Second, outputCap: 4096})
	if err != nil || out.ExitCode != 0 || !out.CleanupOK || !out.GroupCleanupOK {
		t.Fatalf("env boundary: %+v %v", out, err)
	}
}

func TestRunFailsClosedOnIncompleteDrain(t *testing.T) {
	out, err := run(context.Background(), runSpec{command: script(t, `echo hi; sleep 5 & exit 0`), timeout: 5 * time.Second, outputCap: 4096, waitDelay: 200 * time.Millisecond})
	if err != nil || out.ExitCode != 0 || out.WaitErrorKind != "wait-delay" {
		t.Fatalf("held pipe not detected: %+v %v", out, err)
	}
}

func TestRunCapDeadlineAndCancelKillGroup(t *testing.T) {
	out, _ := run(context.Background(), runSpec{command: script(t, `head -c 8192 /dev/zero | tr '\0' x; sleep 30`), timeout: 5 * time.Second, outputCap: 1024})
	if !out.CapExceeded || out.WaitStatus != "signaled(SIGKILL)" || out.Duration > 3*time.Second {
		t.Fatalf("cap did not kill: %+v", out)
	}
	out, _ = run(context.Background(), runSpec{command: script(t, `sleep 30`), timeout: 300 * time.Millisecond, outputCap: 1024})
	if !out.TimedOut || out.WaitStatus != "signaled(SIGKILL)" {
		t.Fatalf("deadline did not kill: %+v", out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// 1s, not 200ms: a freshly written script costs ~200ms to exec on macOS
	// (first-exec scan), and cancelling inside that window would prove nothing
	// about descendants because the child has not forked one yet.
	time.AfterFunc(time.Second, cancel)
	pidFile := filepath.Join(t.TempDir(), "pid")
	out, _ = run(ctx, runSpec{command: script(t, `sleep 30 & echo $! > `+pidFile+`; sleep 30`), timeout: 5 * time.Second, outputCap: 1024})
	if !out.Canceled || out.WaitStatus != "signaled(SIGKILL)" || !out.GroupCleanupOK {
		t.Fatalf("cancel did not kill group: %+v", out)
	}
	waitForDeath(t, pidFile)
}

// TestRunCanceledDistinguishesCallerFromDeadline pins Canceled to the caller's
// context: an internally cancelled run (cap, deadline) must not report it.
func TestRunCanceledDistinguishesCallerFromDeadline(t *testing.T) {
	out, _ := run(context.Background(), runSpec{command: script(t, `head -c 8192 /dev/zero | tr '\0' x; sleep 30`), timeout: 5 * time.Second, outputCap: 1024})
	if !out.CapExceeded || out.Canceled {
		t.Fatalf("cap run reported caller cancellation: %+v", out)
	}
	out, _ = run(context.Background(), runSpec{command: script(t, `sleep 30`), timeout: 300 * time.Millisecond, outputCap: 1024})
	if !out.TimedOut || out.Canceled {
		t.Fatalf("timed-out run reported caller cancellation: %+v", out)
	}
}

// TestRunKillsLingeringGroupAfterNormalExit covers the post-exit group SIGKILL:
// the leader exits 0 while a same-group descendant still holds the pipe.
func TestRunKillsLingeringGroupAfterNormalExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	out, err := run(context.Background(), runSpec{command: script(t, `sleep 30 & echo $! > `+pidFile+`; exit 0`), timeout: 5 * time.Second, outputCap: 4096, waitDelay: 200 * time.Millisecond})
	if err != nil || out.ExitCode != 0 || !out.GroupCleanupOK {
		t.Fatalf("lingering group not reaped: %+v %v", out, err)
	}
	waitForDeath(t, pidFile)
}

func TestRunVerifiesTargetImmediatelyBeforeExec(t *testing.T) {
	p := script(t, `exit 0`)
	good := sha256File(t, p)
	if out, err := run(context.Background(), runSpec{command: p, sha256: good, timeout: time.Second, outputCap: 1024}); err != nil || out.ExitCode != 0 {
		t.Fatalf("verified target refused: %+v %v", out, err)
	}
	if err := os.WriteFile(p+".new", []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".new", p); err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background(), runSpec{command: p, sha256: good, timeout: time.Second, outputCap: 1024}); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("replaced target executed: %v", err)
	}
	link := p + ".link"
	_ = os.Symlink(p, link)
	if _, err := run(context.Background(), runSpec{command: link, timeout: time.Second, outputCap: 1024}); err == nil {
		t.Fatal("symlink executed")
	}
}

func TestRunRejectsOversizedOrInvalidStdin(t *testing.T) {
	for _, in := range []string{strings.Repeat("x", 65537), "bad\xff"} {
		if _, err := run(context.Background(), runSpec{command: script(t, `exit 0`), stdin: in, timeout: time.Second, outputCap: 1024}); err == nil {
			t.Fatal("bad stdin accepted")
		}
	}
}

func TestRunStderrIsCountedNotRetained(t *testing.T) {
	out, err := run(context.Background(), runSpec{command: script(t, `echo SENSITIVE >&2; echo ok`), timeout: 5 * time.Second, outputCap: 4096})
	if err != nil || out.StderrBytes == 0 || string(out.Stdout) != "ok\n" {
		t.Fatalf("stderr accounting: %+v %v", out, err)
	}
	if s := fmt.Sprintf("%+v", out); strings.Contains(s, "SENSITIVE") {
		t.Fatalf("stderr content retained in the outcome: %s", s)
	}
}

func TestRunNonzeroExitIsReported(t *testing.T) {
	out, err := run(context.Background(), runSpec{command: script(t, `exit 3`), timeout: 5 * time.Second, outputCap: 1024})
	if err != nil || out.ExitCode != 3 || out.WaitStatus != "exited(3)" || out.WaitErrorKind != "exit" {
		t.Fatalf("nonzero exit: %+v %v", out, err)
	}
}

func TestRunCwdIsPrivateAndRemoved(t *testing.T) {
	out, err := run(context.Background(), runSpec{command: script(t, `printf '%s' "$PWD"`), timeout: 5 * time.Second, outputCap: 4096})
	if err != nil || out.ExitCode != 0 || !out.CleanupOK {
		t.Fatalf("cwd run: %+v %v", out, err)
	}
	got := string(out.Stdout)
	if got == "" {
		t.Fatal("child reported no cwd")
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("private cwd %s survived: %v", got, err)
	}
	found := false
	for _, c := range out.Cwds {
		if c == got {
			found = true
		}
	}
	if !found {
		t.Fatalf("cwds %v do not include the child's %s", out.Cwds, got)
	}
}

func TestRunArgsArePassedVerbatim(t *testing.T) {
	out, err := run(context.Background(), runSpec{command: script(t, `printf '%s\n%s\n' "$#" "$2"`), args: []string{"-p", "--x", ""}, timeout: 5 * time.Second, outputCap: 4096})
	if err != nil || string(out.Stdout) != "3\n--x\n" {
		t.Fatalf("argv: %q %+v %v", out.Stdout, out, err)
	}
}
