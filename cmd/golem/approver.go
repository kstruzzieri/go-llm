package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// replApprover renders a mutation's diff preview and prompts the user for a single
// [y/N] decision over the shared lineSource. It is wired into agent.Request.Approver
// only when a configured tool needs approval; otherwise the runtime's nil-approver
// fail-safe denies every mutating call.
type replApprover struct {
	src   lineSource
	out   io.Writer
	color bool
}

// Compile-time assertion: replApprover must satisfy agent.Approver.
var _ agent.Approver = (*replApprover)(nil)

func newReplApprover(src lineSource, out io.Writer, color bool) *replApprover {
	return &replApprover{src: src, out: out, color: color}
}

// Approve shows the preview and reads one line. "y"/"yes" (case-insensitive) approves.
// "n", empty, and EOF deny with a nil error. A canceled context (Ctrl-C) denies and
// returns the context error so the run aborts.
// The prompt and rendering are action-neutral: run_command and MCP calls get a
// plain preview and run prompt; all other calls get the diff rendering and "Apply
// this change?" prompt.
func (a *replApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	isExec := call.Function.Name == "run_command"
	isMCP := strings.HasPrefix(call.Function.Name, "mcp__")
	isPlan := call.Function.Name == submitPlanToolName
	// The preview still renders here; only the question moves, because the
	// source owns prompt printing and the editor repaints its prompt on every
	// asynchronous write.
	var question string
	switch {
	case isPlan:
		a.renderPlain(preview)
		question = "Lock this plan? [y/N] "
	case isExec:
		a.renderPlain(preview)
		question = "Run this command? [y/N] "
	case isMCP:
		a.renderPlain(preview)
		question = "Run this MCP tool? [y/N] "
	default:
		a.renderDiff(preview)
		question = "Apply this change? [y/N] "
	}
	line, ok, err := a.src.ReadAnswer(ctx, question)
	if errors.Is(err, errInterrupted) {
		// Normalized here, at the approval boundary, so runOnce and the
		// Agentflow author classify one shared error: an interrupted approval
		// IS a cancellation, and the editor-local sentinel must not leak into
		// either caller's error taxonomy.
		return false, context.Canceled
	}
	if err != nil {
		return false, err // ctx canceled: abort the run
	}
	if !ok {
		_, _ = fmt.Fprintln(a.out)
		return false, nil // EOF: deny
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// renderPlain prints a non-diff preview verbatim (no +/- coloring).
func (a *replApprover) renderPlain(preview string) {
	_, _ = fmt.Fprintln(a.out, strings.TrimRight(preview, "\n"))
}

func (a *replApprover) renderDiff(preview string) {
	if !a.color {
		_, _ = fmt.Fprintln(a.out, preview)
		return
	}
	for _, ln := range strings.Split(strings.TrimRight(preview, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+"):
			_, _ = fmt.Fprintf(a.out, "\x1b[32m%s\x1b[0m\n", ln) // green add
		case strings.HasPrefix(ln, "-"):
			_, _ = fmt.Fprintf(a.out, "\x1b[31m%s\x1b[0m\n", ln) // red remove
		default:
			_, _ = fmt.Fprintln(a.out, ln)
		}
	}
}
