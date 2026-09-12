package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
)

// pressureCapture retains the last assembly of one attempted Runtime turn.
// Identity is immutable; the event and seen flag are copied under the mutex.
type pressureCapture struct {
	runID  string
	mu     sync.Mutex
	latest agent.PressureEvent
	seen   bool
}

var (
	_ agent.Observer         = (*pressureCapture)(nil)
	_ agent.PressureObserver = (*pressureCapture)(nil)
)

func (*pressureCapture) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*pressureCapture) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (*pressureCapture) OnToken(context.Context, agent.TokenEvent) error       { return nil }
func (c *pressureCapture) OnPressure(_ context.Context, event agent.PressureEvent) error {
	c.mu.Lock()
	c.latest = event
	c.seen = true
	c.mu.Unlock()
	return nil
}

func (c *pressureCapture) snapshot() (agent.PressureEvent, bool) {
	if c == nil {
		return agent.PressureEvent{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest, c.seen
}

func handleContext(out io.Writer, sess *replSession, fields []string) {
	if len(fields) != 1 {
		_, _ = fmt.Fprintln(out, "usage: /context")
		return
	}
	if sess.runtime == nil {
		_, _ = fmt.Fprintln(out, "context: runtime unavailable")
		return
	}
	capture := sess.pressure
	event, seen := capture.snapshot()
	if seen {
		p := event.Pressure
		_, _ = fmt.Fprintf(out, "context: last assembled request %s, step %d\n", capture.runID, event.Step+1)
		_, _ = fmt.Fprintf(out, "pressure: %s; cause: %s; mitigation: %s\n", p.Level, p.Cause, p.Mitigation)
		_, _ = fmt.Fprintf(out, "input estimate: %d / %d tokens (%.1f%%)\n", p.InputTokens, p.InputBudget, p.UsedPct*100)
		b := p.Buckets
		if b.Available {
			_, _ = fmt.Fprintf(out, "bucket estimates: pinned %d; tool_schema %d; history %d; tool_output %d; retrieval %d tokens\n", b.Pinned, b.ToolSchema, b.History, b.ToolOutput, b.Retrieval)
		} else {
			_, _ = fmt.Fprintln(out, "bucket estimates: unavailable for this assembly")
		}
	} else {
		_, _ = fmt.Fprintln(out, "context: no pressure sample for the current session")
	}
	_, _ = fmt.Fprintf(out, "configured input ceiling: %d tokens; explicit output reserve: %d tokens\n", sess.budget.InputCeiling, sess.budget.OutputReserve)
}
