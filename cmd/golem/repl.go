package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
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

	records *memory.MemoryRecordStore // nil => agent memory disabled (-agent-memory absent or open failed)

	lastModel    string           // last routed ActualModel for /model
	journal      *mutationJournal // nil unless -allow-write enabled writes
	allowWrite   bool
	allowExec    bool
	mcpAttached  bool    // true when external MCP tools are attached (force approver)
	obs          *observ // nil unless -trace/-telemetry enabled
	pressureWarn bool    // enable the one-per-run context-pressure warning line

	modelOptions provider.ModelOptions // per-run model options (-think)
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
		_, _ = runOnce(ctx, out, interrupts, sess, line, lr)
	}
}

// errOneShotFailed signals a one-shot turn that already reported its failure
// on stderr; main exits non-zero without printing a second message.
var errOneShotFailed = errors.New("one-shot run failed")

// runOneShot executes exactly one agent turn for -p. Only the final answer is
// written to stdout (with a single trailing newline); every other line the
// turn produces — tool progress, warnings, errors — goes to stderr via
// runOnce. A nil lineReader means no interactive approver exists, so the
// runtime fail-safe denies any approval-gated tool call.
func runOneShot(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, prompt string) error {
	res, runErr := runOnce(ctx, stderr, interrupts, sess, prompt, nil)
	if runErr != nil {
		return errOneShotFailed // runOnce already reported the failure on stderr
	}
	if strings.TrimSpace(res.Answer) == "" {
		return errors.New("one-shot: model produced no final answer")
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimRight(res.Answer, "\n"))
	return nil
}

// runOnce runs a single agent turn, rendering all progress and errors to out.
// It returns the run result so runOneShot can extract the final answer; the
// REPL ignores it.
func runOnce(ctx context.Context, out io.Writer, interrupts <-chan struct{}, sess *replSession, line string, lr *lineReader) (agent.Result, error) {
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
	if lr != nil && needsApprover(sess.allowWrite, sess.allowExec, sess.mcpAttached) {
		approver = newReplApprover(lr, out, sess.color)
	}

	rend := newRenderer(out, sess.color, sess.maxSteps, sess.clock)
	rend.warnPressure = sess.pressureWarn

	var (
		runID     string
		startedAt time.Time
		sink      *agenttrace.TelemetrySink
	)
	observer := agent.Observer(rend)
	if sess.obs != nil {
		runID = sess.obs.nextRunID()
		startedAt = sess.obs.clock()
		var serr error
		if sink, serr = sess.obs.startSink(runID, startedAt); serr != nil {
			_, _ = fmt.Fprintf(out, "warning: telemetry disabled: %v\n", serr)
		}
		observer = composeObserver(rend, sink)
		if sink != nil {
			defer func() { _ = sink.Close() }()
		}
	}

	req := agent.Request{
		Goal:           line,
		System:         sess.baseSystem,
		HistorySummary: sess.session.historySummary(), // nil-safe: nil session => empty
		History:        sess.session.history(),        // nil-safe: nil session => nil
		Tools:          sess.tools,
		MaxSteps:       sess.maxSteps,
		Budget:         sess.budget,
		Approver:       approver, // nil when read-only => runtime fail-safe denies Write/Exec
		Options:        sess.modelOptions,
	}
	res, runErr := sess.orch.Run(runCtx, req, observer)

	// Post-run observability on EVERY exit path. Uses the parent ctx (not runCtx)
	// so a canceled turn still flushes its partial trace.
	if sess.obs != nil {
		status, partial := runStatus(runErr, runCtx.Err() != nil)
		if sink != nil {
			if ferr := sink.Finish(res, status); ferr != nil {
				_, _ = fmt.Fprintf(out, "warning: telemetry write incomplete: %v\n", ferr)
			}
		}
		if sess.obs.trace {
			meta := agenttrace.TraceMeta{
				Goal:           req.Goal,
				System:         req.System,
				HistorySummary: req.HistorySummary,
				History:        req.History,
				MaxSteps:       req.MaxSteps,
				Budget:         req.Budget,
				ToolSchemaHash: toolSchemaHash(sess.tools),
				ModelHint:      lastRoutedModel(res),
			}
			startedStr := startedAt.UTC().Format(time.RFC3339Nano)
			endedStr := sess.obs.clock().UTC().Format(time.RFC3339Nano)
			if terr := sess.obs.writeTrace(runID, startedStr, endedStr, meta, res, status, partial, runErr); terr != nil {
				_, _ = fmt.Fprintf(out, "warning: trace not written: %v\n", terr)
			}
		}
	}

	if runErr != nil {
		if runCtx.Err() != nil {
			_, _ = fmt.Fprintln(out, "\ncanceled")
			return res, runErr
		}
		_, _ = fmt.Fprintf(out, "\nerror: %v\n", runErr)
		return res, runErr
	}
	if m := lastRoutedModel(res); m != "" {
		sess.lastModel = m
	}
	// Persist only a successful, answered run. recordResult uses the parent ctx so
	// saving the computed answer is not tied to this turn's cancellation scope.
	if sess.session != nil && res.Answer != "" {
		if serr := sess.session.recordResult(ctx, line, res); serr != nil {
			_, _ = fmt.Fprintf(out, "warning: session not saved: %v\n", serr)
		}
		if cerr := sess.maybeCompress(runCtx); cerr != nil {
			_, _ = fmt.Fprintf(out, "warning: compression skipped: %v\n", cerr)
		}
	}
	rend.finalFooter(res)
	return res, nil
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
			// Conversation deletion deliberately does not cascade into agent
			// memory (separate storage concepts); say so to avoid surprise.
			if sess.records != nil {
				_, _ = fmt.Fprintln(out, "agent-memory records kept (see /records)")
			}
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
	case "/records":
		handleRecords(ctx, out, sess, fields)
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
  /records [--forget <id> | --promote <id> <semantic|episodic>]
                 list agent-memory records, forget one, or promote one (with -agent-memory)
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
