package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// renderer is an append-only agent.Observer for a terminal. It streams tokens,
// prints a one-line tool-call notice, a dim per-step footer, and (via
// finalFooter) a dim summary after Run. It deliberately omits a tool-RESULT
// line: that requires the agent.ToolResultObserver seam, added in B3.
type renderer struct {
	out      io.Writer
	color    bool
	maxSteps int
	now      func() time.Time
	lastMark time.Time
	runStart time.Time
	lastNL   bool
}

func newRenderer(out io.Writer, color bool, maxSteps int, now func() time.Time) *renderer {
	if now == nil {
		now = time.Now
	}
	start := now()
	return &renderer{out: out, color: color, maxSteps: maxSteps, now: now, lastMark: start, runStart: start, lastNL: true}
}

func (r *renderer) OnToken(_ context.Context, e agent.TokenEvent) error {
	_, err := io.WriteString(r.out, e.Content)
	if err == nil && e.Content != "" {
		r.lastNL = strings.HasSuffix(e.Content, "\n")
	}
	return err
}

func (r *renderer) OnToolCall(_ context.Context, e agent.ToolCallEvent) error {
	// Some tools take no arguments; omit the trailing space when args are empty
	// so the line reads "> name" rather than "> name ".
	if args := string(e.Call.Function.Arguments); args != "" {
		_, err := fmt.Fprintf(r.out, "\n> %s %s\n", e.Call.Function.Name, args)
		if err == nil {
			r.lastNL = true
		}
		return err
	}
	_, err := fmt.Fprintf(r.out, "\n> %s\n", e.Call.Function.Name)
	if err == nil {
		r.lastNL = true
	}
	return err
}

func (r *renderer) OnStep(_ context.Context, e agent.StepEvent) error {
	t := r.now()
	lat := t.Sub(r.lastMark).Seconds()
	r.lastMark = t
	model := "?"
	if e.RouteOutcome != nil {
		model = e.RouteOutcome.ActualModel.String()
	}
	line := fmt.Sprintf("%s · %.1fs · ctx %.0f%% · step %d/%d",
		model, lat, e.Pressure.UsedPct*100, e.Index+1, r.maxSteps)
	return r.writeDim(line)
}

func (r *renderer) finalFooter(res agent.Result) {
	total := r.now().Sub(r.runStart).Seconds()
	line := fmt.Sprintf("done · %d steps · %.1fs · %d tok",
		len(res.Steps), total, res.Usage.TotalTokens)
	if res.StopReason != agent.Completed {
		// Double space (not " · ") deliberately sets the stop reason apart from
		// the metric fields above it — it is a status suffix, not another metric.
		line += "  stopped: " + res.StopReason.String()
	}
	_ = r.writeDim(line)
}

func (r *renderer) writeDim(line string) error {
	if !r.lastNL {
		if _, err := io.WriteString(r.out, "\n"); err != nil {
			return err
		}
	}
	if r.color {
		line = "\x1b[2m" + line + "\x1b[0m"
	}
	_, err := io.WriteString(r.out, line+"\n")
	if err == nil {
		r.lastNL = true
	}
	return err
}
