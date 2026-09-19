package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"

	"golang.org/x/term"
)

// lineSource is the one seam every interactive read goes through. Before it,
// the REPL loop and the approval prompt each held their own reader and each
// printed their own prompt; the editor arriving in task 5 can do neither,
// because x/term.Terminal prints and repaints its own prompt and must own the
// descriptor for the whole read.
//
// Exactly one implementation exists in this commit (scannerSource, today's
// behavior). The interface is introduced separately from the editor so the
// refactor is provable on its own.
type lineSource interface {
	// ReadGoal reads one top-level goal. The source prints prompt; callers
	// must not.
	ReadGoal(ctx context.Context, prompt string) (string, bool, error)

	// ReadAnswer reads one approval answer. Never recorded to history.
	ReadAnswer(ctx context.Context, prompt string) (string, bool, error)

	// RecordGoal appends an accepted goal to history. runREPL calls it after
	// the turn, excluding secrets-blocked goals as well as empty, invalid, or
	// slash-command lines. Unrelated run failures still enter history.
	RecordGoal(goal string)

	// IdleDisplay renders an asynchronous message while the user is at the
	// prompt, preserving whatever is already on screen.
	//
	// replControl calls this while holding its own mutex, so the atPrompt
	// decision and the display action stay one policy operation. An
	// implementation must therefore not call back into replControl, and must
	// not hold a lock that any path taken by replControl could already own --
	// the editor arriving in task 5 has to release its display and
	// Terminal-binding mutexes before it invokes the interrupt entry point, or
	// the two lock orders form a cycle.
	IdleDisplay(msg string)

	// Close releases whatever the source owns. Safe to call more than once.
	Close() error
}

// lineSourceMode is the final-dispatch decision about whether an interactive
// read can happen at all, and if so what kind. Keeping it a pure function of
// flags makes the table testable without constructing a session.
type lineSourceMode int

const (
	// sourceNone: nothing reads stdin interactively. One-shot (-p), Agentflow
	// task mode (-plan), and auto-approved planning (-goal -approve-plan-lock)
	// all fall here, so none of them opens a reader or a history file.
	sourceNone lineSourceMode = iota

	// sourceREPL: the interactive loop, which reads goals and approval answers
	// and records history.
	sourceREPL

	// sourceAnswerOnly: interactive planning (-goal). It reads only the
	// plan-lock approval, so it gets a source but no history.
	sourceAnswerOnly
)

func lineSourceModeFor(f flags) lineSourceMode {
	switch {
	case f.goalSet && !f.approvePlanLock:
		return sourceAnswerOnly
	case f.goalSet, f.planPath != "", f.promptSet:
		return sourceNone
	default:
		return sourceREPL
	}
}

// termOps is the seam the editor's terminal work goes through. x/term exposes
// IsTerminal, MakeRaw, Restore and GetSize as package functions over integer
// descriptors, so a fake io.ReadWriter cannot observe any of them: without this
// interface no test can tell whether MakeRaw received stdin or whether GetSize
// received stdout.
type termOps interface {
	IsTerminal(fd int) bool
	MakeRaw(fd int) (*term.State, error)
	Restore(fd int, st *term.State) error
	GetSize(fd int) (width, height int, err error)
}

// realTermOps is the production implementation: thin delegation, no logic, so
// the fake and the real path cannot drift.
type realTermOps struct{}

func (realTermOps) IsTerminal(fd int) bool               { return term.IsTerminal(fd) }
func (realTermOps) MakeRaw(fd int) (*term.State, error)  { return term.MakeRaw(fd) }
func (realTermOps) Restore(fd int, st *term.State) error { return term.Restore(fd, st) }
func (realTermOps) GetSize(fd int) (int, int, error)     { return term.GetSize(fd) }

// Compile-time assertion: the production ops must satisfy the seam.
var _ termOps = realTermOps{}

// inputConfig is everything the input layer needs to decide between the editor
// and the scanner and then to build whichever it chose.
//
// Stdin and Stdout are the real descriptors: only their fd numbers are used, by
// termOps. In and Out carry the byte streams, so tests can script input and
// capture output without a pty.
type inputConfig struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr io.Writer
	In     io.Reader // defaults to Stdin
	Out    io.Writer // defaults to Stdout

	NoEditor   bool
	UseHistory bool // true only for the default REPL; -goal reads answers only

	// OnInterrupt delivers an in-band Ctrl-C to the interrupt policy owner
	// (replControl.interrupt in production). Raw mode disables ISIG, so the
	// editor is the only component that ever sees the 0x03 byte; the scanner
	// path keeps cooked-mode SIGINT delivery and never calls this. Defaults to
	// a no-op.
	OnInterrupt func()

	Getenv func(string) string // defaults to os.Getenv
	Root   string              // workspace root, for the per-workspace history

	Ops termOps // defaults to realTermOps{}
}

func (cfg inputConfig) withDefaults() inputConfig {
	if cfg.Ops == nil {
		cfg.Ops = realTermOps{}
	}
	if cfg.In == nil && cfg.Stdin != nil {
		cfg.In = cfg.Stdin
	}
	if cfg.Out == nil && cfg.Stdout != nil {
		cfg.Out = cfg.Stdout
	}
	// Stderr carries the two warnings this layer can emit -- a degraded history
	// and a failed MakeRaw. Both fire exactly when something has already gone
	// wrong, so a nil writer would turn a recoverable problem into a panic.
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.OnInterrupt == nil {
		cfg.OnInterrupt = func() {}
	}
	return cfg
}

// newInput builds the line source for an interactive mode.
//
// The scanner is returned immediately for every declined case, so a non-TTY,
// dumb, or -no-editor run behaves exactly as it did before this feature. For a
// selected editor the scanner factory is passed in unbuilt: constructing one
// eagerly beside a live editor would start a second goroutine reading the same
// stdin, and the two would steal each other's bytes.
func newInput(cfg inputConfig) lineSource {
	cfg = cfg.withDefaults()
	if !selectsEditor(runtime.GOOS, cfg) {
		return newScannerSource(cfg.In, cfg.Out)
	}
	resize, stopResize := watchResize()
	return newEditorSource(cfg, resize, stopResize, func() lineSource {
		return newScannerSource(cfg.In, cfg.Out)
	})
}

// selectsEditor is the pure selection decision, with goos passed in so the
// Windows row is provable on any host.
//
// Windows declines regardless of TTY state, before any descriptor is probed.
// x/term's Windows makeRaw sets ENABLE_VIRTUAL_TERMINAL_INPUT on the input
// handle only and never enables ENABLE_VIRTUAL_TERMINAL_PROCESSING on stdout,
// so a working editor there needs console-output setup this ticket can verify
// only by compile-smoke. Windows keeps today's scanner, which is not a
// regression, and a follow-up can add the editor with real verification.
//
// An empty TERM selects the editor: only the explicit "dumb" terminal type
// declines, matching what "dumb" means.
func selectsEditor(goos string, cfg inputConfig) bool {
	switch {
	case cfg.NoEditor, goos == "windows", cfg.Getenv("TERM") == "dumb":
		return false
	}
	// Both descriptors must be terminals: the editor reads from one and repaints
	// on the other, and a redirected half would either lose the repaint or read
	// a file as keystrokes.
	return cfg.Ops.IsTerminal(int(cfg.Stdin.Fd())) && cfg.Ops.IsTerminal(int(cfg.Stdout.Fd()))
}

// withLineSource owns Close for the interactive modes, so no caller has to
// remember it on every return path. A Close failure is joined onto fn's error
// rather than replacing it. During a panic the deferred Close still runs and
// the panic continues unrecovered, keeping the original stack.
func withLineSource(src lineSource, fn func(lineSource) error) (err error) {
	defer func() {
		if cerr := src.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	return fn(src)
}
