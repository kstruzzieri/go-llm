package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	orch            *agent.Orchestrator
	runtime         *golemruntime.Runtime
	newOrchestrator func() *agent.Orchestrator
	tools           []agent.Tool
	baseSystem      string
	// Mid-session mounting (#372). root is the canonical workspace root the
	// gated tools are built over; sysInputs are the composition inputs behind
	// baseSystem (invariant: baseSystem == composeSystem(sysInputs); the
	// -goal planner reads sysInputs.projectContext); tools[:readToolCount] are
	// the file tools the runtime rebuilds itself; mountAt is where the gated
	// write/exec tools sit (startup order parity); writeToolCount is how many
	// write tools are mounted (exec inserts after them); scratch records
	// -scratch so /allow-write can say promotion stays startup-bound;
	// lateStore is the checkpoint store /allow-write opened (nil when
	// -allow-write owned it at startup, whose store main.go closes itself).
	root           string
	sysInputs      systemInputs
	readToolCount  int
	mountAt        int
	writeToolCount int
	scratch        bool
	lateStore      *checkpointStore
	// verifier is the REPL-mode post-write verification slot (#372); nil
	// outside the REPL and in narrow tests (set is nil-safe).
	verifier        *lateVerifier
	maxSteps        int
	budget          agent.Budget
	color           bool
	clock           func() time.Time
	retrieveOmitted bool // when true, /tools appends the omission note

	session *session // nil => --no-session (no history, no persistence)

	memory       *memory.SQLiteStore // nil => memory disabled (-no-memory or open failed)
	memoryDBPath string              // used to re-secure SQLite sidecars after writes
	workspaceID  string              // stable id used to scope memory create/list/search

	records *memory.MemoryRecordStore // nil => agent memory disabled (-agent-memory absent or open failed)

	lastModel string             // last routed ActualModel for /model
	journal   *checkpointJournal // nil unless -allow-write enabled writes
	// bgManager owns every background command (#346). nil => background exec
	// disabled (-allow-exec absent or non-interactive mode). Process-scoped by
	// the interactive-process-scope policy: /new, /clear, and successful
	// /resume leave its jobs and handles untouched (grants still clear at
	// those boundaries).
	bgManager *tools.BackgroundManager
	// grants is the session-scoped approval grant store (#341). Built once at
	// startup; cleared unconditionally on /new and /clear, on successful
	// /resume, and via /grants clear; read by the per-run approver. nil only
	// in grant-free contexts (methods are nil-safe; the /auto-edits and
	// /grants commands lazily initialize it).
	grants *approvalGrants
	// destAdmission is the destination-consent surface (#477). Its lifetime
	// is deliberately different from grants: conversation resets (/new,
	// /clear, /resume) leave destination authority standing, while
	// /grants clear revokes it and the next GOAL re-runs the batch gate.
	// nil in ungated contexts.
	destAdmission *destinationAdmission
	// headlessApprover is the non-interactive -allow-tool approver (#352). It
	// is non-nil only for one-shot runs that named at least one gated tool;
	// the REPL never has one, and a nil value keeps the pre-#352 fail-safe.
	headlessApprover *headlessApprover
	// machine is the -output-format json/stream-json writer (#352). It is nil
	// in text mode and in the REPL, and every call site treats nil as "no
	// machine output", so no existing path changes shape.
	machine      *machineWriter
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

	// grounding runs claim-support verification after a completed turn (#348).
	// nil unless -grounding is active AND both retrieval and a verifier chain
	// resolved at startup.
	grounding *groundingService

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
		// Pre-turn boundary (#477 D12): a /grants clear leaves ensure
		// re-armed, and the SAME batch gate re-runs here — before the goal,
		// never mid-turn. Denial (or decline) keeps the session usable for
		// slash commands but runs no goal.
		if sess.destAdmission != nil {
			if err := sess.destAdmission.ensure(ctx); err != nil {
				_, _ = fmt.Fprintf(out, "destination admission: %v\n", err)
				continue
			}
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

// runOneShot executes exactly one agent turn for -p. In text mode only the
// final answer is written to stdout (with a single trailing newline); every
// other line the turn produces — tool progress, warnings, errors — goes to
// stderr via runOnce. A nil line source means no interactive approver exists,
// so absent a #352 headless approver the runtime fail-safe denies any
// approval-gated tool call.
//
// #352: in a machine format, stdout carries the protocol event stream
// (stream-json) or nothing yet (json), and then exactly one golem.result.v1
// record — written on EVERY outcome, including failures and cancellations, so
// a consumer always ends on a result line. The record, not the protocol
// terminal event, completes the stream.
func runOneShot(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, prompt string) error {
	res, runErr := runOnce(ctx, stderr, interrupts, sess, prompt, nil)
	if sess.machine != nil {
		// The record mirrors the protocol STREAM, the exit code mirrors the
		// INVOCATION, and the two can legitimately diverge: a post-run local
		// failure (e.g. a checkpoint seal error joined into runErr after
		// run.finished was already emitted) leaves the record "completed"
		// while the process exits 1 with the cause on stderr. Rewriting the
		// record would make it disagree with the terminal event a stream-json
		// consumer already read.
		rec := sess.machine.buildResult(res, runErr)
		if werr := writeJSONLine(stdout, rec); werr != nil {
			_, _ = fmt.Fprintf(stderr, "golem: machine output incomplete: %v\n", werr)
			return errOneShotFailed
		}
		if runErr != nil || rec.Status != "completed" {
			return errOneShotFailed // reported in the record; exit 1
		}
		return nil
	}
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
	//
	// #352: -allow-tool installs a non-interactive approver instead. It is
	// checked FIRST because a headless run has no line source by construction,
	// so the interactive branch could never be reached for it.
	var approver agent.Approver
	switch {
	case sess.headlessApprover != nil:
		approver = sess.headlessApprover
	case src != nil && needsApprover(sess.allowWrite, sess.allowExec, sess.mcpAttached):
		ap := newReplApprover(src, renderOut, sess.color)
		ap.beforeWrite = rend.breakLine
		ap.grants = sess.grants
		approver = ap
	}

	// Arm the durable checkpoint journal for this turn (#355). A refusal
	// (interrupted undo pending, or the journal latched by an earlier
	// failure) blocks the model turn before the provider is called and
	// before any telemetry sink opens for a run that will never happen;
	// slash commands remain usable.
	if sess.journal != nil {
		if jerr := sess.journal.beginTurn(runCtx, line, cancel); jerr != nil {
			writeRunLine("checkpoint: %v", jerr)
			if ferr := rend.finish(); ferr != nil {
				writeRunLine("warning: render flush incomplete: %v", ferr)
			}
			return agent.Result{}, jerr
		}
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
	// Grounding capture is per turn: the recorder holds only what THIS turn
	// retrieved, and the collector only observes when the feature is active. A
	// typed nil would still satisfy agent.Observer, so the variable stays an
	// interface and composeObserver drops it when it is nil.
	var (
		groundCollector *groundingCollector
		groundObserver  agent.Observer
	)
	if sess.grounding != nil {
		sess.grounding.beginTurn()
		groundCollector = &groundingCollector{}
		groundObserver = groundCollector
	}
	observer := composeObserver(rend, sink, feedbackObserver, groundObserver)

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
	}, sess.machine.sink())
	// Seal immediately after Run on every path: writes applied before an
	// interrupt or provider error must stay undoable. The error is joined
	// with the run error below, after the session-persistence demotion, so
	// that demotion can never swallow a durability failure.
	var sealErr error
	if sess.journal != nil {
		sealErr = sess.journal.sealTurn(ctx)
	}
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
	if sealErr != nil {
		writeRunLine("checkpoint: %v", sealErr)
		runErr = errors.Join(runErr, sealErr)
	}
	// A failed tail flush loses only buffered display bytes on the progress
	// stream; the run itself completed. Demoting it to a warning keeps a good
	// one-shot answer printable and records telemetry as the success it was.
	if ferr := rend.finish(); ferr != nil && runErr == nil {
		writeRunLine("warning: render flush incomplete: %v", ferr)
	}

	// The agent run's outcome is SNAPSHOTTED here, before grounding, because
	// grounding is a post-run model call that can block, time out, or be
	// interrupted.
	//
	// The end time is load-bearing: taken after grounding it would fold the
	// verifier's wall time into the agent run's recorded duration. The status is
	// ordering insurance rather than a live guard — runStatus returns
	// "completed" whenever runErr is nil, ahead of its cancellation case, and
	// grounding runs only when the status already IS "completed" and never
	// touches runErr, so recomputing it later would agree today. It is
	// snapshotted anyway so that a future change to runStatus's precedence
	// cannot silently relabel a completed answer as canceled because its
	// verifier was interrupted.
	status, partial := runStatus(runErr, runCtx.Err() != nil)
	endedStr := ""
	// Post-run observability on EVERY exit path. Uses the parent ctx (not runCtx)
	// so a canceled turn still flushes its partial trace.
	if sess.obs != nil {
		endedStr = sess.obs.clock().UTC().Format(time.RFC3339Nano)
		// Telemetry's root run span closes from its own clock, so it must finish
		// BEFORE grounding runs or the span absorbs the verifier's wall time.
		if sink != nil {
			if ferr := sink.Finish(res, status); ferr != nil {
				writeRunLine("warning: telemetry write incomplete: %v", ferr)
			}
		}
	}
	runDuration := rend.now().Sub(rend.runStart)

	// Grounding verification (#348). Only a completed run has a final answer
	// worth judging, and only runCtx makes Ctrl-C during the judge prompt. It
	// never assigns to res, res.Usage, or runErr: a verifier outcome is not an
	// agent outcome.
	var groundingRaw json.RawMessage
	if sess.grounding != nil && status == "completed" {
		rep, diag, show := sess.grounding.verify(runCtx, res.Answer, groundCollector, func() {
			// Only fires when a model call is actually being made. Two sequential
			// verifier calls on a local backend can take tens of seconds, and the
			// answer has already finished streaming, so without this the REPL
			// looks hung until the verdict lands.
			_ = rend.writeDim(groundingCheckingLine)
		})
		if show {
			// Dim, like the run footer: this is a status line about the turn, not
			// part of the answer. Writing it through the renderer also keeps it
			// ordered against the streamed answer instead of racing the markdown
			// buffer, and a failed write costs only display bytes.
			_ = rend.writeDim(groundingSummaryLine(rep, diag))
			if raw, merr := json.Marshal(rep); merr == nil {
				groundingRaw = raw
				// #352: the machine surface reuses the SAME marshalled report
				// the trace records, so the two can never disagree.
				sess.machine.setGrounding(raw)
			}
		}
	}

	if sess.obs != nil && sess.obs.trace {
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
		if terr := sess.obs.writeTrace(runID, startedStr, endedStr, meta, res, status, partial, runErr, groundingRaw); terr != nil {
			writeRunLine("warning: trace not written: %v", terr)
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
	rend.finalFooter(res, runDuration)
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
			_, _ = fmt.Fprintln(out, "writes disabled; run /allow-write or start with -allow-write")
			return "", false
		}
		n := 1
		switch {
		case len(fields) == 1:
		case len(fields) == 2:
			v, err := strconv.Atoi(fields[1])
			if err != nil || v < 1 {
				_, _ = fmt.Fprintln(out, "usage: /undo [n]")
				return "", false
			}
			n = v
		default:
			_, _ = fmt.Fprintln(out, "usage: /undo [n]")
			return "", false
		}
		sess.journal.undo(ctx, out, n)
	case "/checkpoints":
		if sess.journal == nil {
			_, _ = fmt.Fprintln(out, "writes disabled; run /allow-write or start with -allow-write")
		} else if len(fields) == 1 || (len(fields) == 2 && fields[1] == "list") {
			sess.journal.listCheckpoints(ctx, out)
		} else {
			_, _ = fmt.Fprintln(out, "usage: /checkpoints [list]")
		}
	case "/jobs":
		handleJobs(ctx, out, sess, fields)
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
			_, _ = fmt.Fprintln(out, "writes disabled; run /allow-write or start with -allow-write")
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
			if sess.destAdmission != nil {
				sess.destAdmission.render(out)
			}
		case len(fields) == 2 && fields[1] == "clear":
			sess.grants.clear()
			if sess.destAdmission != nil {
				sess.destAdmission.revoke()
				_, _ = fmt.Fprintln(out, "session grants cleared (destination grants revoked; re-approval before the next goal)")
			} else {
				_, _ = fmt.Fprintln(out, "session grants cleared")
			}
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
	case "/allow-write":
		handleAllowWrite(ctx, out, sess, fields)
	case "/allow-exec":
		handleAllowExec(ctx, out, sess, fields)
	default:
		_, _ = fmt.Fprintf(out, "unknown command: %s (try /help)\n", cmd)
	}
	return "", false
}

// handleJobs implements /jobs (#346): pull-based visibility over the session's
// background jobs plus a user-direct stop. The stop is user-initiated, so it
// never routes through the model-call approver; the manager's frozen error
// classes (unknown handle) render as-is. Context expiry mirrors the model
// path (stop_command): the kill is already issued, so it reports the stop as
// requested rather than printing a raw context error.
func handleJobs(ctx context.Context, out io.Writer, sess *replSession, fields []string) {
	if sess.bgManager == nil {
		_, _ = fmt.Fprintln(out, "exec disabled; run /allow-exec or start with -allow-exec")
		return
	}
	switch {
	case len(fields) == 1:
		jobs := sess.bgManager.List()
		if len(jobs) == 0 {
			_, _ = fmt.Fprintln(out, "no background jobs")
			return
		}
		for _, st := range jobs {
			_, _ = fmt.Fprintln(out, renderJobLine(st))
		}
	case len(fields) == 3 && fields[1] == "stop":
		// Bound the reap wait to 10s, matching the model path's tool-call
		// timeout (bgToolTimeout); expiry lands in the still-reaping branch
		// below, so the prompt is never blocked indefinitely.
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		st, err := sess.bgManager.Stop(stopCtx, fields[2])
		renderJobStopResult(out, st, err)
	default:
		_, _ = fmt.Fprintln(out, "usage: /jobs [stop <handle>]")
	}
}

func renderJobStopResult(out io.Writer, st tools.JobStatus, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Early return by contract: the kill is already issued; the manager
			// finishes reaping on its own.
			_, _ = fmt.Fprintf(out, "stop requested for %s; still reaping\n", st.Handle)
			return
		}
		_, _ = fmt.Fprintln(out, err)
		return
	}
	switch {
	case st.State == "killed":
		_, _ = fmt.Fprintf(out, "stopped %s (killed)\n", st.Handle)
	case st.ExitKnown:
		_, _ = fmt.Fprintf(out, "already finished: %s (exit %d)\n", st.Handle, st.ExitCode)
	default:
		_, _ = fmt.Fprintf(out, "already finished: %s (%s)\n", st.Handle, st.State)
	}
}

// renderJobLine renders one /jobs listing line: handle, state (with exit code
// when known), pid, then argv through the dedicated control-safe renderer.
func renderJobLine(st tools.JobStatus) string {
	state := st.State
	if st.ExitKnown {
		state = fmt.Sprintf("%s exit=%d", st.State, st.ExitCode)
	}
	return fmt.Sprintf("%s  %s  pid=%d  %s", st.Handle, state, st.PID, renderJobArgv(st.Argv))
}

// jobArgvDisplayCap bounds the rendered argv to one terminal-friendly line.
const jobArgvDisplayCap = 60

// renderJobArgv renders a job's argv as ONE control-safe display line. Every
// non-graphic rune — C0 controls including newline/CR, DEL, the C1 range
// including U+009B (CSI), and the bidi format controls U+202A-202E and
// U+2066-2069 (category Cf, non-graphic) — renders as its escaped form via
// strconv.QuoteToGraphic, and the result is rune-safely truncated to
// jobArgvDisplayCap runes plus an ellipsis (escape-then-truncate, so a cut can
// only ever land between graphic runes). It deliberately differs from
// sanitizeApprovalPreview, which keeps newlines: approval previews are
// multi-line by design, a listing line never is. Presentation only.
func renderJobArgv(argv []string) string {
	joined := strings.Join(argv, " ")
	var b strings.Builder
	for _, r := range joined {
		if unicode.IsGraphic(r) {
			b.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteToGraphic(string(r))
		b.WriteString(quoted[1 : len(quoted)-1])
	}
	line := b.String()
	runes := []rune(line)
	if len(runes) > jobArgvDisplayCap {
		return string(runes[:jobArgvDisplayCap]) + "..."
	}
	return line
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
  /undo [n]      revert the last n completed turns' writes (when writes are enabled)
  /checkpoints   list undoable turn checkpoints, newest first (when writes are enabled)
  /jobs [stop <handle>]
                 list background jobs, or stop one (when exec is enabled)
  /auto-edits [on|off]
                 show or set session auto-approval for write/edit tools
  /grants [clear]
                 count active session approval grants, or revoke them all
  /allow-write   enable the approval-gated write_file/edit_file tools for the rest of this session
  /allow-exec    enable the approval-gated command tools for the rest of this session
  /remember [--global] <text>
                 save a memory (workspace scope unless --global)
  /forget <id>   delete a saved memory
  /memories [--promote <id> | --localize <id>]
                 list saved memories, or change a memory's scope
  /records [--forget <id> | --promote <id> <semantic|episodic>]
                 list agent-memory records, forget one, or promote one (with -agent-memory)
  /exit, /quit   leave golem
any other line is sent to the agent as a goal.
approval prompts offering "a" grant for the rest of this session; the
prompt names the scope: a=always this command covers one exact command
(not the contents of scripts it runs), a=all edits this session enables
auto-approval for every write/edit (same as /auto-edits on).
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
