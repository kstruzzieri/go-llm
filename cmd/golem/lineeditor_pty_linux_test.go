//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This is the required real-application terminal lifecycle gate (spec 13.2).
// The fake termOps suite proves exact call order; only this proves that a real
// termios, put into raw mode by the real x/term and restored by production
// teardown, comes back byte-identical on every exit path the REPL has.
//
// Calling MakeRaw and Restore directly on a PTY would only retest x/term. Every
// case here therefore runs the whole production stack: newInput selects the
// source, replControl owns interrupt policy, withLineSource owns Close, and
// runREPL is the loop.
//
// Linux only, by build tag. macOS needs posix_openpt, which x/sys does not
// wrap; the development host keeps the fake call-order coverage and this runs
// in Docker and GitHub CI.

// ptyPair is a master/slave PTY pair. The slave stands in for the process's
// real stdin and stdout, so the editor's descriptors are genuine terminals.
type ptyPair struct {
	master *os.File
	slave  *os.File
}

func openPTY(t *testing.T) *ptyPair {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	// Unlock before opening the slave; the pair is locked at creation.
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("TIOCSPTLCK: %v", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("TIOCGPTN: %v", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = unix.Close(mfd)
		t.Fatalf("open %s: %v", slavePath, err)
	}
	// A fresh PTY has no window size. x/term would then wrap after every
	// column-zero character and the prompt would arrive as one rune per line,
	// which no real terminal does. Give it ordinary dimensions so rendering is
	// representative.
	if err := unix.IoctlSetWinsize(sfd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		_ = unix.Close(mfd)
		_ = unix.Close(sfd)
		t.Fatalf("TIOCSWINSZ: %v", err)
	}
	p := &ptyPair{
		master: os.NewFile(uintptr(mfd), "/dev/ptmx"),
		slave:  os.NewFile(uintptr(sfd), slavePath),
	}
	t.Cleanup(p.close)
	return p
}

// close is idempotent: subtests close early to unblock a hung read, and the
// cleanup runs again at test end.
func (p *ptyPair) close() {
	_ = p.master.Close()
	_ = p.slave.Close()
}

// termios reads the slave's current terminal settings. The struct is
// comparable, so equality is a byte-for-byte check of every flag and control
// character -- which is the actual claim: not "restored approximately".
func (p *ptyPair) termios(t *testing.T) unix.Termios {
	t.Helper()
	st, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS: %v", err)
	}
	return *st
}

// drainMaster consumes everything the editor writes to the terminal and makes
// it available to the test. Without a reader the PTY buffer fills and the
// editor blocks on its own prompt repaint, so this is required for progress,
// not just for assertions.
func drainMaster(p *ptyPair) *lockedBuffer {
	seen := &lockedBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.master.Read(buf)
			if n > 0 {
				_, _ = seen.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return seen
}

// ptyREPL runs one full REPL over the pair and returns a channel carrying the
// loop's error and any recovered panic value. onInterrupt is the policy hook;
// nil installs the real replControl behavior.
type ptyResult struct {
	err       error
	recovered any
}

func startPTYREPL(t *testing.T, p *ptyPair, onInterrupt func(func())) (<-chan ptyResult, *lockedBuffer) {
	t.Helper()
	root := t.TempDir()
	sess := newTestSession(t, &scriptCaller{}, root)

	seen := drainMaster(p)
	errOut := &lockedBuffer{}

	replCtx, cancelREPL := context.WithCancel(context.Background())
	t.Cleanup(cancelREPL)
	interrupts := make(chan struct{}, 1)
	ctrl := newReplControl(p.slave, errOut, interrupts, cancelREPL)
	sess.control = ctrl

	hook := ctrl.interrupt
	if onInterrupt != nil {
		hook = func() { onInterrupt(ctrl.interrupt) }
	}

	// Real descriptors, real termOps: MakeRaw and Restore act on the PTY.
	src := newInput(inputConfig{
		Stdin: p.slave, Stdout: p.slave, Stderr: errOut,
		Getenv:      func(string) string { return "" },
		Root:        root,
		OnInterrupt: hook,
	})
	if _, isEditor := src.(*editorSource); !isEditor {
		t.Fatalf("newInput selected %T over a real PTY, want the editor", src)
	}
	ctrl.setIdleDisplay(src.IdleDisplay)

	done := make(chan ptyResult, 1)
	go func() {
		var res ptyResult
		defer func() {
			res.recovered = recover()
			done <- res
		}()
		res.err = withLineSource(src, func(s lineSource) error {
			return runREPL(replCtx, s, p.slave, interrupts, sess)
		})
	}()
	return done, seen
}

// awaitPTY waits for the loop to finish. On timeout it closes both ends first,
// so a blocked read(2) cannot leave the goroutine parked and the failure is
// reported instead of hanging CI.
func awaitPTY(t *testing.T, p *ptyPair, done <-chan ptyResult) ptyResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		p.close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("the REPL did not exit within the timeout")
		return ptyResult{}
	}
}

// awaitPrompt blocks until the prompt is on screen, which happens only after
// MakeRaw has succeeded and ReadLine is inside the raw window.
//
// This barrier is required, not cosmetic. A byte written while the line
// discipline is still canonical is interpreted by the kernel rather than
// delivered: 0x04 becomes a VEOF marker that the switch to non-canonical mode
// discards, and 0x03 becomes a signal character for a foreground process group
// that does not exist. Either way the byte never reaches the editor and the
// read blocks forever.
func awaitPrompt(t *testing.T, p *ptyPair, seen *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(seen.String(), promptText) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	p.close()
	t.Fatalf("the prompt never reached the terminal; master saw %q", seen.String())
}

func writeMaster(t *testing.T, p *ptyPair, s string) {
	t.Helper()
	if _, err := p.master.WriteString(s); err != nil {
		t.Fatalf("write %q to the pty master: %v", s, err)
	}
}

func TestPTYRestoresTermiosOnEveryExit(t *testing.T) {
	// One case per way the REPL can end. Each asserts the same thing the user
	// actually cares about: the shell they return to is not left in raw mode.
	t.Run("slash exit", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "/exit\r")
		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("/exit = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after /exit:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("ctrl-d on an empty line", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "\x04")
		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("Ctrl-D = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after Ctrl-D:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("idle double ctrl-c", func(t *testing.T) {
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, nil)
		awaitPrompt(t, p, seen)

		// The presses must be two distinct reads. The interrupt cycle discards
		// whatever the filter retained past the first 0x03, so a single write
		// of both bytes would drop the second. Waiting for the hint is the
		// barrier that proves the first press was fully processed.
		writeMaster(t, p, "\x03")
		waitFor(t, func() bool { return strings.Contains(seen.String(), ctrlCHint) })
		writeMaster(t, p, "\x03")

		if res := awaitPTY(t, p, done); res.err != nil || res.recovered != nil {
			t.Fatalf("double Ctrl-C = err %v panic %v, want a clean exit", res.err, res.recovered)
		}
		if got := strings.Count(seen.String(), ctrlCHint); got != 1 {
			t.Fatalf("hint printed %d times over a real terminal, want exactly 1", got)
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after a double Ctrl-C:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("panic in the interrupt hook", func(t *testing.T) {
		// The raw-window defer must restore termios while the stack unwinds.
		// A panic that escaped with the terminal still raw would leave the
		// user's shell unusable, which no error path can repair afterwards.
		p := openPTY(t)
		before := p.termios(t)
		done, seen := startPTYREPL(t, p, func(func()) {
			panic("interrupt hook exploded")
		})
		awaitPrompt(t, p, seen)
		writeMaster(t, p, "\x03")

		res := awaitPTY(t, p, done)
		if res.recovered == nil {
			t.Fatal("the panic did not propagate out of the REPL; this case proves nothing without it")
		}
		if after := p.termios(t); after != before {
			t.Fatalf("termios not restored after a panic:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

func TestPTYRawModeIsActuallyEntered(t *testing.T) {
	// Guards the other three cases from passing vacuously. If the editor never
	// put the terminal into raw mode, every "restored" assertion above would
	// hold trivially, so this pins that termios genuinely changes mid-read:
	// ECHO and ICANON off, ISIG off (which is why 0x03 arrives as a byte).
	p := openPTY(t)
	before := p.termios(t)

	root := t.TempDir()
	seen := drainMaster(p)
	errOut := &lockedBuffer{}
	src := newInput(inputConfig{
		Stdin: p.slave, Stdout: p.slave, Stderr: errOut,
		Getenv: func(string) string { return "" },
		Root:   root,
	})
	read := make(chan struct{})
	// Close is owned by the mode boundary and must not overlap a live read; on
	// a t.Fatal path below the reader is still parked inside ReadLine, so this
	// cleanup unblocks it and joins before closing. Registering a bare
	// src.Close() here is a genuine -race flake, not a theoretical one.
	t.Cleanup(func() {
		p.close() // idempotent, and unblocks a parked ReadLine
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			t.Error("the read goroutine never returned; closing the source would race it")
			return
		}
		_ = src.Close()
	})

	go func() {
		defer close(read)
		_, _, _ = src.ReadGoal(context.Background(), promptText)
	}()

	// The prompt on screen means the raw window is open and ReadLine is
	// blocked inside it.
	awaitPrompt(t, p, seen)
	during := p.termios(t)
	if during.Lflag&unix.ECHO != 0 || during.Lflag&unix.ICANON != 0 || during.Lflag&unix.ISIG != 0 {
		t.Fatalf("terminal is not in raw mode during a read: Lflag = %#x", during.Lflag)
	}
	if during == before {
		t.Fatal("termios unchanged during a read; the restore assertions would pass vacuously")
	}

	writeMaster(t, p, "\r")
	select {
	case <-read:
	case <-time.After(10 * time.Second):
		p.close()
		t.Fatal("the read did not return")
	}
	if after := p.termios(t); after != before {
		t.Fatalf("termios not restored after one read:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Compile-time assertion: the PTY helpers use the same io.Writer the editor
// writes through, so a future signature change breaks here rather than at
// runtime in CI only.
var _ io.Writer = (*lockedBuffer)(nil)
