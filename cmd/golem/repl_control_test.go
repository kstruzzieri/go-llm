package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// newTestControl builds a control with no line source bound, so every test
// below exercises the source-free default display.
//
// Read that literally: since the lineSource seam landed, production notices in
// the REPL render through scannerSource.IdleDisplay, not through this default.
// These cases still guard the fallback and the atPrompt/armed policy, but they
// no longer cover the REPL's real rendering path -- TestScannerSourceIdleDisplay
// and TestScannerSourceNoticeBetweenEnterPromptAndReadPrintsOnePrompt do.
func newTestControl() (c *replControl, out, errOut *bytes.Buffer, interrupts chan struct{}, quit *bool) {
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	interrupts = make(chan struct{}, 1)
	q := false
	quit = &q
	c = newReplControl(out, errOut, interrupts, func() { q = true })
	return c, out, errOut, interrupts, quit
}

// notice while idle at the prompt must reprint the prompt so it is never buried
// (the bug that made the REPL look hung).
func TestReplControlNoticeReprintsPromptWhenIdle(t *testing.T) {
	c, out, _, _, _ := newTestControl()
	c.enterPrompt()
	out.Reset() // enterPrompt prints nothing; focus on the notice's effect
	c.notice("warning: something happened")
	got := out.String()
	if !strings.Contains(got, "warning: something happened") {
		t.Fatalf("notice text missing: %q", got)
	}
	if !strings.HasSuffix(got, promptText) {
		t.Errorf("prompt not reprinted after notice: %q", got)
	}
}

// notice during a turn must NOT reprint the prompt (the renderer owns the
// screen mid-turn).
func TestReplControlNoticeNoPromptDuringTurn(t *testing.T) {
	c, out, errOut, _, _ := newTestControl()
	c.enterPrompt()
	c.enterTurn()
	out.Reset()
	c.notice("mid-turn note")
	if out.Len() != 0 {
		t.Errorf("mid-turn notice must not touch stdout (renderer's stream): %q", out.String())
	}
	if !strings.Contains(errOut.String(), "mid-turn note") {
		t.Errorf("mid-turn notice should go to stderr: %q", errOut.String())
	}
}

// Ctrl-C during a turn cancels the turn (sends on interrupts) and does not quit.
func TestReplControlQueuesNoticesWhileSuspended(t *testing.T) {
	// /edit hands the screen to an external process. A notice painted over a
	// full-screen editor corrupts a display golem cannot repaint, because the
	// editor owns it. They are still warnings the user needs, so they are held
	// and flushed rather than dropped.
	var out, errOut bytes.Buffer
	c := newReplControl(&out, &errOut, make(chan struct{}, 1), func() {})
	c.enterPrompt()

	c.suspendNotices()
	c.notice("first while editing")
	c.notice("second while editing")
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("a notice reached the terminal during an edit: out=%q err=%q", out.String(), errOut.String())
	}

	c.resumeNotices()
	got := out.String()
	first, second := strings.Index(got, "first while editing"), strings.Index(got, "second while editing")
	if first < 0 || second < 0 {
		t.Fatalf("queued notices were dropped instead of flushed: %q", got)
	}
	if first > second {
		t.Fatalf("queued notices flushed out of order: %q", got)
	}

	out.Reset()
	c.notice("after the edit")
	if !strings.Contains(out.String(), "after the edit") {
		t.Fatalf("notices did not resume: %q", out.String())
	}
}

func TestReplControlInterruptMidTurnCancels(t *testing.T) {
	c, _, _, interrupts, quit := newTestControl()
	c.enterPrompt()
	c.enterTurn()
	c.interrupt()
	select {
	case <-interrupts:
	default:
		t.Error("mid-turn Ctrl-C did not request turn cancellation")
	}
	if *quit {
		t.Error("mid-turn Ctrl-C must not quit")
	}
}

// First idle Ctrl-C arms + hints; the second quits.
func TestReplControlDoubleInterruptIdleQuits(t *testing.T) {
	c, out, _, interrupts, quit := newTestControl()
	c.enterPrompt()
	out.Reset()

	c.interrupt() // first: arm + hint
	if *quit {
		t.Fatal("first idle Ctrl-C must not quit")
	}
	if !strings.Contains(out.String(), ctrlCHint) {
		t.Errorf("first idle Ctrl-C should print the hint: %q", out.String())
	}
	select {
	case <-interrupts:
		t.Error("idle Ctrl-C must not request turn cancellation")
	default:
	}

	c.interrupt() // second: quit
	if !*quit {
		t.Error("second idle Ctrl-C should quit")
	}
}

// Intervening input (a fresh prompt) disarms, so a later single Ctrl-C hints
// again instead of quitting.
func TestReplControlPromptDisarms(t *testing.T) {
	c, _, _, _, quit := newTestControl()
	c.enterPrompt()
	c.interrupt()   // arm
	c.enterPrompt() // intervening activity disarms
	c.interrupt()   // should hint again, not quit
	if *quit {
		t.Error("Ctrl-C after a fresh prompt must re-arm (hint), not quit")
	}
}

// runREPL must drive the prompt through sess.control when set (the interactive
// wiring), and still exit cleanly on /exit.
func TestReplControlWiredIntoRunREPL(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	// Locked, not a strings.Builder: runREPL writes from its own goroutine
	// while the test reads.
	out, errOut := &lockedBuffer{}, &lockedBuffer{}
	interrupts := make(chan struct{}, 1)
	sess := newTestSession(t, caller, root)
	sess.control = newReplControl(out, errOut, interrupts, func() {})

	// The prompt itself proves nothing here: the SOURCE prints it, so a version
	// of runREPL with enterPrompt deleted would still show one. What the
	// control owns is whether the REPL is idle, and the observable consequence
	// is which stream an asynchronous notice takes -- stdout with the prompt
	// restored when idle, stderr when a turn is running.
	pr, pw := newBlockedPipe(t)
	src := newScannerSource(pr, out)
	sess.control.setIdleDisplay(src.IdleDisplay)

	done := make(chan error, 1)
	go func() {
		done <- runREPL(context.Background(), src, out, interrupts, sess)
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), promptText) })

	sess.control.notice("async while idle")
	waitFor(t, func() bool { return strings.Contains(out.String(), "async while idle") })
	if strings.Contains(errOut.String(), "async while idle") {
		t.Errorf("notice took the mid-turn stderr path; runREPL never marked the prompt idle:\n%s", errOut.String())
	}

	if _, err := pw.Write([]byte("/exit\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("runREPL: %v", err)
	}
}

// End-to-end quit path: when the control's quit cancels the REPL context (as a
// second idle Ctrl-C does), runREPL — parked in a blocking ReadLine — returns
// nil. The reader (an unwritten pipe) never yields a line or EOF, so the only
// way out is the context cancel.
func TestRunREPLExitsWhenControlQuits(t *testing.T) {
	root := t.TempDir()
	sess := newTestSession(t, &scriptCaller{}, root)
	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut strings.Builder
	interrupts := make(chan struct{}, 1)
	sess.control = newReplControl(&out, &errOut, interrupts, cancel)

	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }() // unblock the lineReader goroutine at test end

	done := make(chan error, 1)
	go func() { done <- runREPL(ctx, newScannerSource(pr, &out), &out, interrupts, sess) }()

	cancel() // stand in for quit(): a second idle Ctrl-C
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runREPL after quit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runREPL did not return after the control quit (context cancel)")
	}
}
