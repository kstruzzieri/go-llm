package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// replApprover renders a mutation's diff preview and prompts the user for a single
// [y/N] decision over the shared lineReader. It is wired into agent.Request.Approver
// only when -allow-write is set; otherwise the runtime's nil-approver fail-safe
// denies every mutating call.
type replApprover struct {
	lr    *lineReader
	out   io.Writer
	color bool
}

// Compile-time assertion: replApprover must satisfy agent.Approver.
var _ agent.Approver = (*replApprover)(nil)

func newReplApprover(lr *lineReader, out io.Writer, color bool) *replApprover {
	return &replApprover{lr: lr, out: out, color: color}
}

// Approve prints the diff and reads one line. "y"/"yes" (case-insensitive) approves.
// "n", empty, and EOF deny with a nil error. A canceled context (Ctrl-C) denies and
// returns the context error so the run aborts.
func (a *replApprover) Approve(ctx context.Context, _ provider.ToolCall, preview string) (bool, error) {
	a.renderDiff(preview)
	_, _ = fmt.Fprint(a.out, "Apply this change? [y/N] ")
	line, ok, err := a.lr.ReadLine(ctx)
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
