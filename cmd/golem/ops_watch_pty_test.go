//go:build linux || darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

func TestOpsWatchRealPTYRestoresScreen(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := openPTY(t)
	seen := drainMaster(p)
	f := opsfixture.NewLlamaSwap(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runOps(ctx, []string{"-watch", "-config", fixtureConfig(t, f.URL())}, nil, p.slave, p.slave); err != nil {
		t.Fatalf("runOps -watch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(seen.String(), "\x1b[?1049l") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := seen.String()
	if !strings.Contains(got, "\x1b[?1049h") || !strings.Contains(got, "\x1b[?1049l") || !strings.Contains(got, "gemma4:31b") {
		t.Fatalf("PTY output missing enter/frame/restore:\n%q", got)
	}
	// Residency memory needs the watch interval; a one-shot source has none.
	if !strings.Contains(got, "loaded: first observed") {
		t.Fatalf("PTY frame has no residency memory:\n%q", got)
	}
}

// TestOpsWatchRealPTYFollowsResize resizes the real PTY to 40x8 while -watch
// runs, so the frame size comes from the descriptor runOps was given, read
// again on every frame.
func TestOpsWatchRealPTYFollowsResize(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := openPTY(t)
	seen := drainMaster(p)
	f := opsfixture.NewLlamaSwap(t)
	cfg := fixtureConfig(t, f.URL())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runOps(ctx, []string{"-watch", "-config", cfg}, nil, p.slave, p.slave) }()
	waitSeen(seen, func(s string) bool { return strings.Contains(s, "loaded: first observed") })
	if err := unix.IoctlSetWinsize(int(p.slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 8, Col: 40}); err != nil {
		t.Fatalf("TIOCSWINSZ: %v", err)
	}
	// The frame after the next one began strictly after the resize.
	after := strings.Count(seen.String(), clearHome) + 1
	waitSeen(seen, func(s string) bool { return strings.Count(s, clearHome) > after })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runOps -watch: %v", err)
	}
	waitSeen(seen, func(s string) bool { return strings.HasSuffix(s, altScreenOff) })
	frames := strings.Split(strings.TrimSuffix(seen.String(), altScreenOff), clearHome)
	if len(frames) <= after+1 {
		t.Fatalf("saw %d frames, want more than %d:\n%q", len(frames)-1, after, seen.String())
	}
	if cols := maxCols(frames[1]); cols != 80 {
		t.Fatalf("control: first frame is %d columns wide, want 80 (the clip at the opening size)", cols)
	}
	for i, frame := range frames[after+1:] {
		frame = strings.ReplaceAll(frame, "\r\n", "\n") // the PTY's ONLCR
		if cols, lines := maxCols(frame), strings.Count(frame, "\n"); cols > 40 || lines > 7 {
			t.Fatalf("frame %d after the resize is %d columns x %d lines, want at most 40 x 7:\n%q", after+1+i, cols, lines, frame)
		}
	}
}

// maxCols is the longest line in runes; a frame's lines are ASCII but for
// the one-column clip marker.
func maxCols(frame string) int {
	n := 0
	for _, line := range strings.Split(strings.ReplaceAll(frame, "\r\n", "\n"), "\n") {
		n = max(n, utf8.RuneCountInString(line))
	}
	return n
}

// TestOpsWatchRealPTYRestoresOnSignal signals the test process while -watch
// draws on a real PTY. The test holds its own registration for the signal,
// so one runOps fails to catch is harmless and trips the deadline below
// instead of killing the test binary.
func TestOpsWatchRealPTYRestoresOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			held := make(chan os.Signal, 1)
			signal.Notify(held, sig)
			defer signal.Stop(held)
			p := openPTY(t)
			seen := drainMaster(p)
			f := opsfixture.NewLlamaSwap(t)
			cfg := fixtureConfig(t, f.URL())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runOps(ctx, []string{"-watch", "-config", cfg}, nil, p.slave, p.slave) }()
			// A drawn frame means runOps registered its handler first.
			waitSeen(seen, func(s string) bool { return strings.Contains(s, "gemma4:31b") })
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runOps -watch after %v: %v", sig, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not end -watch", sig)
			}
			waitSeen(seen, func(s string) bool { return strings.HasSuffix(s, altScreenOff) })
			if got := seen.String(); !strings.Contains(got, "gemma4:31b") || !strings.HasSuffix(got, altScreenOff) {
				t.Fatalf("after %v the PTY did not end restored (cursor shown, main screen):\n%q", sig, got)
			}
		})
	}
}

// waitSeen polls the PTY output for up to five seconds.
func waitSeen(seen *lockedBuffer, ok func(string) bool) {
	deadline := time.Now().Add(5 * time.Second)
	for !ok(seen.String()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// jobHelper re-runs this test as a helper in mode, in the process group or
// session attr makes, so the test binary itself is never stopped, and returns
// once the helper stops or exits.
func jobHelper(t *testing.T, mode string, attr *syscall.SysProcAttr) (*exec.Cmd, *lockedBuffer, syscall.WaitStatus) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "GOLEM_WATCH_JOB_HELPER="+mode)
	cmd.SysProcAttr = attr
	out := &lockedBuffer{}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killer := time.AfterFunc(10*time.Second, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() { killer.Stop() })
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED, nil); err != nil {
		t.Fatal(err)
	}
	return cmd, out, ws
}

// stoppedBy decodes a stop by hand: darwin's WaitStatus.Stopped reads a
// SIGSTOP stop as a continue.
func stoppedBy(ws syscall.WaitStatus) (syscall.Signal, bool) {
	return syscall.Signal(ws >> 8 & 0xff), ws&0x7f == 0x7f
}

// TestOpsWatchJobStopsAndResumes runs the real watchJob in a helper in its
// own process group under this test's session: Ctrl-Z is caught rather than
// stopping the helper, suspend stops it with SIGSTOP (a re-raised SIGTSTP
// would be ignored once Go has caught it) and returns only after SIGCONT.
func TestOpsWatchJobStopsAndResumes(t *testing.T) {
	if os.Getenv("GOLEM_WATCH_JOB_HELPER") == "group" {
		job, stop := watchJob()
		defer stop()
		_ = syscall.Kill(os.Getpid(), syscall.SIGTSTP)
		<-job.stops
		fmt.Println("caught")
		job.suspend()
		fmt.Println("resumed")
		return
	}
	cmd, out, ws := jobHelper(t, "group", &syscall.SysProcAttr{Setpgid: true})
	if sig, stopped := stoppedBy(ws); !stopped || sig != syscall.SIGSTOP {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not stop itself with SIGSTOP (status %#x):\n%s", ws, out.String())
	}
	time.Sleep(200 * time.Millisecond) // anything written before the stop has arrived
	if strings.Contains(out.String(), "resumed") {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("suspend returned before the stop landed:\n%s", out.String())
	}
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "caught\nresumed\n") {
		t.Fatalf("helper after SIGCONT: %v:\n%s", err, out.String())
	}
}

// TestOpsWatchJobIgnoredWithoutJobControl runs -watch with the real watchJob
// in a helper that leads its own session, as under tmux new-window, ssh -t or
// a terminal emulator's -e: no shell would ever continue it, so Ctrl-Z must
// neither stop it nor interrupt the drawing.
func TestOpsWatchJobIgnoredWithoutJobControl(t *testing.T) {
	if os.Getenv("GOLEM_WATCH_JOB_HELPER") == "session" {
		job, stop := watchJob()
		defer stop()
		f := opsfixture.NewLlamaSwap(t)
		src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_ = syscall.Kill(os.Getpid(), syscall.SIGTSTP)
		if err := runOpsWatch(ctx, src, os.Stdout, 1, opsTerm{terminal: true, w: 80, h: 24}, xterm, job); err != nil {
			t.Fatal(err)
		}
		return
	}
	cmd, out, ws := jobHelper(t, "session", &syscall.SysProcAttr{Setsid: true})
	if _, stopped := stoppedBy(ws); stopped {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper leading its own session stopped on Ctrl-Z (status %#x); nothing would continue it", ws)
	}
	_ = cmd.Wait() // reaped above; this only drains the output
	if !ws.Exited() || ws.ExitStatus() != 0 || strings.Count(out.String(), clearHome) < 2 {
		t.Fatalf("helper did not keep drawing through Ctrl-Z (status %#x):\n%q", ws, out.String())
	}
}
