package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

// replSession holds the per-process state the REPL needs.
type replSession struct {
	orch                *agent.Orchestrator
	runtime             *golemruntime.Runtime
	newOrchestrator     func() *agent.Orchestrator
	tools               []agent.Tool
	baseSystem          string
	projectContextBlock string // raw fenced project-context block; reused by the planner (-goal)
	maxSteps            int
	budget              agent.Budget
	color               bool
	clock               func() time.Time
	retrieveOmitted     bool // when true, /tools appends the omission note

	session *session // nil => --no-session (no history, no persistence)

	memory       *memory.SQLiteStore // nil => memory disabled (-no-memory or open failed)
	memoryDBPath string              // used to re-secure SQLite sidecars after writes
	workspaceID  string              // stable id used to scope memory create/list/search

	records *memory.MemoryRecordStore // nil => agent memory disabled (-agent-memory absent or open failed)

	lastModel string           // last routed ActualModel for /model
	journal   *mutationJournal // nil unless -allow-write enabled writes
	// grants is the session-scoped approval grant store (#341). Built once at
	// startup; cleared unconditionally on /new and /clear, on successful
	// /resume, and via /grants clear; read by the per-run approver. nil only
	// in grant-free contexts (methods are nil-safe; the /auto-edits and
	// /grants commands lazily initialize it).
	grants       *approvalGrants
	allowWrite   bool
	allowExec    bool
	mcpAttached  bool    // true when external MCP tools are attached (force approver)
	obs          *observ // nil unless -trace/-telemetry enabled
	feedback     *feedbackService
	pressureWarn bool // enable the one-per-run context-pressure warning line
	// mixed mirrors what newOrchestratorFactory puts in ContextManager.Mixed, so
	// the renderer can tell whether a tool result's flat Content is what the
	// model actually read. Same -progressive flag, one source.
	mixed bool

	modelOptions provider.ModelOptions // per-run model options (-think)

	// control coordinates the prompt, async notices, and Ctrl-C. nil in tests
	// and non-interactive callers, where runREPL falls back to a plain prompt
	// and the caller's interrupt wiring.
	control *replControl

	// goalEditor backs /edit. nil outside the default REPL and in narrow
	// tests that never dispatch it; nil renders the unavailable message.
	goalEditor goalEditor
}

// runREPL reads lines from in, dispatching slash commands and running every
// other line as an agent goal. A value on interrupts cancels the in-flight Run
// without ending the loop. EOF (Ctrl-D) returns nil.
func runREPL(ctx context.Context, src lineSource, out io.Writer, interrupts <-chan struct{}, sess *replSession) error {
	for {
		if sess.control != nil {
			sess.control.enterPrompt()
		}
		// The source prints the prompt. runREPL must not: the editor arriving
		// in task 5 prints and repaints its own, and two printers double it.
		line, ok, err := src.ReadGoal(ctx, promptText)
		if err != nil {
			if ctx.Err() != nil {
				return nil // ctx canceled at the prompt: exit quietly (idle Ctrl-C quit / shutdown)
			}
			return err // scanner failure (e.g. line too long): surface it, as before
		}
		if !ok {
			_, _ = fmt.Fprintln(out)
			return nil // EOF (Ctrl-D)
		}
		if sess.control != nil {
			sess.control.enterTurn()
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			forced, exit := dispatchSlash(ctx, out, sess, line)
			if exit {
				return nil
			}
			if forced == "" {
				continue
			}
			// /edit's result is a forced model goal: it falls through to the
			// recording and run below, bypassing slash dispatch exactly once
			// even when it begins with "/".
			line = forced
		}
		// One UTF-8 boundary for every source. The editor rejects malformed
		// bytes at the terminal, but the scanner and /edit never pass through
		// it, and the provider transport is JSON: it would substitute U+FFFD
		// silently, so the model would answer a question the user did not type
		// and history would store bytes arrow recall cannot reproduce. A
		// correctly encoded U+FFFD is fine here -- only x/term cannot represent
		// it -- so validity, not content, is the test.
		if !utf8.ValidString(line) {
			_, _ = fmt.Fprintln(out, invalidUTF8Warning)
			continue
		}
		// Recorded only here: after trimming, after the empty and slash checks,
		// and after validation, so a blank line, a command, or malformed bytes
		// can never reach history.
		src.RecordGoal(line)
		_, _ = runOnce(ctx, out, interrupts, sess, line, src)
	}
}

// errOneShotFailed signals a one-shot turn that already reported its failure
// on stderr; main exits non-zero without printing a second message.
var errOneShotFailed = errors.New("one-shot run failed")

// runOneShot executes exactly one agent turn for -p. Only the final answer is
// written to stdout (with a single trailing newline); every other line the
// turn produces — tool progress, warnings, errors — goes to stderr via
// runOnce. A nil line source means no interactive approver exists, so the
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
func runOnce(ctx context.Context, out io.Writer, interrupts <-chan struct{}, sess *replSession, line string, src lineSource) (agent.Result, error) {
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

	rend := newRenderer(out, sess.color, sess.maxSteps, sess.clock, sess.mixed)
	rend.warnPressure = sess.pressureWarn
	renderOut := rend.rawWriter()
	writeRunLine := func(format string, args ...any) {
		_ = rend.breakLine()
		_, _ = fmt.Fprintf(renderOut, format+"\n", args...)
	}
	// A nil source means no interactive approver is available -- one-shot mode
	// in production, read-only sessions in tests. It is capability absence, not
	// a mode assertion: the runtime's nil-approver fail-safe then denies every
	// gated call.
	var approver agent.Approver
	if src != nil && needsApprover(sess.allowWrite, sess.allowExec, sess.mcpAttached) {
		ap := newReplApprover(src, renderOut, sess.color)
		ap.beforeWrite = rend.breakLine
		ap.grants = sess.grants
		approver = ap
	}

	var (
		runID     = conversation.NewID()
		startedAt time.Time
		sink      *agenttrace.TelemetrySink
	)
	if sess.obs != nil {
		runID = sess.obs.nextRunID()
		startedAt = sess.obs.clock()
		var serr error
		if sink, serr = sess.obs.startSink(runID, startedAt); serr != nil {
			writeRunLine("warning: telemetry disabled: %v", serr)
		}
		if sink != nil {
			defer func() { _ = sink.Close() }()
		}
	}
	var feedbackObserver agent.Observer
	if sess.feedback != nil {
		feedbackObserver = sess.feedback.observer(runID)
	}
	observer := composeObserver(rend, sink, feedbackObserver)

	// Trace metadata only: the runtime builds the real agent.Request. Tools,
	// Approver, and Options are deliberately absent — the runtime supplies its
	// own tools and model options (both from golem.Options), and the approver
	// travels on the Turn below.
	req := agent.Request{
		Goal:           line,
		System:         sess.baseSystem,
		HistorySummary: sess.session.historySummary(), // nil-safe: nil session => empty
		History:        sess.session.history(),        // nil-safe: nil session => nil
		MaxSteps:       sess.maxSteps,
		Budget:         sess.budget,
	}
	threadID := ""
	if sess.session != nil {
		threadID = sess.session.id
	}
	res, runErr := sess.runtime.Run(runCtx, golemruntime.Turn{
		ThreadID: threadID,
		RunID:    runID,
		Message:  line,
		Approver: approver, // nil when read-only => runtime fail-safe denies Write/Exec
		Observer: observer,
	}, func(golemruntime.Event) error { return nil })
	sess.feedback.finishRun(runID)
	// An interrupted approval surfaces as context.Canceled from inside the run
	// and races the interrupt watcher's cancel(). Synchronize the derived
	// context here so status and rendering never depend on scheduler order: an
	// interrupted approval IS a cancellation. This deliberately flips the
	// recorded trace/telemetry status for that case from error to canceled.
	if errors.Is(runErr, context.Canceled) {
		cancel()
	}
	var sessionSaveErr error
	if res.Answer != "" &&
		errors.Is(runErr, golemruntime.ErrSessionPersistence) &&
		!errors.Is(runErr, context.Canceled) &&
		!errors.Is(runErr, context.DeadlineExceeded) {
		sessionSaveErr = runErr
		runErr = nil
	}
	// A failed tail flush loses only buffered display bytes on the progress
	// stream; the run itself completed. Demoting it to a warning keeps a good
	// one-shot answer printable and records telemetry as the success it was.
	if ferr := rend.finish(); ferr != nil && runErr == nil {
		writeRunLine("warning: render flush incomplete: %v", ferr)
	}

	// Post-run observability on EVERY exit path. Uses the parent ctx (not runCtx)
	// so a canceled turn still flushes its partial trace.
	if sess.obs != nil {
		status, partial := runStatus(runErr, runCtx.Err() != nil)
		if sink != nil {
			if ferr := sink.Finish(res, status); ferr != nil {
				writeRunLine("warning: telemetry write incomplete: %v", ferr)
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
				writeRunLine("warning: trace not written: %v", terr)
			}
		}
	}

	if runErr != nil {
		if runCtx.Err() != nil {
			writeRunLine("canceled")
			return res, runErr
		}
		writeRunLine("error: %v", runErr)
		return res, runErr
	}
	if m := lastRoutedModel(res); m != "" {
		sess.lastModel = m
	}
	if sessionSaveErr != nil {
		writeRunLine("warning: session not saved: %v", sessionSaveErr)
	} else if sess.session != nil && res.Answer != "" {
		if _, err := sess.session.switchTo(ctx, sess.session.id); err != nil {
			writeRunLine("warning: session state not refreshed: %v", err)
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
// dispatchSlash handles one slash command. A non-empty forced return is a
// goal the caller must run as a model goal -- /edit's result, which bypasses
// slash dispatch exactly once even when it begins with "/".
func dispatchSlash(ctx context.Context, out io.Writer, sess *replSession, line string) (forced string, exit bool) {
	fields := strings.Fields(line)
	cmd := fields[0]
	switch cmd {
	case "/exit", "/quit":
		return "", true
	case "/help":
		_, _ = fmt.Fprint(out, golemHelp)
	case "/clear":
		// Reset semantics (#341 D8): approval grants drop unconditionally,
		// before the session branch — under --no-session a live approver can
		// still hold grants.
		sess.grants.clear()
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
		// Session switch (#341 D8): grants never outlive the session they
		// were given in, with or without conversation persistence.
		sess.grants.clear()
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
			// Success only (#341 D8): a failed /resume leaves the active
			// session — and therefore its grants — untouched.
			sess.grants.clear()
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
		if sess.allowWrite {
			_, _ = fmt.Fprintf(out, "auto-edits: %s\n", autoEditState(sess))
		}
	case "/auto-edits":
		if !sess.allowWrite {
			_, _ = fmt.Fprintln(out, "writes disabled (run with -allow-write)")
			return "", false
		}
		if sess.grants == nil {
			sess.grants = newApprovalGrants() // D9: the toggle must never lie
		}
		switch {
		case len(fields) == 1:
			_, _ = fmt.Fprintf(out, "auto-edits: %s\n", autoEditState(sess))
		case len(fields) == 2 && fields[1] == "on":
			sess.grants.grant(grantScopeFiles, tools.WriteClassApprovalKey)
			_, _ = fmt.Fprintln(out, "auto-edits: on")
		case len(fields) == 2 && fields[1] == "off":
			sess.grants.revoke(grantScopeFiles, tools.WriteClassApprovalKey)
			_, _ = fmt.Fprintln(out, "auto-edits: off")
		default:
			_, _ = fmt.Fprintln(out, "usage: /auto-edits [on|off]")
		}
	case "/grants":
		if sess.grants == nil {
			sess.grants = newApprovalGrants() // D9: state shown must be state stored
		}
		switch {
		case len(fields) == 1:
			_, _ = fmt.Fprintf(out, "session grants: %d\n", sess.grants.count())
			if sess.allowWrite {
				_, _ = fmt.Fprintf(out, "auto-edits: %s\n", autoEditState(sess))
			}
		case len(fields) == 2 && fields[1] == "clear":
			sess.grants.clear()
			_, _ = fmt.Fprintln(out, "session grants cleared")
		default:
			_, _ = fmt.Fprintln(out, "usage: /grants [clear]")
		}
	case "/edit":
		// Capability is independent of the line editor: -no-editor selects
		// the scanner for input but leaves /edit available on a real TTY. A
		// piped script must never spawn an interactive editor.
		if sess.goalEditor == nil || !sess.goalEditor.Available() {
			_, _ = fmt.Fprintln(out, "/edit requires an interactive terminal")
			return "", false
		}
		seed := strings.TrimSpace(strings.TrimPrefix(line, cmd))
		// The editor owns the screen for its lifetime, and golem cannot repaint
		// what it does not draw. Notices are held and flushed afterwards rather
		// than painted over it.
		if sess.control != nil {
			sess.control.suspendNotices()
			defer sess.control.resumeNotices()
		}
		text, err := sess.goalEditor.Compose(ctx, seed)
		switch {
		case errors.Is(err, errEditTooLarge):
			_, _ = fmt.Fprintln(out, goalLimitWarning)
		case err != nil:
			_, _ = fmt.Fprintf(out, "edit failed: %v\n", err)
		default:
			// Non-empty text is the forced goal; empty aborts to the prompt.
			return strings.TrimSpace(text), false
		}
	default:
		_, _ = fmt.Fprintf(out, "unknown command: %s (try /help)\n", cmd)
	}
	return "", false
}

// autoEditState reports the write-class grant as a toggle state. The "a"
// answer on a write/edit prompt and /auto-edits on set the same grant, so
// this is the single source of truth for both.
func autoEditState(sess *replSession) string {
	if sess.grants.granted(grantScopeFiles, tools.WriteClassApprovalKey) {
		return "on"
	}
	return "off"
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
  /edit [seed]   compose a goal in $VISUAL/$EDITOR (quoting unsupported)
  /undo          revert the last applied write (when -allow-write)
  /auto-edits [on|off]
                 show or set session auto-approval for write/edit tools
  /grants [clear]
                 count active session approval grants, or revoke them all
  /remember [--global] <text>
                 save a memory (workspace scope unless --global)
  /forget <id>   delete a saved memory
  /memories [--promote <id> | --localize <id>]
                 list saved memories, or change a memory's scope
  /records [--forget <id> | --promote <id> <semantic|episodic>]
                 list agent-memory records, forget one, or promote one (with -agent-memory)
  /exit, /quit   leave golem
any other line is sent to the agent as a goal.
approval prompts marked [y/N/a] accept "a": approve and allow the same
action again for the rest of this session (exec: exact command only).
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
