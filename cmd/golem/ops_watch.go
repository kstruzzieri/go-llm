package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsview"
)

const (
	altScreenOn  = "\x1b[?1049h\x1b[?25l"
	altScreenOff = "\x1b[?25h\x1b[?1049l"
	clearHome    = "\x1b[H\x1b[2J"
)

// opsFrameEvery is the -watch redraw cadence; ages advance on every frame.
const opsFrameEvery = time.Second

// opsGOOS is runtime.GOOS; tests override it to reach the Windows refusal.
var opsGOOS = runtime.GOOS

// opsWatchUnsupported refuses -watch on Windows before any descriptor is
// probed, with goos passed in so the row is provable on any host. -watch
// writes VT sequences, and golem never enables
// ENABLE_VIRTUAL_TERMINAL_PROCESSING on stdout (see selectsEditor).
func opsWatchUnsupported(goos string) error {
	if goos == "windows" {
		return errors.New("golem ops: -watch is not supported on Windows")
	}
	return nil
}

// opsJob hands the terminal back on Ctrl-Z. stops delivers SIGTSTP; suspend
// stops the process and returns once it is continued. The zero value never
// stops. Ctrl-Z during a collection waits for it to end (up to TickTimeout).
type opsJob struct {
	stops   <-chan os.Signal
	suspend func()
}

// runOpsWatch redraws every opsFrameEvery and collects every opsInterval
// until ctx ends (SIGINT and SIGTERM exit 0). The alternate screen preserves
// prior terminal output; it and the cursor are restored on every return path,
// errors and panics included.
func runOpsWatch(ctx context.Context, src *opsSource, w io.Writer, fd int, ops termOps, getenv func(string) string, job opsJob) error {
	if !ops.IsTerminal(fd) {
		return errors.New("golem ops: -watch needs a terminal on stdout")
	}
	if getenv("TERM") == "dumb" {
		return errors.New("golem ops: -watch needs an ANSI terminal (TERM=dumb)")
	}
	if _, err := io.WriteString(w, altScreenOn); err != nil {
		return err
	}
	defer func() { _, _ = io.WriteString(w, altScreenOff) }()

	render := time.NewTicker(opsFrameEvery)
	defer render.Stop()
	obs := src.tick(ctx)
	// Collection counts frames rather than comparing clock readings to
	// opsInterval: the ticker fires a hair under two seconds after a
	// collection measured once it ended, which pushed every collection to
	// the following frame (a 3 s cadence).
	frames := 0
	for ctx.Err() == nil {
		view := src.view(obs, opsview.ModeWatch) // freshness judged at render time
		// ponytail: a resize shows on the next frame (up to 1 s, or up to 5 s
		// during a collection); redraw on watchResize if that lag shows.
		width, height, err := ops.GetSize(fd)
		if err != nil || width <= 0 || height <= 1 {
			width, height = 80, 24
		}
		var frame bytes.Buffer
		if err := renderOpsTable(&frame, view, src.clock.Mono()-view.GeneratedMono, width); err != nil {
			return err
		}
		if _, err := io.WriteString(w, clearHome+firstLines(frame.String(), height-1)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
		case <-job.stops:
			// Suspend with the shell's screen and cursor back; take the
			// terminal again on resume, and redraw at once.
			_, _ = io.WriteString(w, altScreenOff)
			job.suspend()
			if _, err := io.WriteString(w, altScreenOn); err != nil {
				return err
			}
		case <-render.C:
			if frames++; frames%int(opsInterval/opsFrameEvery) == 0 {
				obs = src.tick(ctx)
			}
		}
	}
	return nil
}

// firstLines keeps at most n lines.
func firstLines(text string, n int) string {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "")
}
