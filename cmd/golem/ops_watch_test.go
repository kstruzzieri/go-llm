package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

// opsTerm fakes a terminal on stdout, fd 1, the descriptor every test passes.
// Any other descriptor is not a terminal and has no size, so a frame that
// sizes the wrong one falls back to 80x24.
type opsTerm struct {
	terminal bool
	w, h     int
}

var errNotStdout = errors.New("not the stdout descriptor")

func (o opsTerm) IsTerminal(fd int) bool           { return o.terminal && fd == 1 }
func (o opsTerm) MakeRaw(int) (*term.State, error) { return nil, nil }
func (o opsTerm) Restore(int, *term.State) error   { return nil }
func (o opsTerm) GetSize(fd int) (int, int, error) {
	if fd != 1 {
		return 0, 0, errNotStdout
	}
	return o.w, o.h, nil
}

func xterm(string) string { return "xterm-256color" }

// cancelAfterFrame cancels once a frame with the model table has been
// written (the frame is clipped to the terminal height, so the trigger must
// sit near the top).
type cancelAfterFrame struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (c *cancelAfterFrame) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.buf.Write(p)
	if strings.Contains(c.buf.String(), "MODELS") {
		c.cancel()
	}
	return n, err
}

func TestOpsWatchFrameLifecycle(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelAfterFrame{cancel: cancel}
	if err := runOpsWatch(ctx, src, w, 1, opsTerm{terminal: true, w: 100, h: 12}, xterm, opsJob{}); err != nil {
		t.Fatal(err)
	}
	got := w.buf.String()
	if !strings.HasPrefix(got, "\x1b[?1049h\x1b[?25l") || !strings.HasSuffix(got, "\x1b[?25h\x1b[?1049l") {
		t.Fatalf("alternate screen not entered/restored: %q", got)
	}
	frame := got[strings.Index(got, "\x1b[H\x1b[2J"):]
	if lines := strings.Count(frame, "\n"); lines > 11 {
		t.Fatalf("frame has %d lines, want at most height-1 = 11", lines)
	}
	if !strings.Contains(got, "llamacpp/gemma4:31b") {
		t.Fatalf("frame missing model row: %q", got)
	}
	// Watch mode with memory: once mode would show neither.
	if !strings.Contains(got, "mode watch") || !strings.Contains(got, "loaded: first observed") {
		t.Fatalf("frame is not a watch-mode view with residency memory: %q", got)
	}
}

// watchFrame runs -watch against a healthy fixture for one frame at width x
// height and returns the frame.
func watchFrame(t *testing.T, width, height int) string {
	t.Helper()
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &frameRecorder{n: 1, cancel: cancel}
	if err := runOpsWatch(ctx, src, w, 1, opsTerm{terminal: true, w: width, h: height}, xterm, opsJob{}); err != nil {
		t.Fatal(err)
	}
	return w.frame(0)
}

// TestOpsWatchClipCutsActivityLast pins the column order: activity, the
// longest cell and always unknown in phases 0-1, is last, so a terminal clip
// cuts it before observed loads (spec N-5) or the 1h summary. The 1h column
// starts near column 116 with the fixture's residency and loads cells, so it
// is checked at 140.
func TestOpsWatchClipCutsActivityLast(t *testing.T) {
	if got := watchFrame(t, 100, 40); !strings.Contains(got, "observed loads") {
		t.Fatalf("observed loads clipped at 100 columns:\n%s", got)
	}
	got := watchFrame(t, 140, 40)
	if !strings.Contains(got, "observed loads") || !strings.Contains(got, "3 calls, 1 errors") {
		t.Fatalf("observed loads or the 1h summary clipped at 140 columns:\n%s", got)
	}
	if strings.Contains(got, "publishes no reliable in-flight snapshot; last completed") {
		t.Fatalf("control: the activity cell fit at 140 columns, so nothing was clipped:\n%s", got)
	}
}

// TestOpsWatchRefusesDumbAndNonTerminal runs over a real source and an ended
// context, so a dropped check reaches the loop and returns nil instead of
// panicking on an empty source.
func TestOpsWatchRefusesDumbAndNonTerminal(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := runOpsWatch(ctx, src, &out, 1, opsTerm{terminal: false}, func(string) string { return "" }, opsJob{}); err == nil || err.Error() != "golem ops: -watch needs a terminal on stdout" {
		t.Fatalf("non-terminal = %v", err)
	}
	if err := runOpsWatch(ctx, src, &out, 1, opsTerm{terminal: true}, func(string) string { return "dumb" }, opsJob{}); err == nil || err.Error() != "golem ops: -watch needs an ANSI terminal (TERM=dumb)" {
		t.Fatalf("dumb = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused -watch wrote %q, want nothing", out.String())
	}
}

func TestOpsWatchRefusesWindows(t *testing.T) {
	if err := opsWatchUnsupported("windows"); err == nil || err.Error() != "golem ops: -watch is not supported on Windows" {
		t.Fatalf("windows = %v", err)
	}
	for _, goos := range []string{"darwin", "linux"} {
		if err := opsWatchUnsupported(goos); err != nil {
			t.Fatalf("%s = %v, want nil", goos, err)
		}
	}
	// and runOps asks it before anything else
	pinNoDiscovery(t)
	saved := opsGOOS
	opsGOOS = "windows"
	t.Cleanup(func() { opsGOOS = saved })
	var out, errOut bytes.Buffer
	err := runOps(context.Background(), []string{"-watch"}, strings.NewReader(""), &out, &errOut)
	if err == nil || err.Error() != "golem ops: -watch is not supported on Windows" || exitCodeFor(err) == 0 {
		t.Fatalf("runOps -watch on windows = %v (exit %d), want the Windows refusal and a nonzero exit", err, exitCodeFor(err))
	}
}

// frameRecorder keeps every successful write, calls afterFrame with each
// frame's number, and cancels once n frames (one write each) are written.
type frameRecorder struct {
	mu         sync.Mutex
	writes     []string
	frames     int
	n          int
	cancel     context.CancelFunc
	afterFrame func(int)
	failFrames bool // frame writes fail with errOpsWrite
}

func (r *frameRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	frame := strings.HasPrefix(string(p), clearHome)
	if frame && r.failFrames {
		return 0, errOpsWrite
	}
	r.writes = append(r.writes, string(p))
	if frame {
		r.frames++
		if r.afterFrame != nil {
			r.afterFrame(r.frames)
		}
		if r.frames == r.n {
			r.cancel()
		}
	}
	return len(p), nil
}

// frame returns frame i (from 0) without its clear-screen prefix.
func (r *frameRecorder) frame(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.writes {
		if rest, ok := strings.CutPrefix(s, clearHome); ok {
			if i == 0 {
				return rest
			}
			i--
		}
	}
	return ""
}

// kinds names each write: on, off, frame, or the raw text.
func (r *frameRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.writes {
		switch {
		case s == altScreenOn:
			s = "on"
		case s == altScreenOff:
			s = "off"
		case strings.HasPrefix(s, clearHome):
			s = "frame"
		}
		out = append(out, s)
	}
	return out
}

// panicTerm panics where a frame reads the size, standing in for any panic
// inside a frame.
type panicTerm struct{ opsTerm }

func (panicTerm) GetSize(int) (int, int, error) { panic("size probe") }

// TestOpsWatchRestoresOnErrorAndPanic pins the restore on the paths a
// restore written beside the clean return would miss.
func TestOpsWatchRestoresOnErrorAndPanic(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	failing := &frameRecorder{failFrames: true}
	if err := runOpsWatch(context.Background(), src, failing, 1, opsTerm{terminal: true, w: 80, h: 24}, xterm, opsJob{}); !errors.Is(err, errOpsWrite) {
		t.Fatalf("failed frame write returned %v, want errOpsWrite", err)
	}
	if got := strings.Join(failing.writes, ""); got != altScreenOn+altScreenOff {
		t.Fatalf("after a failed frame write the terminal got %q, want the alternate screen entered and restored", got)
	}
	panicking := &frameRecorder{}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = runOpsWatch(context.Background(), src, panicking, 1, panicTerm{opsTerm{terminal: true}}, xterm, opsJob{})
	}()
	if got := strings.Join(panicking.writes, ""); recovered == nil || got != altScreenOn+altScreenOff {
		t.Fatalf("after a panic (%v) the terminal got %q, want the alternate screen entered and restored", recovered, got)
	}
}

// altWriteFailer lands write number failAt (from 1) only up to accept bytes
// and fails it; every other write lands whole. writes keeps what landed,
// with each frame read as "frame".
type altWriteFailer struct {
	failAt, accept int
	calls          int
	writes         []string
}

func (f *altWriteFailer) Write(p []byte) (int, error) {
	f.calls++
	n, err := len(p), error(nil)
	if f.calls == f.failAt {
		n, err = min(f.accept, len(p)), errOpsWrite
	}
	if n > 0 {
		got := string(p[:n])
		if strings.HasPrefix(got, clearHome) {
			got = "frame"
		}
		f.writes = append(f.writes, got)
	}
	return n, err
}

// TestOpsWatchFailedEntryRestoresOnlyWhatReachedTheTerminal pins both
// writes of altScreenOn, at start and on resume from Ctrl-Z: a short write
// that switched screens is restored, and a write that landed nothing gets
// no restore, whose ?1049l would home the cursor on the normal screen
// (DECRC with nothing saved).
func TestOpsWatchFailedEntryRestoresOnlyWhatReachedTheTerminal(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	enter := len("\x1b[?1049h") // the screen switch lands; the cursor hide does not
	// With Ctrl-Z pending, the writes run on, frame, off (suspend), on.
	const start, resume = 1, 4
	for _, tc := range []struct {
		name           string
		failAt, accept int
		want           []string
	}{
		{"short write at start", start, enter, []string{altScreenOn[:enter], altScreenOff}},
		{"nothing written at start", start, 0, nil},
		{"short write on resume", resume, enter, []string{altScreenOn, "frame", altScreenOff, altScreenOn[:enter], altScreenOff}},
		{"nothing written on resume", resume, 0, []string{altScreenOn, "frame", altScreenOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &altWriteFailer{failAt: tc.failAt, accept: tc.accept}
			stops := make(chan os.Signal, 1)
			stops <- nil
			job := opsJob{stops: stops, suspend: func() {}}
			if err := runOpsWatch(context.Background(), src, w, 1, opsTerm{terminal: true, w: 80, h: 24}, xterm, job); !errors.Is(err, errOpsWrite) {
				t.Fatalf("failed alternate-screen write returned %v, want errOpsWrite", err)
			}
			if !slices.Equal(w.writes, tc.want) {
				t.Fatalf("the terminal got %q, want %q", w.writes, tc.want)
			}
		})
	}
}

// TestOpsWatchSuspendHandsBackTheTerminal drives Ctrl-Z through an injected
// job, so the test binary is never stopped: the screen and cursor are
// restored before the process stops, and on resume the alternate screen is
// entered again and redrawn without waiting for the next frame tick.
func TestOpsWatchSuspendHandsBackTheTerminal(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &frameRecorder{n: 2, cancel: cancel}
	stops := make(chan os.Signal, 1)
	stops <- nil // Ctrl-Z, pending before the first frame tick
	var atSuspend []string
	var resumed time.Time
	job := opsJob{stops: stops, suspend: func() {
		atSuspend, resumed = w.kinds(), time.Now()
	}}
	if err := runOpsWatch(ctx, src, w, 1, opsTerm{terminal: true, w: 100, h: 40}, xterm, job); err != nil {
		t.Fatal(err)
	}
	if want := []string{"on", "frame", "off"}; !slices.Equal(atSuspend, want) {
		t.Fatalf("writes before suspending = %q, want %q (screen and cursor restored)", atSuspend, want)
	}
	if got, want := w.kinds(), []string{"on", "frame", "off", "on", "frame", "off"}; !slices.Equal(got, want) {
		t.Fatalf("writes = %q, want %q (re-entered and redrawn on resume)", got, want)
	}
	if d := time.Since(resumed); d >= opsFrameEvery/2 {
		t.Fatalf("redraw came %v after resume, want it at once, not on the next frame tick", d)
	}
}

// shrinkingTerm is a window resized to 40x6 after the second frame.
type shrinkingTerm struct {
	opsTerm
	calls int
}

func (s *shrinkingTerm) GetSize(fd int) (int, int, error) {
	if fd != 1 {
		return 0, 0, errNotStdout
	}
	if s.calls++; s.calls <= 2 {
		return 100, 12, nil
	}
	return 40, 6, nil
}

// TestOpsWatchCadenceAndResize runs three frames (about two seconds): one a
// second, a collection on every other one, each built at its own instant and
// bounded by the size read for it. After the first frame the view's wall
// clock jumps an hour, so the second frame, which collects nothing, must drop
// the readings a frame built only at collection would still show.
func TestOpsWatchCadenceAndResize(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	wall, jump := src.clock.Wall, time.Duration(0) // the collector keeps the unjumped clock
	src.clock.Wall = func() time.Time { return wall().Add(jump) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &frameRecorder{n: 3, cancel: cancel, afterFrame: func(n int) {
		if n == 1 {
			jump = time.Hour
		}
	}}
	start := time.Now()
	if err := runOpsWatch(ctx, src, w, 1, &shrinkingTerm{opsTerm: opsTerm{terminal: true}}, xterm, opsJob{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 3500*time.Millisecond {
		t.Fatalf("three frames took %v, want them a second apart", d)
	}
	var running int
	for _, r := range f.Requests() {
		if r.URI == "/running" {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("/running read %d times over three frames, want 2 (collect every %v, redraw every second)", running, opsInterval)
	}
	if !strings.Contains(w.frame(0), "loaded: first observed") || !strings.Contains(w.frame(1), "llamacpp/gemma4:31b") ||
		strings.Contains(w.frame(1), "loaded: first observed") {
		t.Fatalf("after a wall-clock jump the second frame still shows the first frame's readings:\n%s\n---\n%s", w.frame(0), w.frame(1))
	}
	size := func(frame string) (cols, lines int) {
		for _, line := range strings.Split(frame, "\n") {
			cols = max(cols, utf8.RuneCountInString(line))
		}
		return cols, strings.Count(frame, "\n")
	}
	if w.frame(2) == "" {
		t.Fatalf("wrote %d frames, want 3", w.frames)
	}
	if cols, _ := size(w.frame(1)); cols <= 40 {
		t.Fatalf("control: second frame is %d columns wide, never past the resized width", cols)
	}
	if cols, lines := size(w.frame(2)); cols > 40 || lines > 5 {
		t.Fatalf("frame after resize is %d columns x %d lines, want at most 40 x 5", cols, lines)
	}
}
