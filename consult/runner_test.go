//go:build unix

package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
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

// uidHome is the home directory the OS records for this uid. It deliberately
// does not consult $HOME, so an assertion built on it can catch a runner that
// lets the parent environment steer the child's HOME.
func uidHome(t *testing.T) string {
	t.Helper()
	identity, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(identity.HomeDir) {
		t.Fatalf("uid home %q is not absolute", identity.HomeDir)
	}
	return identity.HomeDir
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

// TestNewEnvelopeIsPrivateAndSelfCleaning pins the two envelope invariants the
// run path depends on: every directory is 0700, and a partial failure strands
// nothing behind.
func TestNewEnvelopeIsPrivateAndSelfCleaning(t *testing.T) {
	base := t.TempDir()
	e, err := newEnvelopeIn(base)
	if err != nil {
		t.Fatalf("newEnvelopeIn: %v", err)
	}
	for _, p := range []string{e.root, e.cwd, e.tmp, e.config, e.cache, e.state} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: mode %v, want a 0700 directory", p, info.Mode())
		}
	}
	if err := os.RemoveAll(e.root); err != nil {
		t.Fatal(err)
	}

	// These cases override the package-level envelopeDirs, so no test in this
	// package may call t.Parallel while they run.
	restore := envelopeDirs
	defer func() { envelopeDirs = restore }()
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		// A subdirectory that cannot be created must take the whole root with it.
		{"uncreatable", []string{"cwd", "no-such-parent/child"}},
		// A list that creates fine but leaves an envelope field unset must also
		// fail: a half-built envelope would send the child's XDG dirs to "".
		{"incomplete", []string{"cwd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelopeDirs = tc.dirs
			if _, err := newEnvelopeIn(base); err == nil {
				t.Fatalf("newEnvelopeIn accepted %v", tc.dirs)
			}
			left, err := os.ReadDir(base)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Fatalf("failed envelope stranded %d entries under %s: %v", len(left), base, left)
			}
		})
	}
}

// TestKillGroupMapsESRCHToProcessDone pins cmd.Cancel's contract: a group that
// is already gone is not an interruption failure, and reporting it as one would
// make Go wrap it into an error waitErrorKind classifies "other" (fail-closed).
func TestKillGroupMapsESRCHToProcessDone(t *testing.T) {
	// A short-lived child, reaped, so its pgid is certainly gone.
	cmd := exec.Command(script(t, `exit 0`))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	for i := 0; i < 200 && syscall.Kill(-pid, 0) != syscall.ESRCH; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := syscall.Kill(-pid, 0); got != syscall.ESRCH {
		t.Skipf("pgid %d still present (%v); cannot exercise the ESRCH path", pid, got)
	}
	if err := killGroup(pid); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup on a dead pgid: got %v, want os.ErrProcessDone", err)
	}
}

func TestRunEnvironmentIsExactlyElevenNames(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-child")
	t.Setenv("USER", "wrong-user")
	// HOME is poisoned too: like USER it must come from the uid, never from the
	// parent environment, or a caller could redirect the subscription
	// credentials the consultant reads.
	t.Setenv("HOME", t.TempDir())
	check := `test "$PATH" = /usr/bin:/bin:/usr/sbin:/sbin && test "$USER" = "$(/usr/bin/id -un)" && test "$HOME" = "` + uidHome(t) + `" && test -d "$TMPDIR" && test -d "$XDG_CONFIG_HOME" && test -d "$XDG_CACHE_HOME" && test -d "$XDG_STATE_HOME" && test "${ANTHROPIC_API_KEY-unset}" = unset && test "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" = 1 && test "$CLAUDE_CODE_DISABLE_AUTO_MEMORY" = 1 && test "$ENABLE_CLAUDEAI_MCP_SERVERS" = false && test "$(env | cut -d= -f1 | grep -cEv '^(PATH|LC_ALL|HOME|TMPDIR|USER|XDG_CONFIG_HOME|XDG_CACHE_HOME|XDG_STATE_HOME|CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC|CLAUDE_CODE_DISABLE_AUTO_MEMORY|ENABLE_CLAUDEAI_MCP_SERVERS|PWD|SHLVL|_)$')" = 0`
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
	// len(Stdout) is load-bearing: the cap must clamp what is retained, not just
	// count it, or an 8 KiB flood under a 1 KiB cap still reaches the caller.
	if !out.CapExceeded || out.WaitStatus != "signaled(SIGKILL)" || out.Duration > 4500*time.Millisecond || len(out.Stdout) > 1024 {
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
	// Duration is load-bearing: Canceled reads the caller's context, which is
	// canceled whether or not the cancellation ever reached the child, so
	// without this bound the 5s deadline could be doing the killing.
	if !out.Canceled || out.WaitStatus != "signaled(SIGKILL)" || !out.GroupCleanupOK || out.Duration > 4500*time.Millisecond {
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
	// Positive path: a genuinely cancelled caller sets Canceled and the run
	// ends well inside its own 5s deadline.
	sc := script(t, `sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	out, _ = run(ctx, runSpec{command: sc, timeout: 5 * time.Second, outputCap: 1024})
	if !out.Canceled || out.TimedOut || out.Duration > 4500*time.Millisecond {
		t.Fatalf("caller cancellation not honoured: %+v", out)
	}
}

// TestRunErrorsAreClassifiable pins the sentinels Task 5 classifies on; message
// text is not a contract, errors.Is is.
func TestRunErrorsAreClassifiable(t *testing.T) {
	ok := script(t, `exit 0`)
	base := runSpec{command: ok, timeout: time.Second, outputCap: 1024}

	oversized := base
	oversized.stdin = strings.Repeat("x", 65537)
	if _, err := run(context.Background(), oversized); !errors.Is(err, errStdinInvalid) {
		t.Fatalf("oversized stdin: got %v, want errStdinInvalid", err)
	}
	badUTF8 := base
	badUTF8.stdin = "bad\xff"
	if _, err := run(context.Background(), badUTF8); !errors.Is(err, errStdinInvalid) {
		t.Fatalf("invalid UTF-8 stdin: got %v, want errStdinInvalid", err)
	}
	link := ok + ".link"
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	symlinked := base
	symlinked.command = link
	if _, err := run(context.Background(), symlinked); !errors.Is(err, errTargetInvalid) {
		t.Fatalf("symlink leaf: got %v, want errTargetInvalid", err)
	}
	drifted := base
	drifted.sha256 = strings.Repeat("0", 64)
	if _, err := run(context.Background(), drifted); !errors.Is(err, errTargetDrift) {
		t.Fatalf("digest mismatch: got %v, want errTargetDrift", err)
	}
}

// TestRunStderrOverflowAbortsTheRun proves the cap is enforced on stderr too:
// a child that only floods stderr is still killed.
func TestRunStderrOverflowAbortsTheRun(t *testing.T) {
	out, _ := run(context.Background(), runSpec{command: script(t, `head -c 8192 /dev/zero | tr '\0' x 1>&2; sleep 30`), timeout: 5 * time.Second, outputCap: 1024})
	if !out.CapExceeded || out.WaitStatus != "signaled(SIGKILL)" || out.Duration > 4500*time.Millisecond {
		t.Fatalf("stderr flood did not abort the run: %+v", out)
	}
	if len(out.Stdout) != 0 {
		t.Fatalf("stderr content reached Stdout: %q", out.Stdout)
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
	// A one-nibble difference in the last position must be caught: the compare
	// is over the whole digest, not a prefix.
	nearMiss := good[:len(good)-1] + map[bool]string{true: "1", false: "0"}[good[len(good)-1] == '0']
	if _, err := run(context.Background(), runSpec{command: p, sha256: nearMiss, timeout: time.Second, outputCap: 1024}); !errors.Is(err, errTargetDrift) {
		t.Fatalf("last-nibble digest difference accepted: %v", err)
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
