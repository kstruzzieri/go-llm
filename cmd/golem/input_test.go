package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestLineSourceModeFor(t *testing.T) {
	// One row per dispatch outcome in main.run. A mode that reads nothing must
	// never construct a source, because that would open a reader and, later, a
	// history file for a non-interactive run.
	for _, tc := range []struct {
		name string
		f    flags
		want lineSourceMode
	}{
		{"default REPL", flags{}, sourceREPL},
		{"interactive goal", flags{goalSet: true}, sourceAnswerOnly},
		{"auto-approved goal", flags{goalSet: true, approvePlanLock: true}, sourceNone},
		{"agentflow task", flags{planPath: "plan.json"}, sourceNone},
		{"one shot", flags{promptSet: true}, sourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineSourceModeFor(tc.f); got != tc.want {
				t.Fatalf("mode = %v, want %v", got, tc.want)
			}
		})
	}
}

// countingSource records lifecycle calls so ownership can be asserted.
type countingSource struct {
	closes  int
	closeIn error
	goals   []string
}

func (c *countingSource) ReadGoal(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (c *countingSource) ReadAnswer(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (c *countingSource) RecordGoal(g string) { c.goals = append(c.goals, g) }
func (c *countingSource) IdleDisplay(string)  {}
func (c *countingSource) Close() error        { c.closes++; return c.closeIn }

func TestWithLineSourceClosesOnce(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		src := &countingSource{}
		if err := withLineSource(src, func(lineSource) error { return nil }); err != nil {
			t.Fatalf("err = %v", err)
		}
		if src.closes != 1 {
			t.Fatalf("closes = %d, want 1", src.closes)
		}
	})

	t.Run("error is preserved", func(t *testing.T) {
		src := &countingSource{}
		want := errors.New("from fn")
		err := withLineSource(src, func(lineSource) error { return want })
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if src.closes != 1 {
			t.Fatalf("closes = %d, want 1", src.closes)
		}
	})

	t.Run("close error is joined not swallowed", func(t *testing.T) {
		closeErr := errors.New("close failed")
		fnErr := errors.New("from fn")
		src := &countingSource{closeIn: closeErr}
		err := withLineSource(src, func(lineSource) error { return fnErr })
		if !errors.Is(err, fnErr) || !errors.Is(err, closeErr) {
			t.Fatalf("err = %v, want both %v and %v", err, fnErr, closeErr)
		}
	})

	t.Run("panic still closes and propagates", func(t *testing.T) {
		src := &countingSource{}
		func() {
			defer func() {
				// The value is asserted, not just its presence: a
				// recover-and-repanic would satisfy a nil check while
				// discarding the original stack the spec requires.
				switch r := recover(); r {
				case nil:
					t.Fatal("panic did not propagate past withLineSource")
				case "boom":
				default:
					t.Fatalf("panic value = %v, want the original %q", r, "boom")
				}
			}()
			_ = withLineSource(src, func(lineSource) error { panic("boom") })
		}()
		if src.closes != 1 {
			t.Fatalf("closes = %d, want 1 even during a panic", src.closes)
		}
	})
}

func TestScannerSourcePrintsItsOwnPrompt(t *testing.T) {
	var out bytes.Buffer
	src := newScannerSource(strings.NewReader("hello\n"), &out)
	line, ok, err := src.ReadGoal(context.Background(), promptText)
	if !ok || err != nil || line != "hello" {
		t.Fatalf("read = %q ok=%v err=%v", line, ok, err)
	}
	if got := out.String(); got != promptText {
		t.Fatalf("out = %q, want exactly one %q", got, promptText)
	}
}

func TestScannerSourceIdleDisplay(t *testing.T) {
	t.Run("no prompt on screen writes the message alone", func(t *testing.T) {
		var out bytes.Buffer
		src := newScannerSource(strings.NewReader(""), &out)
		src.IdleDisplay("heads up")
		if got, want := out.String(), "heads up\n"; got != want {
			t.Fatalf("out = %q, want %q; nothing was on screen to restore", got, want)
		}
	})

	t.Run("prompt on screen is restored", func(t *testing.T) {
		// A read is in flight over a pipe that has produced nothing yet, so the
		// prompt is visible and must be reprinted under the message.
		var out lockedBuffer
		pr, pw := newBlockedPipe(t)
		src := newScannerSource(pr, &out)
		started := make(chan struct{})
		done := make(chan struct{})
		go func() {
			close(started)
			_, _, _ = src.ReadGoal(context.Background(), promptText)
			close(done)
		}()
		<-started
		waitFor(t, func() bool { return strings.Contains(out.String(), promptText) })
		out.Reset()

		src.IdleDisplay("heads up")
		if got, want := out.String(), "\nheads up\n"+promptText; got != want {
			t.Fatalf("out = %q, want %q", got, want)
		}
		_, _ = pw.Write([]byte("x\n"))
		<-done
	})
}

func TestScannerSourceNoticeBetweenEnterPromptAndReadPrintsOnePrompt(t *testing.T) {
	// The window between enterPrompt marking the REPL idle and the read
	// printing the prompt is real. A notice landing in it must not print a
	// prompt of its own, or the user sees two.
	// Driven synchronously with input already available, so the count is taken
	// after the read has certainly printed. Waiting for the prompt to appear
	// instead would be satisfied by a spurious prompt and never see the real
	// one, which is exactly the double-prompt this guards against.
	var out lockedBuffer
	ctrl := newReplControl(&out, &bytes.Buffer{}, nil, func() {})
	src := newScannerSource(strings.NewReader("x\n"), &out)
	ctrl.setIdleDisplay(src.IdleDisplay)

	ctrl.enterPrompt()
	ctrl.notice("async note")
	if _, _, err := src.ReadGoal(context.Background(), promptText); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}

	if got := strings.Count(out.String(), promptText); got != 1 {
		t.Fatalf("prompt appeared %d times in %q, want exactly 1", got, out.String())
	}
	if !strings.Contains(out.String(), "async note") {
		t.Fatalf("notice missing from %q", out.String())
	}
}

// TestRunREPLOutputIsByteIdenticalToPreSeam is the characterization golden the
// plan requires for this refactor: routing every read through lineSource must
// not move a single byte of REPL output. The expected value was captured by
// running this exact script against 3ab671b, the commit before the seam, so it
// encodes pre-change behavior rather than the post-change code's own opinion.
//
// The consecutive "golem> golem> golem> " run is the load-bearing part: it is
// one prompt per blank/whitespace line with nothing between them, which is what
// a double-printing or a missing prompt would disturb.
//
// /bogus rather than /help on purpose -- the help body would make this golden
// churn on unrelated command changes without testing anything the seam owns.
//
// It still pins the renderer footer and the ctx percentage, so an unrelated
// renderer or system-prompt change will fail it. That is intended: confirm the
// prompt structure above is unchanged, then update the constant. Do not
// regenerate it from the code under test without looking.
func TestRunREPLOutputIsByteIdenticalToPreSeam(t *testing.T) {
	const want = "golem> ok one\n" +
		"? · 0.0s · ctx 7% · step 1/16\n" +
		"done · 1 step · 0.0s · 0 tok\n" +
		"golem> golem> golem> unknown command: /bogus (try /help)\n" +
		"golem> ok two\n" +
		"? · 0.0s · ctx 7% · step 1/16\n" +
		"done · 1 step · 0.0s · 0 tok\n" +
		"golem> \n"

	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{Content: "ok one"}},
		{Response: provider.ChatResponse{Content: "ok two"}},
	}}
	sess := newTestSession(t, caller, t.TempDir())
	var out, errOut strings.Builder
	sess.control = newReplControl(&out, &errOut, make(chan struct{}, 1), func() {})

	in := strings.NewReader("first goal\n\n   \n/bogus\nsecond goal\n")
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if got := out.String(); got != want {
		t.Fatalf("REPL output changed.\ngot:  %q\nwant: %q", got, want)
	}
}

func TestSetIdleDisplayNilRestoresTheDefault(t *testing.T) {
	// runREPL returns with atPrompt still true and signal.Stop runs after the
	// source closes, so the REPL branch unbinds on the way out. Without a
	// restore, a Ctrl-C in that window would render through a closed source.
	var out, errOut bytes.Buffer
	c := newReplControl(&out, &errOut, make(chan struct{}, 1), func() {})

	sourceCalls := 0
	c.setIdleDisplay(func(string) { sourceCalls++ })
	c.enterPrompt()
	c.notice("through the source")
	if sourceCalls != 1 {
		t.Fatalf("source display called %d times, want 1", sourceCalls)
	}
	if out.Len() != 0 {
		t.Fatalf("source display must own the write, got %q", out.String())
	}

	c.setIdleDisplay(nil) // source closing
	c.notice("after unbind")
	if sourceCalls != 1 {
		t.Fatalf("closed source still received a notice: %d calls", sourceCalls)
	}
	if got, want := out.String(), "\nafter unbind\n"+promptText; got != want {
		t.Fatalf("out = %q, want the source-free default %q", got, want)
	}
}

func TestReplControlWithoutSourceDoesNotPanic(t *testing.T) {
	// -plan and Agentflow task mode build a control but no source, so
	// setIdleDisplay is never called. A nil field would be a dereference one
	// caller away.
	var out, errOut bytes.Buffer
	interrupts := make(chan struct{}, 1)
	quit := 0
	c := newReplControl(&out, &errOut, interrupts, func() { quit++ })

	c.notice("mid-turn") // not at prompt: stderr
	c.interrupt()        // not at prompt: cancels the turn
	c.enterPrompt()
	c.notice("idle") // at prompt: default display
	c.interrupt()    // arms and hints
	c.interrupt()    // quits
	if quit != 1 {
		t.Fatalf("quit called %d times, want 1", quit)
	}
	if !strings.Contains(out.String(), ctrlCHint) {
		t.Fatalf("default idle display did not render the hint: %q", out.String())
	}
}

// lockedBuffer is a bytes.Buffer safe for the concurrent writes these tests
// make from a reading goroutine and the test goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// newBlockedPipe returns a pipe whose reader blocks until the test writes,
// modelling a prompt that is on screen with no input yet.
func newBlockedPipe(t *testing.T) (*io.PipeReader, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	return pr, pw
}

// waitFor polls cond until it holds or the test times out, so these tests
// synchronize on observable output rather than on a sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within the timeout")
}

// recordingSource wraps the scanner so a test can see exactly which lines
// runREPL chose to record.
type recordingSource struct {
	*scannerSource
	recorded []string
}

func (r *recordingSource) RecordGoal(g string) { r.recorded = append(r.recorded, g) }

func TestRunREPLRecordsOnlyAcceptedGoals(t *testing.T) {
	// Recording happens after trimming and after the empty and slash checks,
	// so a blank line or a command can never reach history. Spec 13b requires
	// the same of a rejected goal in a later task; this pins the ordering the
	// guarantee rests on.
	var out strings.Builder
	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{Content: "ok one"}},
		{Response: provider.ChatResponse{Content: "ok two"}},
	}}
	sess := newTestSession(t, caller, t.TempDir())
	in := strings.NewReader("first goal\n\n   \n/tools\n/help\nsecond goal\n")
	src := &recordingSource{scannerSource: newScannerSource(in, &out)}

	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	want := []string{"first goal", "second goal"}
	if !equalStrings(src.recorded, want) {
		t.Fatalf("recorded = %q, want %q", src.recorded, want)
	}
}
