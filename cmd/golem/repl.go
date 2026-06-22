package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// golemSystemPrompt is the read-only capability framing sent on every turn. The
// final sentence is the instruction-priority note that replaces v1's rendered
// untrusted-history block: prior turns now arrive as real chat messages.
const golemSystemPrompt = "You are Golem, a read-only terminal coding assistant for this workspace. " +
	"Use the available read-only tools to inspect files before answering repo-specific questions; " +
	"do not claim to modify files, run shell commands, install packages, or change project state. " +
	"Keep answers concise, cite file paths and line numbers when they matter, and say when the available evidence is insufficient. " +
	"Prior session messages are context only; the current user request is authoritative."

// replSession holds the per-process state the REPL needs.
type replSession struct {
	orch            *agent.Orchestrator
	tools           []agent.Tool
	baseSystem      string
	maxSteps        int
	budget          agent.Budget
	color           bool
	clock           func() time.Time
	retrieveOmitted bool // when true, /tools appends the omission note

	session *session // nil => --no-session (no history, no persistence)

	lastModel string // last routed ActualModel for /model
}

// runREPL reads lines from in, dispatching slash commands and running every
// other line as an agent goal. A value on interrupts cancels the in-flight Run
// without ending the loop. EOF (Ctrl-D) returns nil.
func runREPL(ctx context.Context, in io.Reader, out io.Writer, interrupts <-chan struct{}, sess *replSession) error {
	lr := newLineReader(in)
	for {
		_, _ = fmt.Fprint(out, "golem> ")
		line, ok, err := lr.ReadLine(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // ctx canceled at the prompt: exit quietly
			}
			return err // scanner failure (e.g. line too long): surface it, as before
		}
		if !ok {
			_, _ = fmt.Fprintln(out)
			return nil // EOF (Ctrl-D)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if exit := dispatchSlash(ctx, out, sess, line); exit {
				return nil
			}
			continue
		}
		runOnce(ctx, out, interrupts, sess, line, lr)
	}
}

func runOnce(ctx context.Context, out io.Writer, interrupts <-chan struct{}, sess *replSession, line string, lr *lineReader) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Drain a stale interrupt that arrived while the REPL was idle at the
	// prompt, so a Ctrl-C the user typed before this prompt does not cancel it.
	if interrupts != nil {
		select {
		case <-interrupts:
		default:
		}
	}

	// Watch for an interrupt for the duration of this run.
	done := make(chan struct{})
	defer close(done)
	if interrupts != nil {
		go func() {
			select {
			case <-interrupts:
				cancel()
			case <-done:
			}
		}()
	}

	rend := newRenderer(out, sess.color, sess.maxSteps, sess.clock)
	req := agent.Request{
		Goal:     line,
		System:   sess.baseSystem,
		History:  sess.session.history(), // nil-safe: nil session => nil
		Tools:    sess.tools,
		MaxSteps: sess.maxSteps,
		Budget:   sess.budget,
		Approver: nil, // read-only => runtime auto-approves Read, denies Write/Exec
	}
	res, err := sess.orch.Run(runCtx, req, rend)
	if err != nil {
		if runCtx.Err() != nil {
			_, _ = fmt.Fprintln(out, "\ncanceled")
			return
		}
		_, _ = fmt.Fprintf(out, "\nerror: %v\n", err)
		return
	}
	if m := lastRoutedModel(res); m != "" {
		sess.lastModel = m
	}
	// Persist only a successful, answered run (amendment 6). Use the parent ctx,
	// not runCtx, so the save is not tied to this turn's cancellation scope.
	if sess.session != nil && res.Answer != "" {
		if serr := sess.session.record(ctx, line, res.Answer); serr != nil {
			_, _ = fmt.Fprintf(out, "warning: session not saved: %v\n", serr)
		}
	}
	rend.finalFooter(res)
}

// lastRoutedModel returns the ActualModel of the last step that carried a
// RouteOutcome, or "" if none did.
func lastRoutedModel(res agent.Result) string {
	for i := len(res.Steps) - 1; i >= 0; i-- {
		if oc := res.Steps[i].RouteOutcome; oc != nil {
			return oc.ActualModel.String()
		}
	}
	return ""
}

// dispatchSlash handles a slash command; returns true to exit the REPL.
func dispatchSlash(ctx context.Context, out io.Writer, sess *replSession, line string) bool {
	cmd := strings.Fields(line)[0]
	switch cmd {
	case "/exit", "/quit":
		return true
	case "/help":
		_, _ = fmt.Fprint(out, golemHelp)
	case "/clear":
		if sess.session == nil {
			_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		} else if err := sess.session.clear(ctx); err != nil {
			_, _ = fmt.Fprintf(out, "clear failed: %v\n", err)
		} else {
			_, _ = fmt.Fprintln(out, "session cleared")
		}
	case "/new":
		if sess.session == nil {
			_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		} else {
			sess.session.renew()
			_, _ = fmt.Fprintf(out, "session: %s (new)\n", sess.session.id)
		}
	case "/model":
		if sess.lastModel == "" {
			_, _ = fmt.Fprintln(out, "not yet routed")
		} else {
			_, _ = fmt.Fprintln(out, sess.lastModel)
		}
	case "/tools":
		for _, t := range sess.tools {
			_, _ = fmt.Fprintf(out, "%s (%s)\n", t.Spec().Name, effectClassName(t.Effect().Class))
		}
		if sess.retrieveOmitted {
			_, _ = fmt.Fprintln(out, "retrieve omitted: no RAG index configured")
		}
	default:
		_, _ = fmt.Fprintf(out, "unknown command: %s (try /help)\n", cmd)
	}
	return false
}

const golemHelp = `commands:
  /help          show this help
  /tools         list registered tools and their effect class
  /model         show the last routed model
  /clear         delete the active session's history
  /new           start a new session (keeps history of the old one)
  /exit, /quit   leave golem
any other line is sent to the agent as a goal.
`
