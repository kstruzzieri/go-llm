package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/memory"
)

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

	compress compressPolicy // post-turn history compression policy

	memory       *memory.SQLiteStore // nil => memory disabled (-no-memory or open failed)
	memoryDBPath string              // used to re-secure SQLite sidecars after writes
	workspaceID  string              // stable id used to scope memory create/list/search

	lastModel   string           // last routed ActualModel for /model
	journal     *mutationJournal // nil unless -allow-write enabled writes
	allowWrite  bool
	allowExec   bool
	mcpAttached bool // true when external MCP tools are attached (force approver)
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

	var approver agent.Approver
	if needsApprover(sess.allowWrite, sess.allowExec, sess.mcpAttached) {
		approver = newReplApprover(lr, out, sess.color)
	}

	rend := newRenderer(out, sess.color, sess.maxSteps, sess.clock)
	req := agent.Request{
		Goal:           line,
		System:         sess.baseSystem,
		HistorySummary: sess.session.historySummary(), // nil-safe: nil session => empty
		History:        sess.session.history(),        // nil-safe: nil session => nil
		Tools:          sess.tools,
		MaxSteps:       sess.maxSteps,
		Budget:         sess.budget,
		Approver:       approver, // nil when read-only => runtime fail-safe denies Write/Exec
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
	// Persist only a successful, answered run (amendment 6). recordResult uses the
	// parent ctx so saving the computed answer is not tied to this turn's
	// cancellation scope. maybeCompress, by contrast, makes a new summarizer model
	// call — it uses runCtx so a Ctrl-C during post-turn compression interrupts it
	// (CompressMessages then returns a cancellation error and the session is left
	// untouched, which the warning below surfaces).
	if sess.session != nil && res.Answer != "" {
		if serr := sess.session.recordResult(ctx, line, res); serr != nil {
			_, _ = fmt.Fprintf(out, "warning: session not saved: %v\n", serr)
		}
		if cerr := sess.maybeCompress(runCtx); cerr != nil {
			_, _ = fmt.Fprintf(out, "warning: compression skipped: %v\n", cerr)
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
	fields := strings.Fields(line)
	cmd := fields[0]
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
	case "/sessions":
		if sess.session == nil {
			_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		} else if err := printSessions(ctx, out, sess.session); err != nil {
			_, _ = fmt.Fprintf(out, "sessions failed: %v\n", err)
		}
	case "/search-sessions":
		if sess.session == nil {
			_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		} else if q := strings.TrimSpace(strings.TrimPrefix(line, cmd)); q == "" {
			_, _ = fmt.Fprintln(out, "usage: /search-sessions <query>")
		} else if err := printSessionSearch(ctx, out, sess.session, q); err != nil {
			_, _ = fmt.Fprintf(out, "session search failed: %v\n", err)
		}
	case "/resume":
		if sess.session == nil {
			_, _ = fmt.Fprintln(out, "session disabled (--no-session)")
		} else if len(fields) != 2 {
			_, _ = fmt.Fprintln(out, "usage: /resume <session-id>")
		} else if id, err := resolveSessionID(sessionIDOpts{explicit: fields[1]}); err != nil {
			_, _ = fmt.Fprintln(out, err)
		} else if info, err := sess.session.switchTo(ctx, id); err != nil {
			_, _ = fmt.Fprintf(out, "resume failed: %v\n", err)
		} else {
			_, _ = fmt.Fprintln(out, info.line())
		}
	case "/model":
		if sess.lastModel == "" {
			_, _ = fmt.Fprintln(out, "not yet routed")
		} else {
			_, _ = fmt.Fprintln(out, sess.lastModel)
		}
	case "/undo":
		if sess.journal == nil {
			_, _ = fmt.Fprintln(out, "writes disabled (run with -allow-write)")
		} else {
			sess.journal.undo(out)
		}
	case "/remember":
		handleRemember(ctx, out, sess, line)
	case "/forget":
		handleForget(ctx, out, sess, fields)
	case "/memories":
		handleMemories(ctx, out, sess, fields)
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
  /sessions      list saved sessions
  /search-sessions <query>
                 search saved sessions
  /resume <id>   switch to a saved session
  /undo          revert the last applied write (when -allow-write)
  /remember [--global] <text>
                 save a memory (workspace scope unless --global)
  /forget <id>   delete a saved memory
  /memories [--promote <id> | --localize <id>]
                 list saved memories, or change a memory's scope
  /exit, /quit   leave golem
any other line is sent to the agent as a goal.
`

func printSessions(ctx context.Context, out io.Writer, s *session) error {
	summaries, err := s.store.List(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "sessions:")
	if len(summaries) == 0 {
		_, _ = fmt.Fprintln(out, "  (none)")
		return nil
	}
	for _, sum := range summaries {
		title := strings.TrimSpace(sum.Title)
		if title == "" {
			title = "(untitled)"
		}
		_, _ = fmt.Fprintf(out, "  %s  %s  updated %s  %s\n",
			sum.ID,
			plural(sum.MessageCount, "message", "messages"),
			sum.UpdatedAt.Format("2006-01-02 15:04"),
			title,
		)
	}
	return nil
}

func printSessionSearch(ctx context.Context, out io.Writer, s *session, query string) error {
	results, err := s.store.Search(ctx, query, conversation.SearchOptions{Limit: 10})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "session search:")
	if len(results) == 0 {
		_, _ = fmt.Fprintln(out, "  (no matches)")
		return nil
	}
	for _, res := range results {
		title := strings.TrimSpace(res.Title)
		if title == "" {
			title = "(untitled)"
		}
		snippet := strings.TrimSpace(strings.ReplaceAll(res.Snippet, "\n", " "))
		if snippet == "" {
			snippet = title
		}
		_, _ = fmt.Fprintf(out, "  %s  updated %s  %s\n    %s\n",
			res.ID,
			res.UpdatedAt.Format("2006-01-02 15:04"),
			title,
			snippet,
		)
	}
	return nil
}
