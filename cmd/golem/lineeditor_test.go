package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/term"
)

// fakeTermOps stands in for golang.org/x/term's package functions. They take
// integer descriptors, so a fake io.ReadWriter cannot observe them: whether
// MakeRaw got stdin and GetSize got stdout is invisible without this seam, and
// on the macOS development host a stdin/stdout mix-up is invisible at runtime
// too (TIOCGWINSZ answers on either descriptor).
type fakeTermOps struct {
	mu sync.Mutex

	ttys        map[int]bool
	makeRawErr  error
	restoreErr  error    // applies to every Restore not covered by restoreErrs
	restoreErrs []error  // consumed in order, then restoreErr takes over
	sizes       [][2]int // consumed in order; the last entry repeats
	sizeErr     error    // applies to every GetSize not covered by sizeErrs
	sizeErrs    []error  // consumed in order, then sizeErr takes over

	// snapshotOut is read at Restore so a test can prove ordering against the
	// bytes already written to the terminal.
	snapshotOut func() string

	isTerminalFDs []int
	makeRawFDs    []int
	restoreFDs    []int
	getSizeFDs    []int
	restoreSnaps  []string

	// Identity matters, not contents: after a failed restore the terminal is
	// still raw, so which saved state a later window restores is the difference
	// between a cooked shell and a permanently raw one.
	madeStates   []*term.State
	restoredWith []*term.State
}

func (f *fakeTermOps) IsTerminal(fd int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isTerminalFDs = append(f.isTerminalFDs, fd)
	return f.ttys[fd]
}

func (f *fakeTermOps) MakeRaw(fd int) (*term.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.makeRawFDs = append(f.makeRawFDs, fd)
	if f.makeRawErr != nil {
		return nil, f.makeRawErr
	}
	// A distinct allocation per call, so a test can tell the states apart.
	st := &term.State{}
	f.madeStates = append(f.madeStates, st)
	return st, nil
}

func (f *fakeTermOps) Restore(fd int, st *term.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreFDs = append(f.restoreFDs, fd)
	f.restoredWith = append(f.restoredWith, st)
	if f.snapshotOut != nil {
		f.restoreSnaps = append(f.restoreSnaps, f.snapshotOut())
	}
	if i := len(f.restoreFDs) - 1; i < len(f.restoreErrs) {
		return f.restoreErrs[i]
	}
	return f.restoreErr
}

func (f *fakeTermOps) states() (made, restoredWith []*term.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*term.State(nil), f.madeStates...), append([]*term.State(nil), f.restoredWith...)
}

func (f *fakeTermOps) GetSize(fd int) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSizeFDs = append(f.getSizeFDs, fd)
	i := len(f.getSizeFDs) - 1
	if i < len(f.sizeErrs) && f.sizeErrs[i] != nil {
		return 0, 0, f.sizeErrs[i]
	}
	if i >= len(f.sizeErrs) && f.sizeErr != nil {
		return 0, 0, f.sizeErr
	}
	if i >= len(f.sizes) {
		i = len(f.sizes) - 1
	}
	return f.sizes[i][0], f.sizes[i][1], nil
}

func (f *fakeTermOps) counts() (makeRaw, restore, getSize int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.makeRawFDs), len(f.restoreFDs), len(f.getSizeFDs)
}

func (f *fakeTermOps) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.isTerminalFDs)
}

func (f *fakeTermOps) restoreSnapshots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.restoreSnaps...)
}

// editorFixture wires an editorSource over fakes: real descriptors only for
// their fd values, scripted bytes for input, and a buffer for output.
type editorFixture struct {
	src            *editorSource
	ops            *fakeTermOps
	out            *lockedBuffer
	errOut         *lockedBuffer
	stdinFD        int
	stdoutFD       int
	resize         chan struct{}
	stops          *int
	fallbacks      *int
	fallbackCloses *int
}

// countingCloseSource observes that the editor closes the fallback it built.
// scannerSource.Close is a no-op today, so nothing else could see the call.
type countingCloseSource struct {
	lineSource
	closes *int
}

func (c countingCloseSource) Close() error {
	*c.closes++
	return c.lineSource.Close()
}

type editorOpts struct {
	in          io.Reader
	out         io.Writer // wraps the fixture's lockedBuffer when set; assertions still read f.out
	useHistory  bool
	getenv      func(string) string
	root        string
	ops         *fakeTermOps
	resize      chan struct{}
	onInterrupt func()
}

// tempDescriptors returns two distinct real descriptors. Only their fd numbers
// matter: every terminal operation goes through the fake ops, and the byte
// streams go through cfg.In / cfg.Out.
func tempDescriptors(t *testing.T) (stdin, stdout *os.File) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.Create(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatalf("create stdin: %v", err)
	}
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatalf("create stdout: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = out.Close()
	})
	if in.Fd() == out.Fd() {
		t.Fatalf("descriptors must differ to prove routing, both are %d", in.Fd())
	}
	return in, out
}

func newEditorFixture(t *testing.T, opts editorOpts) *editorFixture {
	t.Helper()
	stdin, stdout := tempDescriptors(t)
	out := &lockedBuffer{}
	errOut := &lockedBuffer{}

	ops := opts.ops
	if ops == nil {
		ops = &fakeTermOps{}
	}
	if ops.sizes == nil {
		ops.sizes = [][2]int{{80, 24}}
	}
	if ops.ttys == nil {
		ops.ttys = map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true}
	}
	if ops.snapshotOut == nil {
		ops.snapshotOut = out.String
	}

	in := opts.in
	if in == nil {
		in = strings.NewReader("")
	}
	getenv := opts.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	f := &editorFixture{
		ops:            ops,
		out:            out,
		errOut:         errOut,
		stdinFD:        int(stdin.Fd()),
		stdoutFD:       int(stdout.Fd()),
		resize:         opts.resize,
		stops:          new(int),
		fallbacks:      new(int),
		fallbackCloses: new(int),
	}
	if f.resize == nil {
		f.resize = make(chan struct{}, 1)
	}

	var cfgOut io.Writer = out
	if opts.out != nil {
		cfgOut = opts.out
	}
	cfg := inputConfig{
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      errOut,
		In:          in,
		Out:         cfgOut,
		UseHistory:  opts.useHistory,
		Getenv:      getenv,
		Root:        opts.root,
		Ops:         ops,
		OnInterrupt: opts.onInterrupt,
	}
	var resize <-chan struct{}
	if f.resize != nil {
		resize = f.resize
	}
	f.src = newEditorSource(cfg, resize, func() { *f.stops++ }, func() lineSource {
		*f.fallbacks++
		return countingCloseSource{newScannerSource(in, out), f.fallbackCloses}
	})
	t.Cleanup(func() { _ = f.src.Close() })
	return f
}

func (f *editorFixture) readGoal(t *testing.T) (string, bool, error) {
	t.Helper()
	return f.src.ReadGoal(context.Background(), promptText)
}

const (
	editorPrompt = promptText

	// The bracketed-paste mode escapes golem owns, distinct from the pasteOn /
	// pasteOff markers a terminal sends around pasted content.
	pasteEnableSeq  = "\x1b[?2004h"
	pasteDisableSeq = "\x1b[?2004l"
)

func TestEditorSourceReadsALine(t *testing.T) {
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "hello" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"hello\" true nil", line, ok, err)
	}
}

func TestEditorSourceArrowEditing(t *testing.T) {
	// Left arrow through the real x/term parser: "ac", cursor left, insert "b".
	// A straight-line test would pass on an editor that ignored escape
	// sequences entirely, which is the whole feature.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("ac\x1b[Db\r")})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "abc" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"abc\" true nil", line, ok, err)
	}
}

func TestEditorSourceRecallsAcrossRestart(t *testing.T) {
	// History is the reason the editor owns a file at all: a goal recorded in
	// one session must come back under the up arrow in the next.
	root := t.TempDir()
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}

	first := newEditorFixture(t, editorOpts{useHistory: true, getenv: getenv, root: root})
	first.src.RecordGoal("remember me")
	if err := first.src.Close(); err != nil {
		t.Fatalf("close first source: %v", err)
	}

	second := newEditorFixture(t, editorOpts{
		in:         strings.NewReader("\x1b[A\r"),
		useHistory: true,
		getenv:     getenv,
		root:       root,
	})
	line, ok, err := second.readGoal(t)
	if err != nil || !ok || line != "remember me" {
		t.Fatalf("recalled = %q ok=%v err=%v, want \"remember me\" true nil", line, ok, err)
	}
}

func TestEditorSourceApprovalReadCannotRecallGoals(t *testing.T) {
	// The discard History is bound for the whole answer read, so an up arrow
	// at an approval prompt has nothing to offer and the answer stays empty.
	root := t.TempDir()
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}
	first := newEditorFixture(t, editorOpts{useHistory: true, getenv: getenv, root: root})
	first.src.RecordGoal("secret goal")
	if err := first.src.Close(); err != nil {
		t.Fatalf("close first source: %v", err)
	}

	second := newEditorFixture(t, editorOpts{
		in:         strings.NewReader("\x1b[A\r"),
		useHistory: true,
		getenv:     getenv,
		root:       root,
	})
	line, ok, err := second.src.ReadAnswer(context.Background(), "approve? ")
	if err != nil || !ok {
		t.Fatalf("ReadAnswer ok=%v err=%v", ok, err)
	}
	if line != "" {
		t.Fatalf("answer = %q, want empty: goal history must be unreachable here", line)
	}
}

func TestEditorSourcePrintsPromptExactlyOnce(t *testing.T) {
	// Kept separate from the notice tests on purpose: Terminal.Write
	// legitimately repaints the prompt, so only an uninterrupted read can pin
	// the count at one.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	if got := strings.Count(f.out.String(), editorPrompt); got != 1 {
		t.Fatalf("prompt printed %d times in %q, want exactly 1", got, f.out.String())
	}
}

func TestEditorSourceOneRawWindowPerCall(t *testing.T) {
	// The window spans the logical call, not each Terminal.ReadLine: flapping
	// termios or bracketed paste mid-call would corrupt a paste in flight.
	// Two lines in one stream: the second read also proves the first claimed its
	// segment flag, since the filter answers the next Read with
	// errTerminatorSwallowed while one is unclaimed.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("one\rtwo\r")})
	if line, _, err := f.readGoal(t); err != nil || line != "one" {
		t.Fatalf("first ReadGoal = %q err=%v", line, err)
	}
	makeRaw, restore, _ := f.ops.counts()
	if makeRaw != 1 || restore != 1 {
		t.Fatalf("after one call: MakeRaw=%d Restore=%d, want 1 and 1", makeRaw, restore)
	}
	if got := strings.Count(f.out.String(), pasteEnableSeq); got != 1 {
		t.Fatalf("bracketed paste enabled %d times, want 1", got)
	}
	if got := strings.Count(f.out.String(), pasteDisableSeq); got != 1 {
		t.Fatalf("bracketed paste disabled %d times, want 1", got)
	}

	if line, _, err := f.readGoal(t); err != nil || line != "two" {
		t.Fatalf("second ReadGoal = %q err=%v", line, err)
	}
	makeRaw, restore, _ = f.ops.counts()
	if makeRaw != 2 || restore != 2 {
		t.Fatalf("after two calls: MakeRaw=%d Restore=%d, want 2 and 2", makeRaw, restore)
	}
}

func TestEditorSourceDisablesPasteBeforeRestore(t *testing.T) {
	// term.Restore resets termios; it does not emit ESC[?2004l. Disabling
	// after the restore would leak paste markers into the cooked-mode input
	// queue and into the user's shell after exit.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	snaps := f.ops.restoreSnapshots()
	if len(snaps) != 1 {
		t.Fatalf("Restore called %d times, want 1", len(snaps))
	}
	if !strings.Contains(snaps[0], pasteDisableSeq) {
		t.Fatalf("paste was still enabled at Restore; output so far was %q", snaps[0])
	}
}

func TestEditorSourceDescriptorRouting(t *testing.T) {
	// x/term's Windows GetSize calls GetConsoleScreenBufferInfo, an
	// output-handle API, while Unix TIOCGWINSZ answers on either descriptor. A
	// mix-up is therefore invisible on this host and wrong on Windows.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	f.ops.mu.Lock()
	makeRawFDs := append([]int(nil), f.ops.makeRawFDs...)
	restoreFDs := append([]int(nil), f.ops.restoreFDs...)
	getSizeFDs := append([]int(nil), f.ops.getSizeFDs...)
	f.ops.mu.Unlock()

	for _, fd := range makeRawFDs {
		if fd != f.stdinFD {
			t.Fatalf("MakeRaw got fd %d, want stdin %d", fd, f.stdinFD)
		}
	}
	for _, fd := range restoreFDs {
		if fd != f.stdinFD {
			t.Fatalf("Restore got fd %d, want stdin %d", fd, f.stdinFD)
		}
	}
	if len(getSizeFDs) == 0 {
		t.Fatal("GetSize was never called; the editor cannot size the terminal")
	}
	for _, fd := range getSizeFDs {
		if fd != f.stdoutFD {
			t.Fatalf("GetSize got fd %d, want stdout %d", fd, f.stdoutFD)
		}
	}
}

func TestInputConfigDefaultsEveryStream(t *testing.T) {
	// Stderr carries the degraded-history and failed-MakeRaw warnings, both of
	// which fire when something has already gone wrong. A nil writer there would
	// turn a recoverable problem into a panic, so it defaults like the others.
	stdin, stdout := tempDescriptors(t)
	cfg := inputConfig{Stdin: stdin, Stdout: stdout}.withDefaults()
	if cfg.Stderr == nil {
		t.Fatal("Stderr left nil; the two warning paths would panic")
	}
	if cfg.In == nil || cfg.Out == nil || cfg.Getenv == nil || cfg.Ops == nil {
		t.Fatalf("undefaulted field: In=%v Out=%v Getenv=%v Ops=%v",
			cfg.In != nil, cfg.Out != nil, cfg.Getenv != nil, cfg.Ops != nil)
	}
}

func TestSelectsEditor(t *testing.T) {
	stdin, stdout := tempDescriptors(t)
	inFD, outFD := int(stdin.Fd()), int(stdout.Fd())

	base := func() inputConfig {
		return inputConfig{
			Stdin:  stdin,
			Stdout: stdout,
			Getenv: func(string) string { return "" },
			Ops:    &fakeTermOps{ttys: map[int]bool{inFD: true, outFD: true}},
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*inputConfig)
		want    bool
		noProbe bool
	}{
		{name: "tty on both descriptors", mutate: func(*inputConfig) {}, want: true},
		{name: "empty TERM still selects the editor", mutate: func(c *inputConfig) {
			c.Getenv = func(string) string { return "" }
		}, want: true},
		{name: "TERM=dumb", mutate: func(c *inputConfig) {
			c.Getenv = func(k string) string {
				if k == "TERM" {
					return "dumb"
				}
				return ""
			}
		}, want: false, noProbe: true},
		{name: "-no-editor", mutate: func(c *inputConfig) { c.NoEditor = true }, want: false, noProbe: true},
		{name: "stdin is not a tty", mutate: func(c *inputConfig) {
			c.Ops = &fakeTermOps{ttys: map[int]bool{outFD: true}}
		}, want: false},
		{name: "stdout is not a tty", mutate: func(c *inputConfig) {
			c.Ops = &fakeTermOps{ttys: map[int]bool{inFD: true}}
		}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			if got := selectsEditor("linux", cfg); got != tc.want {
				t.Fatalf("selectsEditor = %v, want %v", got, tc.want)
			}
			if tc.noProbe {
				if probes := cfg.Ops.(*fakeTermOps).probes(); probes != 0 {
					t.Fatalf("declined without a descriptor probe, got %d probes", probes)
				}
			}
		})
	}
}

func TestSelectsEditorWindowsDeclinesWithoutProbing(t *testing.T) {
	// x/term's Windows makeRaw sets ENABLE_VIRTUAL_TERMINAL_INPUT on the input
	// handle only and never enables ENABLE_VIRTUAL_TERMINAL_PROCESSING on
	// stdout, so a raw-mode takeover there needs console-output setup this
	// slice cannot verify. Windows keeps today's scanner.
	stdin, stdout := tempDescriptors(t)
	ttys := map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true}

	ops := &fakeTermOps{ttys: ttys}
	cfg := inputConfig{Stdin: stdin, Stdout: stdout, Getenv: func(string) string { return "" }, Ops: ops}
	if selectsEditor("windows", cfg) {
		t.Fatal("windows selected the editor")
	}
	if probes := ops.probes(); probes != 0 {
		t.Fatalf("windows probed %d descriptors before declining, want 0", probes)
	}

	for _, goos := range []string{"linux", "darwin", "freebsd", "openbsd"} {
		ops := &fakeTermOps{ttys: ttys}
		cfg := inputConfig{Stdin: stdin, Stdout: stdout, Getenv: func(string) string { return "" }, Ops: ops}
		if !selectsEditor(goos, cfg) {
			t.Fatalf("%s declined the editor on a full TTY", goos)
		}
	}
}

func TestNewInputSelectsTheScannerWithoutATTY(t *testing.T) {
	stdin, stdout := tempDescriptors(t)
	src := newInput(inputConfig{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: &lockedBuffer{},
		In:     strings.NewReader("hello\n"),
		Out:    &lockedBuffer{},
		Getenv: func(string) string { return "" },
		Ops:    &fakeTermOps{}, // no descriptor is a terminal
	})
	t.Cleanup(func() { _ = src.Close() })
	if _, isScanner := src.(*scannerSource); !isScanner {
		t.Fatalf("newInput returned %T, want *scannerSource", src)
	}
}

func TestNewInputSelectsTheEditorOnATTY(t *testing.T) {
	stdin, stdout := tempDescriptors(t)
	src := newInput(inputConfig{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: &lockedBuffer{},
		In:     strings.NewReader("hello\r"),
		Out:    &lockedBuffer{},
		Getenv: func(string) string { return "" },
		Ops: &fakeTermOps{
			ttys:  map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true},
			sizes: [][2]int{{80, 24}},
		},
	})
	t.Cleanup(func() { _ = src.Close() })
	if _, isEditor := src.(*editorSource); !isEditor {
		t.Fatalf("newInput returned %T, want *editorSource", src)
	}
}

func TestEditorSourceFallbackIsLazy(t *testing.T) {
	// Constructing a scanner beside a live editor would start a second
	// goroutine reading the same stdin, and the two would steal each other's
	// bytes. The factory must stay uncalled until MakeRaw actually fails.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	if *f.fallbacks != 0 {
		t.Fatalf("fallback constructed eagerly: %d times", *f.fallbacks)
	}
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	if *f.fallbacks != 0 {
		t.Fatalf("successful read constructed a fallback: %d times", *f.fallbacks)
	}
}

func TestEditorSourceMakeRawFailureFallsBackOnce(t *testing.T) {
	ops := &fakeTermOps{makeRawErr: errors.New("no raw mode here")}
	f := newEditorFixture(t, editorOpts{
		in:  strings.NewReader("first\nsecond\n"), // scanner semantics: LF-terminated
		ops: ops,
	})

	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "first" {
		t.Fatalf("fallback read = %q ok=%v err=%v, want \"first\" true nil", line, ok, err)
	}
	if *f.fallbacks != 1 {
		t.Fatalf("fallback factory called %d times, want 1", *f.fallbacks)
	}
	if got := strings.Count(f.errOut.String(), "no raw mode here"); got != 1 {
		t.Fatalf("stderr warned %d times in %q, want exactly 1", got, f.errOut.String())
	}
	if *f.stops != 1 {
		t.Fatalf("resize delivery stopped %d times on fallback, want 1", *f.stops)
	}

	// The swap covers IdleDisplay too: a notice landing after the fallback must
	// render through the scanner, never through a half-initialized editor.
	f.out.Reset()
	f.src.IdleDisplay("heads up")
	if got, want := f.out.String(), "heads up\n"; got != want {
		t.Fatalf("IdleDisplay after fallback wrote %q, want the scanner's %q", got, want)
	}

	// A second read reuses the same scanner rather than building another.
	line, ok, err = f.readGoal(t)
	if err != nil || !ok || line != "second" {
		t.Fatalf("second fallback read = %q ok=%v err=%v", line, ok, err)
	}
	if *f.fallbacks != 1 {
		t.Fatalf("fallback factory called %d times across two reads, want 1", *f.fallbacks)
	}
	if makeRaw, restore, _ := f.ops.counts(); restore != 0 {
		t.Fatalf("MakeRaw=%d Restore=%d: a failed MakeRaw must not be restored", makeRaw, restore)
	}

	// The editor built the fallback, so it owns closing it.
	if err := f.src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if *f.fallbackCloses != 1 {
		t.Fatalf("fallback closed %d times, want 1", *f.fallbackCloses)
	}
	if err := f.src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if *f.fallbackCloses != 1 {
		t.Fatalf("fallback closed %d times across two closes, want 1", *f.fallbackCloses)
	}
}

func TestEditorSourceResizeAppliesStdoutSize(t *testing.T) {
	ops := &fakeTermOps{sizes: [][2]int{{80, 24}, {100, 24}}}
	resize := make(chan struct{}, 1)
	pr, pw := newBlockedPipe(t)
	f := newEditorFixture(t, editorOpts{in: pr, ops: ops, resize: resize})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.readGoal(t)
	}()
	if _, err := pw.Write([]byte("he")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, func() bool { return strings.Contains(f.out.String(), editorPrompt) })

	resize <- struct{}{}
	waitFor(t, func() bool {
		_, _, getSize := f.ops.counts()
		return getSize >= 2
	})
	// A width change repaints the prompt plus the partially typed line, which
	// is only possible on the currently bound Terminal.
	waitFor(t, func() bool { return strings.Count(f.out.String(), editorPrompt) >= 2 })

	f.ops.mu.Lock()
	getSizeFDs := append([]int(nil), f.ops.getSizeFDs...)
	f.ops.mu.Unlock()
	for _, fd := range getSizeFDs {
		if fd != f.stdoutFD {
			t.Fatalf("resize sized from fd %d, want stdout %d", fd, f.stdoutFD)
		}
	}

	if _, err := pw.Write([]byte("y\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
}

func TestEditorSourceResizeSurvivesASizeQueryFailure(t *testing.T) {
	// A watcher that returned instead of continuing would silently stop
	// resizing for the rest of the session, and nothing else would report it.
	ops := &fakeTermOps{
		sizes:    [][2]int{{80, 24}, {80, 24}, {100, 24}},
		sizeErrs: []error{nil, errors.New("size query failed")},
	}
	resize := make(chan struct{}, 1)
	pr, pw := newBlockedPipe(t)
	f := newEditorFixture(t, editorOpts{in: pr, ops: ops, resize: resize})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.readGoal(t)
	}()
	if _, err := pw.Write([]byte("he")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, func() bool { return strings.Contains(f.out.String(), editorPrompt) })

	resize <- struct{}{} // GetSize call 2: fails, nothing to apply
	waitFor(t, func() bool {
		_, _, getSize := f.ops.counts()
		return getSize >= 2
	})

	resize <- struct{}{} // GetSize call 3: succeeds, so the watcher is still alive
	waitFor(t, func() bool { return strings.Count(f.out.String(), editorPrompt) >= 2 })

	if _, err := pw.Write([]byte("y\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
}

func TestEditorSourceCloseWithPendingResize(t *testing.T) {
	// The forwarder coalesces into a capacity-one channel, so a resize can be
	// pending with nobody about to read it. Stop and join must not block on it.
	resize := make(chan struct{}, 1)
	resize <- struct{}{}
	f := newEditorFixture(t, editorOpts{resize: resize})

	closed := make(chan error, 1)
	go func() { closed <- f.src.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked with a coalesced resize pending")
	}
	if *f.stops != 1 {
		t.Fatalf("resize stopped %d times, want 1", *f.stops)
	}
}

func TestEditorSourceCloseLifecycle(t *testing.T) {
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	_, restoreAfterRead, _ := f.ops.counts()
	pasteOffAfterRead := strings.Count(f.out.String(), pasteDisableSeq)

	if err := f.src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	_, restore, _ := f.ops.counts()
	if restore != restoreAfterRead {
		t.Fatalf("Close issued %d extra Restore calls; the read already restored", restore-restoreAfterRead)
	}
	if got := strings.Count(f.out.String(), pasteDisableSeq); got != pasteOffAfterRead {
		t.Fatalf("Close issued an unmatched paste-disable: %d, want %d", got, pasteOffAfterRead)
	}
	if *f.stops != 1 {
		t.Fatalf("resize stopped %d times across two closes, want 1", *f.stops)
	}
}

func TestEditorSourceRestoreFailure(t *testing.T) {
	restoreErr := errors.New("restore failed")

	t.Run("successful read still returns the restore error", func(t *testing.T) {
		// Returning the line would let a turn render on a terminal still in raw
		// mode, where every renderer newline staircases.
		ops := &fakeTermOps{restoreErr: restoreErr}
		f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r"), ops: ops})
		line, ok, err := f.readGoal(t)
		if !errors.Is(err, restoreErr) {
			t.Fatalf("err = %v, want %v", err, restoreErr)
		}
		if ok || line != "" {
			t.Fatalf("read returned %q ok=%v despite a failed restore", line, ok)
		}
	})

	t.Run("read error is joined with the restore error", func(t *testing.T) {
		readErr := errors.New("stdin exploded")
		ops := &fakeTermOps{restoreErr: restoreErr}
		f := newEditorFixture(t, editorOpts{in: &erroringReader{err: readErr}, ops: ops})
		_, _, err := f.readGoal(t)
		if !errors.Is(err, readErr) || !errors.Is(err, restoreErr) {
			t.Fatalf("err = %v, want both %v and %v", err, readErr, restoreErr)
		}
	})

	t.Run("a later window restores the oldest saved state", func(t *testing.T) {
		// After a failed Restore the terminal is still raw, so the next MakeRaw
		// hands back the RAW settings as the "previous" state. Restoring those
		// at the end of that window would pin the terminal in raw mode for good
		// and, worse, would clear the pending state so Close had nothing to
		// retry. Only the oldest saved state returns the shell to cooked.
		ops := &fakeTermOps{restoreErrs: []error{restoreErr}}
		f := newEditorFixture(t, editorOpts{in: strings.NewReader("one\rtwo\r"), ops: ops})

		if _, _, err := f.readGoal(t); !errors.Is(err, restoreErr) {
			t.Fatalf("first read err = %v, want %v", err, restoreErr)
		}
		if line, ok, err := f.readGoal(t); err != nil || !ok || line != "two" {
			t.Fatalf("second read = %q ok=%v err=%v, want it to succeed", line, ok, err)
		}

		made, restoredWith := ops.states()
		if len(made) != 2 || len(restoredWith) != 2 {
			t.Fatalf("MakeRaw %d times, Restore %d times, want 2 and 2", len(made), len(restoredWith))
		}
		if restoredWith[1] != made[0] {
			t.Fatalf("second window restored the state MakeRaw returned while the terminal was already raw; want the original saved state")
		}

		// The successful restore consumed the pending state, so Close has
		// nothing left to retry.
		if err := f.src.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, restore, _ := f.ops.counts(); restore != 2 {
			t.Fatalf("Restore called %d times, want 2: a completed restore leaves nothing pending", restore)
		}
	})

	t.Run("close retries the saved state exactly once", func(t *testing.T) {
		ops := &fakeTermOps{restoreErr: restoreErr}
		f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r"), ops: ops})
		if _, _, err := f.readGoal(t); !errors.Is(err, restoreErr) {
			t.Fatalf("read err = %v, want %v", err, restoreErr)
		}
		if _, restore, _ := f.ops.counts(); restore != 1 {
			t.Fatalf("Restore called %d times during the read, want 1", restore)
		}
		if err := f.src.Close(); !errors.Is(err, restoreErr) {
			t.Fatalf("Close err = %v, want the retry failure %v", err, restoreErr)
		}
		if _, restore, _ := f.ops.counts(); restore != 2 {
			t.Fatalf("Restore called %d times after Close, want 2 (one read, one retry)", restore)
		}
		if err := f.src.Close(); !errors.Is(err, restoreErr) {
			t.Fatalf("second Close err = %v, want the remembered %v", err, restoreErr)
		}
		if _, restore, _ := f.ops.counts(); restore != 2 {
			t.Fatalf("Restore called %d times after a second Close, want exactly one retry", restore)
		}
	})
}

// erroringReader fails every read, standing in for a broken descriptor.
type erroringReader struct{ err error }

func (e *erroringReader) Read([]byte) (int, error) { return 0, e.err }

// blockingWriter parks the first Write until the test releases it, so a barrier
// can prove what the binding mutex excludes.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buf     strings.Builder
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestTerminalBindingSerializesReplacement(t *testing.T) {
	// Without this exclusion a notice already inside Terminal.Write repaints the
	// buffer of a Terminal that has just been discarded.
	first := newBlockingWriter()
	var second strings.Builder
	b := &terminalBinding{}
	b.replace(term.NewTerminal(termIO{r: strings.NewReader(""), w: first}, "> "))

	displayed := make(chan struct{})
	go func() {
		defer close(displayed)
		b.idleDisplay("parked")
	}()
	<-first.entered

	replaced := make(chan struct{})
	go func() {
		defer close(replaced)
		b.replace(term.NewTerminal(termIO{r: strings.NewReader(""), w: &second}, "> "))
	}()
	select {
	case <-replaced:
		t.Fatal("replacement proceeded while a write held the binding")
	case <-time.After(50 * time.Millisecond):
	}

	close(first.release)
	<-displayed
	select {
	case <-replaced:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement never completed after the write finished")
	}

	b.idleDisplay("after")
	if strings.Contains(first.String(), "after") {
		t.Fatalf("notice reached the discarded Terminal: %q", first.String())
	}
	if !strings.Contains(second.String(), "after") {
		t.Fatalf("notice never reached the replacement: %q", second.String())
	}
}

func TestEditorSourceIdleDisplayDuringInFlightRead(t *testing.T) {
	// The sole reader snapshots the Terminal under the binding mutex and then
	// releases it before blocking in ReadLine. Holding it across the read would
	// deadlock every asynchronous notice for as long as the user is thinking.
	pr, pw := newBlockedPipe(t)
	f := newEditorFixture(t, editorOpts{in: pr})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.readGoal(t)
	}()
	waitFor(t, func() bool { return strings.Contains(f.out.String(), editorPrompt) })

	displayed := make(chan struct{})
	go func() {
		defer close(displayed)
		f.src.IdleDisplay("async note")
	}()
	select {
	case <-displayed:
	case <-time.After(5 * time.Second):
		t.Fatal("IdleDisplay blocked while a read was in flight")
	}
	waitFor(t, func() bool { return strings.Contains(f.out.String(), "async note") })

	if _, err := pw.Write([]byte("x\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
}

func TestEditorSourceIdleDisplayBeforeTheFirstRead(t *testing.T) {
	// main.run binds the display before starting auto-indexing, so a startup
	// warning can land before any read has built a Terminal. Dropping it there
	// would lose exactly the messages the notice path exists for.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	f.src.IdleDisplay("startup warning")
	if got, want := f.out.String(), "startup warning\n"; got != want {
		t.Fatalf("IdleDisplay before the first read wrote %q, want %q", got, want)
	}

	// The upcoming read still owns the prompt: the message must not have
	// printed one of its own.
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	if got := strings.Count(f.out.String(), editorPrompt); got != 1 {
		t.Fatalf("prompt printed %d times in %q, want exactly 1", got, f.out.String())
	}
}

func TestEditorSourceReadSurvivesASizeQueryFailure(t *testing.T) {
	// A terminal that cannot report its size is still usable; x/term keeps its
	// 80x24 default. Failing the read would be a worse outcome than a wrong
	// wrap width.
	ops := &fakeTermOps{sizeErr: errors.New("no size for you")}
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r"), ops: ops})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "hello" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want the line despite the size failure", line, ok, err)
	}
}

func TestDiscardHistorySatisfiesTermContract(t *testing.T) {
	var h term.History = discardHistory{}
	h.Add("a goal")
	if h.Len() != 0 {
		t.Fatalf("Len = %d after Add, want 0: an approval read must store nothing", h.Len())
	}
	defer func() {
		if recover() == nil {
			t.Fatal("At did not panic on an out-of-range index, which term.History requires")
		}
	}()
	_ = h.At(0)
}

func TestEditorSourceRefusesToOpenAWindowOnADeadContext(t *testing.T) {
	// A blocked tty read cannot be interrupted by ctx, but an already-cancelled
	// one is free to honor -- and must be, or a user who has already been told
	// the session is over gets a fresh raw prompt.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("hello\r")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		read func() (string, bool, error)
	}{
		{"goal", func() (string, bool, error) { return f.src.ReadGoal(ctx, promptText) }},
		{"answer", func() (string, bool, error) { return f.src.ReadAnswer(ctx, "approve? ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, ok, err := tc.read()
			if !errors.Is(err, context.Canceled) || ok || line != "" {
				t.Fatalf("read = %q ok=%v err=%v, want the context error", line, ok, err)
			}
		})
	}
	if makeRaw, _, _ := f.ops.counts(); makeRaw != 0 {
		t.Fatalf("MakeRaw called %d times on a dead context, want 0", makeRaw)
	}
	if got := f.out.String(); got != "" {
		t.Fatalf("a cancelled read still printed %q", got)
	}
}

func TestEditorSourceCleanEOF(t *testing.T) {
	// Ctrl-D on an empty line and a closed stdin are the same io.EOF above the
	// Terminal, and both are a clean exit rather than an error.
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"closed stdin", ""},
		{"ctrl-d on an empty line", "\x04"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditorFixture(t, editorOpts{in: strings.NewReader(tc.in)})
			line, ok, err := f.readGoal(t)
			if err != nil || ok || line != "" {
				t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"\" false nil", line, ok, err)
			}
		})
	}
}

func TestEditorSourceRecordGoalWritesHistory(t *testing.T) {
	root := t.TempDir()
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}
	f := newEditorFixture(t, editorOpts{useHistory: true, getenv: getenv, root: root})
	f.src.RecordGoal("a goal")
	if err := f.src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(xdg, "golem", "history", strings.ReplaceAll(workspaceID(root), ":", "-"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if !strings.Contains(string(data), "a goal") {
		t.Fatalf("history file %q does not contain the recorded goal", string(data))
	}
}

func TestEditorSourceWithoutHistoryRecordsNothing(t *testing.T) {
	// Interactive -goal shares the editor but must not accumulate a file: it
	// only ever reads a plan-lock approval.
	root := t.TempDir()
	xdg := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return xdg
		}
		return ""
	}
	f := newEditorFixture(t, editorOpts{getenv: getenv, root: root})
	f.src.RecordGoal("a goal")
	if err := f.src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(xdg, "golem")); !os.IsNotExist(err) {
		t.Fatalf("history directory exists without UseHistory (stat err = %v)", err)
	}
}

func TestEditorSourceComposesAPastedGoal(t *testing.T) {
	// Three pasted lines then a typed Enter are ONE goal. Relying on
	// term.ErrPasteIndicator here would submit "a" alone; provenance comes
	// from the filter's segment flags instead.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(pasteOn + "a\nb\nc" + pasteOff + "\r")})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "a\nb\nc" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"a\\nb\\nc\" true nil", line, ok, err)
	}
}

func TestEditorSourceDropsATrailingEmptyPasteSegment(t *testing.T) {
	// A paste ending in a newline yields a final empty segment when the user
	// presses Enter; the goal must not grow a trailing blank line from it.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(pasteOn + "a\nb\nc\n" + pasteOff + "\r")})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "a\nb\nc" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"a\\nb\\nc\" true nil", line, ok, err)
	}
}

func TestEditorSourceTypedPrefixThenPasteIsOneGoal(t *testing.T) {
	// The classic ErrPasteIndicator trap: a typed prefix followed by a
	// multiline paste returns its first segment with a nil error. Submitting
	// early would turn the rest of the paste into separate goals.
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("fix " + pasteOn + "a\nb" + pasteOff + "\r")})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "fix a\nb" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"fix a\\nb\" true nil", line, ok, err)
	}
	if line, ok, err := f.readGoal(t); ok || err != nil {
		t.Fatalf("second ReadGoal = %q ok=%v err=%v, want a clean EOF: nothing may submit early", line, ok, err)
	}
}

func TestEditorSourceBackslashParity(t *testing.T) {
	// Spec 8.2: a trailing run of n backslashes emits n/2 literals; odd n
	// additionally continues the goal. Interior backslashes are untouched.
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"n=1 continues", `foo\` + "\r" + "bar\r", "foo\nbar"},
		{"n=2 literal", `foo\\` + "\r", `foo\`},
		{"n=3 literal plus continue", `foo\\\` + "\r" + "bar\r", "foo\\\nbar"},
		{"n=4 two literals", `foo\\\\` + "\r", `foo\\`},
		{"interior untouched", `a\b` + "\r", `a\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditorFixture(t, editorOpts{in: strings.NewReader(tc.in)})
			line, ok, err := f.readGoal(t)
			if err != nil || !ok || line != tc.want {
				t.Fatalf("ReadGoal = %q ok=%v err=%v, want %q true nil", line, ok, err, tc.want)
			}
		})
	}
}

func TestEditorSourceContinuationPrompt(t *testing.T) {
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(`foo\` + "\r" + "bar\r")})
	if _, _, err := f.readGoal(t); err != nil {
		t.Fatalf("ReadGoal: %v", err)
	}
	if !strings.Contains(f.out.String(), continuationPrompt) {
		t.Fatalf("output %q does not show the continuation prompt %q", f.out.String(), continuationPrompt)
	}
}

func TestEditorSourceWarnsOnlyOnARefusedInsertion(t *testing.T) {
	// x/term drops the keystroke that would exceed 4096 runes. A line of
	// exactly 4096 runes is complete and untruncated, so warning on returned
	// length is a false positive; only the refused 4097th insertion warns.
	for _, tc := range []struct {
		runes int
		warn  bool
	}{
		{4095, false},
		{4096, false},
		{4097, true},
	} {
		f := newEditorFixture(t, editorOpts{in: strings.NewReader(strings.Repeat("a", tc.runes) + "\r")})
		wantLine := strings.Repeat("a", min(tc.runes, maxEditorRunes))
		line, ok, err := f.readGoal(t)
		if err != nil || !ok || line != wantLine {
			t.Fatalf("%d runes: ReadGoal ok=%v err=%v len=%d, want len=%d true nil", tc.runes, ok, err, len(line), len(wantLine))
		}
		if got := strings.Contains(f.out.String(), lineLimitWarning); got != tc.warn {
			t.Fatalf("%d runes: warning present=%v, want %v; exact text %q", tc.runes, got, tc.warn, lineLimitWarning)
		}
	}
}

func TestEditorSourceNoWarningForANonPrintingKeyAtTheLimit(t *testing.T) {
	// The predicate must match x/term's unexported isPrintable exactly:
	// key >= 32 && !(0xd800 <= key && key <= 0xdbff). BEL is below 32; an
	// unknown escape becomes keyUnknown (0xd800), inside the surrogate hole.
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"control byte", "\x07"},
		{"unknown escape becomes a surrogate key", "\x1b[z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.Repeat("a", maxEditorRunes) + tc.key + "\r"
			f := newEditorFixture(t, editorOpts{in: strings.NewReader(in)})
			line, ok, err := f.readGoal(t)
			if err != nil || !ok || len(line) != maxEditorRunes {
				t.Fatalf("ReadGoal ok=%v err=%v len=%d, want a full 4096-rune line", ok, err, len(line))
			}
			if strings.Contains(f.out.String(), "warning: input line") {
				t.Fatalf("a non-printing key at the limit warned; output %q", f.out.String())
			}
		})
	}
}

func TestEditorSourceOversizedPasteWarnsRecreatesAndSubmitsNothing(t *testing.T) {
	// A single-line bracketed paste beyond 1 MiB: forwarding stops at the
	// bound, the paste drains through its end marker, the Terminal is
	// recreated, the warning prints, and nothing is submitted.
	big := strings.Repeat("a", maxGoalBytes+2)
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(pasteOn + big + pasteOff)})
	line, ok, err := f.readGoal(t)
	if err != nil || ok || line != "" {
		t.Fatalf("ReadGoal = len %d ok=%v err=%v, want \"\" false nil", len(line), ok, err)
	}
	out := f.out.String()
	if !strings.Contains(out, goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
	if got := strings.Count(out, pasteEnableSeq); got != 2 {
		t.Fatalf("bracketed paste enabled %d times, want 2: the Terminal must be recreated after the rejection", got)
	}
}

func TestEditorSourceOversizedPasteTruncatedByEOF(t *testing.T) {
	// The stream ends before the paste-end marker: still warn and recreate,
	// then report the EOF as a clean exit rather than an error.
	big := strings.Repeat("a", maxGoalBytes+2)
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(pasteOn + big)})
	line, ok, err := f.readGoal(t)
	if err != nil || ok || line != "" {
		t.Fatalf("ReadGoal = len %d ok=%v err=%v, want \"\" false nil", len(line), ok, err)
	}
	if !strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
}

func TestEditorSourceOversizedPasteTruncatedByReaderErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	big := strings.Repeat("a", maxGoalBytes+2)
	r := &chunkReader{chunks: [][]byte{[]byte(pasteOn + big)}, err: boom}
	f := newEditorFixture(t, editorOpts{in: r})
	_, ok, err := f.readGoal(t)
	if ok || !errors.Is(err, boom) || !errors.Is(err, errPasteTooLarge) {
		t.Fatalf("ReadGoal ok=%v err=%v, want the reader error joined with errPasteTooLarge", ok, err)
	}
	if !strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
}

func TestEditorSourceAggregateCeilingRejectsTheJoinedGoal(t *testing.T) {
	// Each paste is under the per-paste budget, but the composed goal joined
	// with a continuation exceeds 1 MiB: warn and submit nothing.
	half := strings.Repeat("a", 600*1024)
	in := pasteOn + half + pasteOff + `\` + "\r" + pasteOn + half + pasteOff + "\r"
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(in)})
	line, ok, err := f.readGoal(t)
	if err != nil || ok || line != "" {
		t.Fatalf("ReadGoal = len %d ok=%v err=%v, want \"\" false nil", len(line), ok, err)
	}
	if !strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
}

func TestEditorSourceAggregateCeilingMidPasteDrainsTheRemainder(t *testing.T) {
	// The rejecting segment arrives inside a paste. The rest of that paste
	// must be drained and discarded, or its lines become later goals.
	half := strings.Repeat("a", 600*1024)
	in := pasteOn + half + pasteOff + `\` + "\r" +
		pasteOn + half + "\nDO NOT RUN" + pasteOff + "\r"
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(in)})
	line, ok, err := f.readGoal(t)
	if err != nil || ok || line != "" {
		t.Fatalf("ReadGoal = %.40q ok=%v err=%v, want \"\" false nil", line, ok, err)
	}
	if !strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
}

func TestEditorSourcePropagatesReaderErrors(t *testing.T) {
	// Only term.ErrPasteIndicator is ignored; every other ReadLine error
	// reaches the caller.
	boom := errors.New("boom")
	r := &chunkReader{chunks: [][]byte{[]byte("par")}, err: boom}
	f := newEditorFixture(t, editorOpts{in: r})
	_, ok, err := f.readGoal(t)
	if ok || !errors.Is(err, boom) {
		t.Fatalf("ReadGoal ok=%v err=%v, want the reader error", ok, err)
	}
}

func TestEditorSourceAggregateCeilingIsExact(t *testing.T) {
	// The ceiling counts the joining newline: a composed goal of exactly
	// maxGoalBytes is accepted -- matching what the scanner path always
	// admitted -- and one byte more is rejected. Off-by-one arithmetic in
	// either direction fails one of the two.
	first := strings.Repeat("a", 600*1024)
	for _, tc := range []struct {
		name   string
		second int
		accept bool
	}{
		{"exactly at the ceiling", maxGoalBytes - len(first) - 1, true},
		{"one byte over", maxGoalBytes - len(first), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := strings.Repeat("b", tc.second)
			in := pasteOn + first + pasteOff + `\` + "\r" + pasteOn + second + pasteOff + "\r"
			f := newEditorFixture(t, editorOpts{in: strings.NewReader(in)})
			line, ok, err := f.readGoal(t)
			if err != nil {
				t.Fatalf("ReadGoal err = %v", err)
			}
			if tc.accept {
				if !ok || line != first+"\n"+second {
					t.Fatalf("ReadGoal ok=%v len=%d, want the %d-byte goal accepted", ok, len(line), maxGoalBytes)
				}
				return
			}
			if ok || line != "" {
				t.Fatalf("ReadGoal ok=%v len=%d, want rejection one byte past the ceiling", ok, len(line))
			}
			if !strings.Contains(f.out.String(), goalLimitWarning) {
				t.Fatalf("output does not contain %q", goalLimitWarning)
			}
		})
	}
}

func TestEditorSourceCtrlCDiscardsRetainedBytesAndContinues(t *testing.T) {
	// Ctrl-C at the prompt: filter-retained bytes past 0x03 are discarded
	// (cooked-mode SIGINT flushes the input queue, so this is fidelity), the
	// kernel queue -- bytes still in the reader -- survives, the Terminal is
	// recreated, and the next line typed becomes the next goal with no
	// sacrificial Enter. All inside ONE raw window.
	r := &chunkReader{chunks: [][]byte{[]byte("junk\x03XYZ"), []byte("ok\r")}}
	var calls int
	f := newEditorFixture(t, editorOpts{in: r, onInterrupt: func() { calls++ }})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "ok" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"ok\" true nil", line, ok, err)
	}
	if calls != 1 {
		t.Fatalf("OnInterrupt called %d times, want 1", calls)
	}
	if got := strings.Count(f.out.String(), pasteEnableSeq); got != 2 {
		t.Fatalf("paste enabled %d times, want 2: the Terminal must be recreated after Ctrl-C", got)
	}
	makeRaw, restore, getSize := f.ops.counts()
	if makeRaw != 1 || restore != 1 {
		t.Fatalf("MakeRaw=%d Restore=%d, want 1/1: the cycle stays inside one raw window", makeRaw, restore)
	}
	if getSize != 2 {
		t.Fatalf("GetSize called %d times, want 2: the replacement Terminal must be sized before use", getSize)
	}
}

func TestEditorSourceCtrlCHintBeginsOnAFreshLine(t *testing.T) {
	// The replacement Terminal has cursorX/cursorY == 0 and cannot know where
	// the discarded line left the physical cursor, so the editor writes an
	// explicit CRLF before the interrupt owner prints its hint.
	r := &chunkReader{chunks: [][]byte{[]byte("\x03"), []byte("ok\r")}}
	var f *editorFixture
	f = newEditorFixture(t, editorOpts{in: r, onInterrupt: func() { f.src.IdleDisplay(ctrlCHint) }})
	if line, _, err := f.readGoal(t); err != nil || line != "ok" {
		t.Fatalf("ReadGoal = %q err=%v", line, err)
	}
	if !strings.Contains(f.out.String(), "\r\n"+ctrlCHint) {
		t.Fatalf("hint not preceded by an explicit CRLF in %q", f.out.String())
	}
}

func TestEditorSourceSecondCtrlCQuitsWithoutReenteringPrompt(t *testing.T) {
	// The arm/hint cycle never leaves ReadGoal: the first idle Ctrl-C arms and
	// hints, the second quits via replControl. Because both presses live in a
	// single ReadGoal call, runREPL cannot call enterPrompt in between, which
	// is exactly what keeps the arm from being cleared.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interrupts := make(chan struct{}, 1)
	ctrl := newReplControl(io.Discard, io.Discard, interrupts, cancel)
	r := &chunkReader{chunks: [][]byte{[]byte("partial\x03"), []byte("\x03")}}
	f := newEditorFixture(t, editorOpts{in: r, onInterrupt: ctrl.interrupt})
	ctrl.setIdleDisplay(f.src.IdleDisplay)
	ctrl.enterPrompt()

	line, ok, err := f.src.ReadGoal(ctx, promptText)
	if !errors.Is(err, context.Canceled) || ok || line != "" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want the canceled context", line, ok, err)
	}
	if got := strings.Count(f.out.String(), ctrlCHint); got != 1 {
		t.Fatalf("hint printed %d times, want exactly 1 (first press arms, second quits)", got)
	}
}

func TestEditorSourceCtrlCDiscardsComposition(t *testing.T) {
	// An interrupted partial goal -- a backslash continuation plus a partial
	// paste -- must not leak into the next goal or be recorded.
	in := `foo\` + "\r" + pasteOn + "a\nb" + pasteOff + "\x03"
	r := &chunkReader{chunks: [][]byte{[]byte(in), []byte("next\r")}}
	f := newEditorFixture(t, editorOpts{in: r})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "next" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"next\" true nil", line, ok, err)
	}
}

func TestEditorSourceCtrlCInsidePasteIsData(t *testing.T) {
	var calls int
	f := newEditorFixture(t, editorOpts{
		in:          strings.NewReader(pasteOn + "a\x03b" + pasteOff + "\r"),
		onInterrupt: func() { calls++ },
	})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok || line != "a\x03b" {
		t.Fatalf("ReadGoal = %q ok=%v err=%v, want \"a\\x03b\" true nil", line, ok, err)
	}
	if calls != 0 {
		t.Fatalf("OnInterrupt called %d times for an in-paste 0x03, want 0", calls)
	}
}

func TestEditorSourceCtrlCRecreationKeepsConfiguration(t *testing.T) {
	// The replacement Terminal must be rebound with the same configuration as
	// the original: goal history and the 4096-refusal watcher are the two with
	// observable behavior.
	t.Run("history recall", func(t *testing.T) {
		root := t.TempDir()
		xdg := t.TempDir()
		getenv := func(k string) string {
			if k == "XDG_DATA_HOME" {
				return xdg
			}
			return ""
		}
		r := &chunkReader{chunks: [][]byte{[]byte("\x03"), []byte("\x1b[A\r")}}
		f := newEditorFixture(t, editorOpts{in: r, useHistory: true, getenv: getenv, root: root})
		f.src.RecordGoal("remember me")
		line, ok, err := f.readGoal(t)
		if err != nil || !ok || line != "remember me" {
			t.Fatalf("post-Ctrl-C recall = %q ok=%v err=%v, want \"remember me\"", line, ok, err)
		}
	})
	t.Run("4096 watcher", func(t *testing.T) {
		r := &chunkReader{chunks: [][]byte{[]byte("\x03"), []byte(strings.Repeat("a", maxEditorRunes+1) + "\r")}}
		f := newEditorFixture(t, editorOpts{in: r})
		line, ok, err := f.readGoal(t)
		if err != nil || !ok || len(line) != maxEditorRunes {
			t.Fatalf("post-Ctrl-C read ok=%v err=%v len=%d, want a full 4096-rune line", ok, err, len(line))
		}
		if !strings.Contains(f.out.String(), lineLimitWarning) {
			t.Fatalf("the refusal watcher did not survive recreation; output %q", f.out.String())
		}
	})
}

func TestEditorSourceAnswerCtrlCReturnsInterrupted(t *testing.T) {
	// ReadAnswer never continues the approval prompt: one interrupt delivery,
	// then errInterrupted after the raw window closes. The approver maps the
	// sentinel; the editor only reports the event.
	var calls int
	f := newEditorFixture(t, editorOpts{in: strings.NewReader("y\x03"), onInterrupt: func() { calls++ }})
	line, ok, err := f.src.ReadAnswer(context.Background(), "approve? ")
	if !errors.Is(err, errInterrupted) || ok || line != "" {
		t.Fatalf("ReadAnswer = %q ok=%v err=%v, want errInterrupted", line, ok, err)
	}
	if calls != 1 {
		t.Fatalf("OnInterrupt called %d times, want 1", calls)
	}
	makeRaw, restore, _ := f.ops.counts()
	if makeRaw != 1 || restore != 1 {
		t.Fatalf("MakeRaw=%d Restore=%d, want 1/1: the window must close before returning", makeRaw, restore)
	}
	if got := strings.Count(f.out.String(), pasteDisableSeq); got != 1 {
		t.Fatalf("paste disabled %d times, want 1", got)
	}
}

func TestEditorSourceAnswerDrainsAPastedAnswer(t *testing.T) {
	// A multiline paste at an approval prompt: the answer is invalid (deny),
	// the remaining segments are drained inside the same raw window, and the
	// next goal reads clean -- without this, lines 2..n become later goals.
	r := &chunkReader{chunks: [][]byte{
		[]byte(pasteOn + "y\nrm -rf /" + pasteOff + "\r"),
		[]byte("next\r"),
	}}
	f := newEditorFixture(t, editorOpts{in: r})
	line, ok, err := f.src.ReadAnswer(context.Background(), "approve? ")
	if err != nil || !ok || line != "" {
		t.Fatalf("pasted answer = %q ok=%v err=%v, want the invalid-answer denial (\"\" true nil)", line, ok, err)
	}
	makeRaw, restore, _ := f.ops.counts()
	if makeRaw != 1 || restore != 1 {
		t.Fatalf("MakeRaw=%d Restore=%d after the answer, want 1/1: the drain stays inside the same raw window", makeRaw, restore)
	}
	goal, ok, err := f.readGoal(t)
	if err != nil || !ok || goal != "next" {
		t.Fatalf("next ReadGoal = %q ok=%v err=%v, want \"next\": drained paste lines must not leak", goal, ok, err)
	}
}

func TestEditorSourceAnswerOversizedPasteDenies(t *testing.T) {
	// errPasteTooLarge at an approval prompt recreates the Terminal and denies
	// with no approval error; it must not re-prompt for the same approval, and
	// the next goal reads clean.
	big := strings.Repeat("a", maxGoalBytes+2)
	r := &chunkReader{chunks: [][]byte{
		[]byte(pasteOn + big + pasteOff),
		[]byte("next\r"),
	}}
	f := newEditorFixture(t, editorOpts{in: r})
	line, ok, err := f.src.ReadAnswer(context.Background(), "approve? ")
	if err != nil || !ok || line != "" {
		t.Fatalf("oversized pasted answer = %q ok=%v err=%v, want denial with no error", line, ok, err)
	}
	if !strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
	goal, ok, err := f.readGoal(t)
	if err != nil || !ok || goal != "next" {
		t.Fatalf("next ReadGoal = %q ok=%v err=%v, want \"next\"", goal, ok, err)
	}
}

// gatedWriter blocks the first write containing its marker until released,
// signalling entry. Everything else passes straight through to inner.
type gatedWriter struct {
	inner   io.Writer
	marker  string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
	}
	return w.inner.Write(p)
}

// signalGateReader signals the first read request, then blocks every read on
// gate before delegating.
type signalGateReader struct {
	requested chan struct{}
	gate      chan struct{}
	r         io.Reader
	once      sync.Once
}

func (s *signalGateReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.requested) })
	<-s.gate
	return s.r.Read(p)
}

func TestEditorSourceCtrlCRecreationSerializesWithNotices(t *testing.T) {
	// Barrier-forced ordering, not repetition: an in-flight notice holds the
	// binding mutex through its Terminal write, so recreation (replace, CRLF,
	// hint) cannot start until it completes. The notice is parked in Write
	// while the Ctrl-C byte is released, proving the recreation path queues
	// behind the binding rather than racing it.
	entered := make(chan struct{})
	release := make(chan struct{})
	gate := make(chan struct{})
	requested := make(chan struct{})

	var f *editorFixture
	inner := &chunkReader{chunks: [][]byte{[]byte("\x03"), []byte("ok\r")}}
	in := &signalGateReader{requested: requested, gate: gate, r: inner}
	opts := editorOpts{in: in, onInterrupt: func() { f.src.IdleDisplay(ctrlCHint) }}
	f = newEditorFixture(t, opts)
	f.src.rw.w = &gatedWriter{inner: f.src.rw.w, marker: "NOTICE", entered: entered, release: release}

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, _, err := f.readGoal(t)
		done <- result{line, err}
	}()

	<-requested // the reader is at the input gate with a Terminal bound
	go f.src.IdleDisplay("NOTICE")
	<-entered   // the notice holds the binding mutex, parked mid-write
	close(gate) // release the Ctrl-C byte; recreation must queue behind the notice
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case res := <-done:
		if res.err != nil || res.line != "ok" {
			t.Fatalf("ReadGoal = %q err=%v", res.line, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not complete; recreation deadlocked against the notice")
	}
	out := f.out.String()
	notice := strings.Index(out, "NOTICE")
	hint := strings.Index(out, "\r\n"+ctrlCHint)
	if notice == -1 || hint == -1 || notice > hint {
		t.Fatalf("notice at %d, CRLF+hint at %d: the parked notice must land before recreation's output in %q", notice, hint, out)
	}
}

func TestEditorSourceCeilingIgnoresTheDroppedFinalSegment(t *testing.T) {
	// A paste ending in a newline leaves an empty final segment that
	// composition drops, so it contributes no bytes and no separator. Charging
	// it one rejects a goal of exactly maxGoalBytes -- the same size
	// TestEditorSourceAggregateCeilingIsExact pins as acceptable when the last
	// segment is non-empty.
	first := strings.Repeat("a", 500000)
	second := strings.Repeat("b", maxGoalBytes-len(first)-1)
	in := pasteOn + first + "\n" + pasteOff + pasteOn + second + "\n" + pasteOff + "\r"

	f := newEditorFixture(t, editorOpts{in: strings.NewReader(in)})
	line, ok, err := f.readGoal(t)
	if err != nil || !ok {
		t.Fatalf("ReadGoal ok=%v err=%v, want the exactly-maxGoalBytes goal accepted", ok, err)
	}
	if len(line) != maxGoalBytes {
		t.Fatalf("goal len = %d, want exactly %d", len(line), maxGoalBytes)
	}
	if strings.Contains(f.out.String(), goalLimitWarning) {
		t.Fatalf("a legal goal was rejected; output contained %q", goalLimitWarning)
	}
}

func TestEditorSourceOversizedPasteDoesNotRepaintTheRejectedInput(t *testing.T) {
	// The rejected paste is still in x/term's line buffer when the ceiling
	// trips. Warning through that Terminal makes Terminal.Write clear back to
	// the prompt and repaint prompt+line, dumping the whole rejected paste to
	// the screen. The replacement Terminal must be bound first.
	big := strings.Repeat("z", maxGoalBytes+2)
	f := newEditorFixture(t, editorOpts{in: strings.NewReader(pasteOn + big + pasteOff)})
	if _, ok, err := f.readGoal(t); err != nil || ok {
		t.Fatalf("ReadGoal ok=%v err=%v, want the paste rejected", ok, err)
	}
	out := f.out.String()
	if !strings.Contains(out, goalLimitWarning) {
		t.Fatalf("output does not contain %q", goalLimitWarning)
	}
	// x/term echoes the paste as it arrives, so one copy of the forwarded
	// bytes is expected. A second copy means Terminal.Write repainted t.line
	// while rejecting it. Counting bytes rather than runs matters: writeLine
	// wraps at the terminal width, so the echo is never one contiguous run.
	//
	// Measured: 1048576 with the rebind first, 2097152 with the warning first.
	if n := strings.Count(out, "z"); n > maxGoalBytes {
		t.Fatalf("rejected paste occupies %d bytes of output, want at most one %d-byte echo; the discarded line was repainted", n, maxGoalBytes)
	}
}
