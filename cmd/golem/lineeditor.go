package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/term"
)

// discardHistory is the History bound for an approval read. Approval answers
// are structurally incapable of reaching goal history: a different method, a
// binding that stores nothing, and RecordGoal never called on that path. An
// up arrow at an approval prompt therefore has nothing to offer.
type discardHistory struct{}

func (discardHistory) Add(string) {}
func (discardHistory) Len() int   { return 0 }

// At panics as term.History requires for an out-of-range index. Len is always
// zero, so x/term's bounds check (terminal.go historyAt) never calls it.
func (discardHistory) At(int) string { panic("golem: discard history has no entries") }

// Compile-time assertion: the discard binding must satisfy x/term's contract.
var _ term.History = discardHistory{}

// errSegmentFlagMissing reports that the Terminal returned a line the key
// filter never framed. It is an internal protocol failure, not a user-input
// problem: the provenance of this line -- and so of every later one -- is
// unknown, and an approval read may depend on exactly that bit. Fail closed
// rather than reading on.
var errSegmentFlagMissing = errors.New("golem: terminal returned a line with no segment flag; terminator provenance is unknown")

// termIO is the single io.ReadWriter term.NewTerminal takes. Input comes from
// the key filter and output goes to stdout, so they have to be joined into one
// object; nothing else needs the pair.
type termIO struct {
	r io.Reader
	w io.Writer
}

func (t termIO) Read(p []byte) (int, error)  { return t.r.Read(p) }
func (t termIO) Write(p []byte) (int, error) { return t.w.Write(p) }

// terminalBinding owns the answer to "where does terminal output go right
// now": the current Terminal, or the scanner the editor permanently fell back
// to after MakeRaw failed.
//
// Its mutex is held through pointer lookup and every nonblocking operation, so
// a notice, a resize, or a paste escape can never target a Terminal that has
// already been discarded. The sole reader is the one exception: it snapshots
// under the mutex, releases it, and then calls the blocking ReadLine. Only that
// same goroutine may replace the snapshot, and only after ReadLine returns.
type terminalBinding struct {
	mu     sync.Mutex
	tm     *term.Terminal
	fallen lineSource

	// out is where an idle message goes before the first read has built a
	// Terminal. The auto-index goroutine can emit a notice in exactly that
	// window -- the display is bound before the job starts -- and without this
	// the message would be dropped on the floor. The terminal is still cooked
	// there, so a plain line is the correct rendering.
	out io.Writer
}

// replace installs a Terminal. Called by the sole reader between reads.
func (b *terminalBinding) replace(tm *term.Terminal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fallen != nil {
		return
	}
	b.tm = tm
}

// snapshot hands the reader the current Terminal so it can block on ReadLine
// without holding the mutex, which would deadlock every asynchronous notice for
// as long as the user is thinking.
func (b *terminalBinding) snapshot() *term.Terminal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tm
}

func (b *terminalBinding) fallenSource() lineSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fallen
}

// fallBack makes the swap to the scanner atomic with respect to reads, notices,
// resize, and paste escapes: after it returns, none of them can reach a
// Terminal whose descriptor was never put into raw mode.
func (b *terminalBinding) fallBack(src lineSource) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fallen = src
	b.tm = nil
}

func (b *terminalBinding) setPaste(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tm != nil {
		b.tm.SetBracketedPasteMode(on)
	}
}

func (b *terminalBinding) setSize(width, height int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tm != nil {
		_ = b.tm.SetSize(width, height)
	}
}

// idleDisplay writes an asynchronous message. Terminal.Write repaints the
// prompt and any partially typed input itself, so unlike the scanner the editor
// adds nothing of its own.
func (b *terminalBinding) idleDisplay(msg string) {
	b.mu.Lock()
	if s := b.fallen; s != nil {
		// Released before delegating: the scanner takes its own display lock,
		// and holding two display locks at once is how the notice path and the
		// interrupt path form a cycle.
		b.mu.Unlock()
		s.IdleDisplay(msg)
		return
	}
	if b.tm != nil {
		_, _ = b.tm.Write([]byte(msg + "\n"))
	} else if b.out != nil {
		_, _ = fmt.Fprintf(b.out, "%s\n", msg)
	}
	b.mu.Unlock()
}

// editorSource is the TTY line source: an x/term.Terminal over the key filter,
// with raw mode scoped to one logical read.
//
// Reads are synchronous and there is no owner goroutine. Cancelling a blocked
// read(2) on a tty needs a per-platform poll loop and a self-pipe wakeup;
// wrapping the read in a goroutine does not provide it, because it abandons the
// caller while the read stays live and the terminal stays raw.
type editorSource struct {
	stdinFD  int
	stdoutFD int
	stderr   io.Writer
	ops      termOps

	// rw and filter are built once. A per-read key filter would drop the bytes
	// it retains across chunk boundaries and the typeahead already buffered.
	rw     termIO
	filter *keyFilter

	hist   *goalHistory // nil when history is disabled
	recall term.History // hist, or discardHistory when it is nil

	binding terminalBinding

	resizeDone chan struct{}
	resizeWG   sync.WaitGroup
	stopResize func()
	stopOnce   sync.Once

	fallback     func() lineSource
	fallbackOnce sync.Once

	// rawState is the termios saved by the current or last raw window. It is
	// cleared only once Restore succeeds, so a failed restore leaves exactly one
	// retry for Close. Owned by the sole reader and by Close, which the mode
	// boundary keeps from overlapping.
	rawState *term.State

	closeOnce sync.Once
	closeErr  error
}

// Compile-time assertion: editorSource must satisfy lineSource.
var _ lineSource = (*editorSource)(nil)

// newEditorSource builds the editor. resize may be nil (platforms without a
// watcher); fallback is invoked at most once, and only if MakeRaw fails.
func newEditorSource(cfg inputConfig, resize <-chan struct{}, stopResize func(),
	fallback func() lineSource) *editorSource {

	cfg = cfg.withDefaults()
	if stopResize == nil {
		stopResize = func() {}
	}
	e := &editorSource{
		stdinFD:    int(cfg.Stdin.Fd()),
		stdoutFD:   int(cfg.Stdout.Fd()),
		stderr:     cfg.Stderr,
		ops:        cfg.Ops,
		filter:     newKeyFilter(cfg.In, maxGoalBytes),
		recall:     discardHistory{},
		stopResize: stopResize,
		resizeDone: make(chan struct{}),
		fallback:   fallback,
	}
	e.rw = termIO{r: e.filter, w: cfg.Out}
	e.binding.out = cfg.Out
	if cfg.UseHistory {
		e.hist = newGoalHistory(cfg.Getenv, cfg.Root, func(msg string) {
			_, _ = fmt.Fprintln(cfg.Stderr, msg)
		})
		e.recall = e.hist
	}
	if resize != nil {
		e.resizeWG.Add(1)
		go e.watchSize(resize)
	}
	return e
}

// watchSize applies terminal resizes to whichever Terminal is bound. The size
// always comes from stdout: x/term's Windows GetSize calls
// GetConsoleScreenBufferInfo, an output-handle API, while Unix TIOCGWINSZ
// answers on either descriptor, so a mix-up is invisible on Unix and wrong on
// Windows.
func (e *editorSource) watchSize(resize <-chan struct{}) {
	defer e.resizeWG.Done()
	for {
		select {
		case <-e.resizeDone:
			return
		case <-resize:
			width, height, err := e.ops.GetSize(e.stdoutFD)
			if err != nil {
				// A transient size query failure is not worth interrupting the
				// prompt for; the terminal keeps its previous dimensions.
				continue
			}
			e.binding.setSize(width, height)
		}
	}
}

// stopResizeDelivery stops the signal watcher and joins this source's consumer.
// Idempotent: both the permanent MakeRaw fallback and Close call it.
func (e *editorSource) stopResizeDelivery() {
	e.stopOnce.Do(func() {
		e.stopResize()
		close(e.resizeDone)
		e.resizeWG.Wait()
	})
}

func (e *editorSource) ReadGoal(ctx context.Context, prompt string) (string, bool, error) {
	return e.read(ctx, prompt, e.recall, func(s lineSource) (string, bool, error) {
		return s.ReadGoal(ctx, prompt)
	})
}

func (e *editorSource) ReadAnswer(ctx context.Context, prompt string) (string, bool, error) {
	// The discard binding covers the whole answer read, so no arrow key can
	// surface a goal at an approval prompt.
	return e.read(ctx, prompt, discardHistory{}, func(s lineSource) (string, bool, error) {
		return s.ReadAnswer(ctx, prompt)
	})
}

// read is one logical call: one raw window, one bracketed-paste window, and one
// reverse-order teardown, regardless of how many segments the Terminal returns
// inside it. Opening and closing per segment would flap termios in the middle
// of a paste.
//
// ctx cannot interrupt a read already blocked in read(2) on a tty: doing that
// needs a per-platform poll loop and a self-pipe wakeup, and wrapping the read
// in a goroutine does not provide it -- it abandons the caller while the read
// stays live and the terminal stays raw. Section 4 of the spec establishes that
// no cancellation source fires in that window, and the supported cancellation
// path is the in-band Ctrl-C byte routed to the policy owner.
//
// An already-cancelled ctx is a different question, and honoring it is free.
// Opening a raw window on a dead context would prompt a user who has already
// been told the session is over. delegate carries the caller's ctx for the
// scanner fallback, which can honor it throughout.
func (e *editorSource) read(ctx context.Context, prompt string, hist term.History,
	delegate func(lineSource) (string, bool, error)) (line string, ok bool, err error) {

	if s := e.binding.fallenSource(); s != nil {
		return delegate(s)
	}
	if cerr := ctx.Err(); cerr != nil {
		return "", false, cerr
	}

	state, rawErr := e.ops.MakeRaw(e.stdinFD)
	if rawErr != nil {
		return delegate(e.fallBack(rawErr))
	}
	// Keep the oldest un-restored state, never the newest. If a previous
	// window's Restore failed, termios never returned to its original settings,
	// so what MakeRaw just handed back as "previous" is the raw state itself.
	// Restoring that at the end of this window would pin the terminal in raw
	// mode and clear the pending state, leaving Close nothing to retry.
	if e.rawState == nil {
		e.rawState = state
	}
	restoreState := e.rawState
	defer func() {
		// One defer, exact reverse order. term.Restore resets termios but does
		// not emit ESC[?2004l, so disabling paste after it would leak markers
		// into the cooked-mode input queue and into the user's shell after exit.
		e.binding.setPaste(false)
		if rerr := e.ops.Restore(e.stdinFD, restoreState); rerr != nil {
			// Refuse to let a turn render on a terminal still in raw mode, where
			// every unsynchronized renderer newline staircases. rawState is kept
			// so Close can retry the restore exactly once.
			err = errors.Join(err, rerr)
			line, ok = "", false
			return
		}
		e.rawState = nil
	}()

	// A fresh Terminal per logical call. x/term keeps cursor and line state that
	// is unreachable from outside the package, and a reused instance would
	// resume with a stale cursor model.
	tm := term.NewTerminal(e.rw, prompt)
	tm.History = hist
	e.binding.replace(tm)
	if width, height, sizeErr := e.ops.GetSize(e.stdoutFD); sizeErr == nil {
		e.binding.setSize(width, height)
	}
	e.binding.setPaste(true)

	// The sole-reader exception: snapshot under the binding mutex, release it,
	// then block.
	current := e.binding.snapshot()
	line, readErr := current.ReadLine()
	switch {
	case readErr == nil, errors.Is(readErr, term.ErrPasteIndicator):
		// ErrPasteIndicator only reports that the line happened to arrive
		// entirely inside a paste. It says nothing about whether more of that
		// paste is pending, so it is advisory here and the line is real.
		//
		// Claiming the flag is mandatory, not bookkeeping: the filter delivers
		// at most one line terminator at a time and answers the next Read with
		// errTerminatorSwallowed while a flag is unclaimed, so a reader that
		// skipped this would fail on its second call. Composition consumes the
		// paste bit in a later slice; here only its presence is checked.
		if _, flagOK := e.filter.PopSegmentFlag(); !flagOK {
			// Unlike an input rejection, buffered bytes here are of unknown
			// provenance and must not be reused.
			e.filter.Discard()
			return "", false, errSegmentFlagMissing
		}
		return line, true, nil
	case errors.Is(readErr, io.EOF):
		// x/term collapses a closed stdin and Ctrl-D on an empty line to io.EOF;
		// both are a clean exit.
		return "", false, nil
	default:
		return "", false, readErr
	}
}

// fallBack permanently swaps this source for the scanner after MakeRaw failed,
// warns once, and stops resize delivery. Everything it turns off is a no-op for
// the later Close.
//
// Known limitation: if MakeRaw fails on a later window rather than the first,
// any bytes the key filter had already buffered are dropped, because there is
// no way to hand a partially consumed stream to the scanner. That costs at most
// the typeahead sitting in the filter at the moment the descriptor stopped
// being a terminal, which is a strictly better outcome than reading on with
// termios in an unknown state.
func (e *editorSource) fallBack(cause error) lineSource {
	e.fallbackOnce.Do(func() {
		src := e.fallback()
		_, _ = fmt.Fprintf(e.stderr, "golem: line editing unavailable (%v); using basic input\n", cause)
		e.stopResizeDelivery()
		e.binding.fallBack(src)
	})
	return e.binding.fallenSource()
}

// RecordGoal appends an accepted goal. Called by runREPL between reads, on the
// same goroutine as the reader, which is what makes the unsynchronized history
// safe.
func (e *editorSource) RecordGoal(goal string) {
	if s := e.binding.fallenSource(); s != nil {
		s.RecordGoal(goal)
		return
	}
	if e.hist != nil {
		e.hist.Record(goal)
	}
}

func (e *editorSource) IdleDisplay(msg string) { e.binding.idleDisplay(msg) }

// Close is state-aware and idempotent. A completed read has already disabled
// paste and restored termios, so Close must not issue an unmatched second pair;
// it only finishes what is still outstanding.
func (e *editorSource) Close() error {
	e.closeOnce.Do(func() {
		// A read whose Restore failed left its termios saved. Retry exactly once,
		// before anything else, so the user's shell is not left raw.
		if st := e.rawState; st != nil {
			e.rawState = nil
			if err := e.ops.Restore(e.stdinFD, st); err != nil {
				e.closeErr = errors.Join(e.closeErr, err)
			}
		}
		e.stopResizeDelivery()
		// This source built the fallback, so it owns closing it. Today the
		// scanner's Close is a no-op; leaving it out would make that an
		// invariant of a type this file does not own.
		if s := e.binding.fallenSource(); s != nil {
			if err := s.Close(); err != nil {
				e.closeErr = errors.Join(e.closeErr, err)
			}
		}
		if e.hist != nil {
			if err := e.hist.Close(); err != nil {
				e.closeErr = errors.Join(e.closeErr, err)
			}
		}
	})
	return e.closeErr
}
