package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

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
	c.prompt()
	out.Reset() // drop the initial prompt; focus on the notice's effect
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
	c.prompt()
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
func TestReplControlInterruptMidTurnCancels(t *testing.T) {
	c, _, _, interrupts, quit := newTestControl()
	c.prompt()
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
	c.prompt()
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
	c.prompt()
	c.interrupt() // arm
	c.prompt()    // intervening activity disarms
	c.interrupt() // should hint again, not quit
	if *quit {
		t.Error("Ctrl-C after a fresh prompt must re-arm (hint), not quit")
	}
}

// runREPL must drive the prompt through sess.control when set (the interactive
// wiring), and still exit cleanly on /exit.
func TestReplControlWiredIntoRunREPL(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	var out, errOut strings.Builder
	interrupts := make(chan struct{}, 1)
	sess := newTestSession(t, caller, root)
	sess.control = newReplControl(&out, &errOut, interrupts, func() {})

	in := strings.NewReader("/exit\n")
	if err := runREPL(context.Background(), in, &out, interrupts, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(out.String(), promptText) {
		t.Errorf("control-driven prompt not printed:\n%s", out.String())
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
	defer pw.Close() // unblock the lineReader goroutine at test end

	done := make(chan error, 1)
	go func() { done <- runREPL(ctx, pr, &out, interrupts, sess) }()

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
