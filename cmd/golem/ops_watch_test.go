package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

type opsTerm struct {
	terminal bool
	w, h     int
}

func (o opsTerm) IsTerminal(int) bool              { return o.terminal }
func (o opsTerm) MakeRaw(int) (*term.State, error) { return nil, nil }
func (o opsTerm) Restore(int, *term.State) error   { return nil }
func (o opsTerm) GetSize(int) (int, int, error)    { return o.w, o.h, nil }

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
	if err := runOpsWatch(ctx, src, w, 1, opsTerm{terminal: true, w: 100, h: 12}, func(string) string { return "xterm-256color" }); err != nil {
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
	if err := runOpsWatch(ctx, src, &out, 1, opsTerm{terminal: false}, func(string) string { return "" }); err == nil || err.Error() != "golem ops: -watch needs a terminal on stdout" {
		t.Fatalf("non-terminal = %v", err)
	}
	if err := runOpsWatch(ctx, src, &out, 1, opsTerm{terminal: true}, func(string) string { return "dumb" }); err == nil || err.Error() != "golem ops: -watch needs an ANSI terminal (TERM=dumb)" {
		t.Fatalf("dumb = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused -watch wrote %q, want nothing", out.String())
	}
}

// frameRecorder keeps every successful write and cancels once n frames (one
// write each) have been written.
type frameRecorder struct {
	mu         sync.Mutex
	writes     []string
	n          int
	cancel     context.CancelFunc
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
		if r.n--; r.n == 0 {
			r.cancel()
		}
	}
	return len(p), nil
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
	xterm := func(string) string { return "xterm-256color" }
	failing := &frameRecorder{failFrames: true}
	if err := runOpsWatch(context.Background(), src, failing, 1, opsTerm{terminal: true, w: 80, h: 24}, xterm); !errors.Is(err, errOpsWrite) {
		t.Fatalf("failed frame write returned %v, want errOpsWrite", err)
	}
	if got := strings.Join(failing.writes, ""); got != altScreenOn+altScreenOff {
		t.Fatalf("after a failed frame write the terminal got %q, want the alternate screen entered and restored", got)
	}
	panicking := &frameRecorder{}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = runOpsWatch(context.Background(), src, panicking, 1, panicTerm{opsTerm{terminal: true}}, xterm)
	}()
	if got := strings.Join(panicking.writes, ""); recovered == nil || got != altScreenOn+altScreenOff {
		t.Fatalf("after a panic (%v) the terminal got %q, want the alternate screen entered and restored", recovered, got)
	}
}

// shrinkingTerm is a window resized to 40x6 after the first frame.
type shrinkingTerm struct {
	opsTerm
	calls int
}

func (s *shrinkingTerm) GetSize(int) (int, int, error) {
	if s.calls++; s.calls == 1 {
		return 100, 12, nil
	}
	return 40, 6, nil
}

// TestOpsWatchCadenceAndResize runs three frames (about two seconds): one a
// second, a collection on every other one, each bounded by the size read
// for it.
func TestOpsWatchCadenceAndResize(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	src, err := newOpsSource(fixtureConfig(t, f.URL()), opsInterval)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &frameRecorder{n: 3, cancel: cancel}
	if err := runOpsWatch(ctx, src, w, 1, &shrinkingTerm{opsTerm: opsTerm{terminal: true}}, func(string) string { return "xterm-256color" }); err != nil {
		t.Fatal(err)
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
	var frames []string
	for _, s := range w.writes {
		if rest, ok := strings.CutPrefix(s, clearHome); ok {
			frames = append(frames, rest)
		}
	}
	size := func(frame string) (cols, lines int) {
		for _, line := range strings.Split(frame, "\n") {
			cols = max(cols, utf8.RuneCountInString(line))
		}
		return cols, strings.Count(frame, "\n")
	}
	if len(frames) != 3 {
		t.Fatalf("wrote %d frames, want 3", len(frames))
	}
	if cols, _ := size(frames[0]); cols <= 40 {
		t.Fatalf("control: first frame is %d columns wide, never past the resized width", cols)
	}
	if cols, lines := size(frames[2]); cols > 40 || lines > 5 {
		t.Fatalf("frame after resize is %d columns x %d lines, want at most 40 x 5", cols, lines)
	}
}
