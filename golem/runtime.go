// Package golem exposes Golem's embeddable agent runtime.
package golem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// ErrRunConflict reports an active duplicate run ID or thread.
var ErrRunConflict = errors.New("golem: run conflict")

// ErrClosed reports use of a closed Runtime.
var ErrClosed = errors.New("golem: runtime is closed")

// ErrInvalidRequest reports a malformed or oversized turn.
var ErrInvalidRequest = errors.New("golem: invalid request")

// ErrSessionPersistence reports a failure while persisting a completed answer.
var ErrSessionPersistence = errors.New("golem: session persistence failed")

var errDuplicateRunID = errors.New("golem: duplicate active run ID")

// ProtocolVersion is the current consumer event contract version.
const ProtocolVersion = 1

const (
	defaultMaxMessageBytes     = 64 * 1024
	maxContextDescriptionBytes = 256
	maxContextValueBytes       = 64 * 1024
	maxContextBytes            = 256 * 1024
	maxCorrelationIDBytes      = 256
)

// SessionStore loads and saves complete stateful-thread snapshots. Load must
// return an error matching conversation.ErrNotFound for a missing ID, and a
// successful Load must return a non-nil Conversation with that ID. Save must
// replace or upsert the complete snapshot.
//
// Calls for different thread IDs may overlap, so implementations must be safe
// for concurrent use. Same-thread serialization applies only within one
// Runtime; callers sharing a store across runtimes must not run the same thread
// concurrently. Compression may save a completed turn twice: first the raw
// snapshot, then a best-effort compressed snapshot. The caller owns the store
// and must keep it alive until Runtime.Close returns; Runtime never closes,
// migrates, or hardens it.
type SessionStore interface {
	Load(ctx context.Context, id string) (*conversation.Conversation, error)
	Save(ctx context.Context, conv conversation.Conversation) error
}

// Options configures a Runtime.
type Options struct {
	Root   string
	System string
	Tools  []agent.Tool
	// ScopeGuard is installed on the runtime-owned Workspace backing every
	// built-in file tool. It executes inside each tool, below approval
	// handling. Nil preserves current behavior exactly. Concurrent runs may
	// call it concurrently; it must not panic and must not call Runtime.Close
	// synchronously. Point lookups pass only the final cleaned
	// workspace-relative path — never its ancestors — so a guard must deny
	// descendants itself (e.g. deny "secrets" AND "secrets/..."); directory
	// walks consult it per directory and deny by skipping the subtree.
	ScopeGuard agenttools.ScopeGuard
	// FailureMessage presents the public message placed in run.failed. When
	// nil, Runtime preserves the existing truncated err.Error() behavior. The
	// error Run returns is never replaced by the presentation. Concurrent
	// runs may call it concurrently; it must not call Runtime.Close
	// synchronously.
	FailureMessage func(code string, err error) string
	ConfigPath     string
	MaxSteps       int
	Budget         agent.Budget
	// MaxMessageBytes bounds Turn.Message; zero or negative selects the
	// 64 KiB default. Trusted hosts (like the CLI) may raise it.
	MaxMessageBytes int
	ModelOptions    provider.ModelOptions
	// Summarizer enables history compression for stateful turns. When
	// Orchestrator is supplied without a Summarizer, threads persist
	// uncompressed and grow without bound.
	Summarizer         conversation.Summarizer
	DisableCompression bool
	// SessionStore overrides the lazy durable SQLite store for non-empty
	// ThreadIDs, including compression, without opening the default database.
	// Nil preserves the default SQLite behavior; empty ThreadIDs stay stateless.
	SessionStore SessionStore
	// RetainReasoning preserves model reasoning in returned Results. Leave
	// false for untrusted embedders; reasoning never reaches events or the
	// session database either way.
	RetainReasoning bool
	// OnWarning receives bootstrap/compression/hardening warnings. Concurrent
	// runs may call it concurrently; it must not call Runtime.Close
	// synchronously.
	OnWarning func(error)
	// Orchestrator overrides the config-driven bootstrap. The caller retains
	// ownership: Close never releases it or its providers.
	Orchestrator *agent.Orchestrator
	// DestinationPolicy governs which REMOTE model destinations the
	// config-driven runtime may reach (#477). The zero value fails closed:
	// local-only configurations work unchanged, and any reachable remote
	// destination returns a typed error (matching
	// provider.ErrDestinationDenied) before any outbound byte. Hosts opt in
	// with an exact provider.NewDestinationPolicy(...) set or an explicit,
	// greppable provider.AllowAllDestinations().
	//
	// Upgrade note for config-driven callers (e.g. Firn's commit-message
	// generator): a models.json whose resolved routes reach a hosted
	// provider now requires one of those two policies; a local-only config
	// needs nothing.
	//
	// Supplying a NONZERO policy together with Orchestrator is refused with
	// ErrDestinationPolicyIneffective: a custom orchestrator owns its own
	// transports, and accepting the policy would imply protection that does
	// not exist.
	DestinationPolicy provider.DestinationPolicy
	// Progressive enables #331 mixed context assembly on the orchestrator this
	// package bootstraps. That is ALL it does here. Off preserves current
	// behavior exactly.
	//
	// It does NOT configure any tool: this package builds no retrieval tool, so
	// a host that supplies agent/tools.Retrieve via Tools must set that tool's
	// own Progressive field to get progressive rendering. Setting only this
	// field yields mixed assembly over flat-rendered retrieval results.
	//
	// It also does not touch a caller-supplied Orchestrator, which brings its
	// own ContextManager. The golem CLI's -progressive flag drives both halves;
	// a library host owns both itself.
	Progressive bool
}

// ContextItem is trusted context supplied with one turn.
type ContextItem struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// Turn is one request to the runtime.
type Turn struct {
	ThreadID     string
	RunID        string
	Message      string
	Instructions string
	Context      []ContextItem
	Approver     agent.Approver
	Observer     agent.Observer
}

// Event is the versioned consumer event envelope. A run that stops early
// (cancellation, sink failure, or the orchestrator's tool-error cap ending a
// parallel batch) may leave tool.started events without a matching
// tool.finished; consumers must not assume pairing.
type Event struct {
	Protocol int             `json:"protocol"`
	ThreadID string          `json:"threadId,omitempty"`
	RunID    string          `json:"runId"`
	Seq      uint64          `json:"seq"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

// EventSink consumes ordered events from one run. It must return promptly —
// delivery buffering and timeouts are the consumer's job; a blocking sink
// stalls the run, wedges its thread reservation, and hangs Close. It must not
// call Runtime.Close synchronously; Cancel is safe.
type EventSink func(Event) error

type activeRun struct {
	cancel   context.CancelFunc
	terminal bool
}

// Runtime is a concrete facade over agent.Orchestrator.
type Runtime struct {
	orchestrator    *agent.Orchestrator
	root            string
	system          string
	tools           []agent.Tool
	maxSteps        int
	budget          agent.Budget
	maxMessageBytes int
	modelOptions    provider.ModelOptions
	summarizer      conversation.Summarizer
	compress        bool
	retainReasoning bool
	onWarning       func(error)
	failureMessage  func(code string, err error) string
	mu              sync.Mutex
	active          map[string]*activeRun
	activeThreads   map[string]*activeRun
	wg              sync.WaitGroup
	closed          bool
	closeDone       chan struct{}
	closeErr        error
	sessionMu       sync.Mutex
	sessions        *threadStore
	bundle          *providerbootstrap.Bundle
	closeOwned      func() error
}

// New constructs a Runtime.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("golem: initialize runtime: %w", err)
	}
	root, err := canonicalRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.System == "" {
		opts.System = SystemPrompt(false, false)
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return nil, fmt.Errorf("golem: build workspace tools: %w", err)
	}
	ws.SetScopeGuard(opts.ScopeGuard)
	fileTools := agenttools.NewFileToolsForWorkspace(ws)
	tools := append(fileTools, opts.Tools...)
	if err := validateTools(tools); err != nil {
		return nil, err
	}
	orchestrator := opts.Orchestrator
	var bundle *providerbootstrap.Bundle
	summarizer := opts.Summarizer
	if orchestrator == nil {
		var defaultSummarizer conversation.Summarizer
		orchestrator, bundle, defaultSummarizer, err = bootstrapOrchestrator(ctx, opts.ConfigPath, opts.DestinationPolicy, opts.Progressive, opts.OnWarning)
		if err != nil {
			return nil, err
		}
		if summarizer == nil {
			summarizer = defaultSummarizer
		}
	} else if !opts.DestinationPolicy.IsZero() {
		// #477 D9: with a caller-supplied orchestrator this package owns no
		// provider transports, so the policy would guard nothing.
		return nil, ErrDestinationPolicyIneffective
	}
	if err := ctx.Err(); err != nil {
		_ = bundle.Close()
		return nil, fmt.Errorf("golem: initialize runtime: %w", err)
	}
	maxMessage := opts.MaxMessageBytes
	if maxMessage <= 0 {
		maxMessage = defaultMaxMessageBytes
	}
	var sessions *threadStore
	if opts.SessionStore != nil {
		sessions = &threadStore{store: opts.SessionStore}
	}
	return &Runtime{
		orchestrator:    orchestrator,
		root:            root,
		system:          opts.System,
		tools:           tools,
		maxSteps:        opts.MaxSteps,
		budget:          opts.Budget,
		maxMessageBytes: maxMessage,
		modelOptions:    opts.ModelOptions,
		summarizer:      summarizer,
		compress:        !opts.DisableCompression && summarizer != nil,
		retainReasoning: opts.RetainReasoning,
		onWarning:       opts.OnWarning,
		failureMessage:  opts.FailureMessage,
		active:          make(map[string]*activeRun),
		activeThreads:   make(map[string]*activeRun),
		closeDone:       make(chan struct{}),
		sessions:        sessions,
		bundle:          bundle,
	}, nil
}

func validateTools(tools []agent.Tool) error {
	names := make(map[string]struct{}, len(tools))
	for i, tool := range tools {
		if tool == nil {
			return fmt.Errorf("golem: nil tool at index %d", i)
		}
		name := tool.Spec().Name
		if name == "" {
			return fmt.Errorf("golem: tool with empty name")
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("golem: duplicate tool name %q", name)
		}
		names[name] = struct{}{}
	}
	return nil
}

// Run executes one turn and emits ordered protocol events.
func (r *Runtime) Run(ctx context.Context, turn Turn, sink EventSink) (agent.Result, error) {
	if sink == nil {
		return agent.Result{}, fmt.Errorf("%w: event sink is required", ErrInvalidRequest)
	}
	if err := r.validateTurn(turn); err != nil {
		return agent.Result{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	seq := uint64(0)
	var sinkErr error
	makeEvent := func(eventType string, payload any, eventSeq uint64) (Event, error) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("golem: marshal %s event: %w", eventType, err)
		}
		return Event{
			Protocol: ProtocolVersion,
			ThreadID: turn.ThreadID,
			RunID:    turn.RunID,
			Seq:      eventSeq,
			Type:     eventType,
			Payload:  raw,
		}, nil
	}
	emit := func(eventType string, payload any) error {
		if sinkErr != nil {
			return sinkErr
		}
		event, err := makeEvent(eventType, payload, seq+1)
		if err != nil {
			return err
		}
		if err := validateEventSize(event); err != nil {
			return err
		}
		seq++
		err = sink(event)
		if err != nil {
			sinkErr = err
			cancel()
		}
		return err
	}
	active, err := r.reserve(turn, cancel)
	if err != nil {
		if errors.Is(err, errDuplicateRunID) {
			return agent.Result{}, err
		}
		if emitErr := emit("run.started", struct{}{}); emitErr != nil {
			return agent.Result{}, emitErr
		}
		if emitErr := emit("run.failed", r.runFailedPayload(err)); emitErr != nil {
			return agent.Result{}, emitErr
		}
		return agent.Result{}, err
	}
	defer r.release(turn, active)

	finish := func(eventType string, payload any, cause error) error {
		eventType = r.commitTerminal(active, runCtx, eventType)
		if eventType == "run.canceled" {
			payload = struct{}{}
		}
		if err := emit(eventType, payload); err != nil {
			return err
		}
		if eventType != "run.canceled" {
			return cause
		}
		if err := runCtx.Err(); err != nil {
			return err
		}
		if isCancellation(cause) {
			return cause
		}
		return context.Canceled
	}

	if err := emit("run.started", struct{}{}); err != nil {
		return agent.Result{}, err
	}
	if err := runCtx.Err(); err != nil {
		return agent.Result{}, finish("run.canceled", struct{}{}, err)
	}
	var thread *threadState
	if turn.ThreadID != "" {
		var err error
		thread, err = r.loadThread(runCtx, turn.ThreadID)
		if err != nil {
			eventType, payload := r.terminalFailure(err)
			return agent.Result{}, finish(eventType, payload, err)
		}
	}
	emitDelta := func(messageID, text string) error {
		chunks, err := splitDelta(text, func(chunk string) (bool, error) {
			event, err := makeEvent("message.delta", struct {
				MessageID string `json:"messageId"`
				Text      string `json:"text"`
			}{MessageID: messageID, Text: chunk}, math.MaxUint64)
			if err != nil {
				return false, err
			}
			err = validateEventSize(event)
			if errors.Is(err, errEventTooLarge) {
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if err := emit("message.delta", struct {
				MessageID string `json:"messageId"`
				Text      string `json:"text"`
			}{MessageID: messageID, Text: chunk}); err != nil {
				return err
			}
		}
		return nil
	}
	observer := &eventObserver{runID: turn.RunID, emit: emit, emitDelta: emitDelta, host: turn.Observer}
	goal, err := turnGoal(turn)
	if err != nil {
		return agent.Result{}, finish("run.failed", r.runFailedPayload(err), err)
	}
	request := agent.Request{
		Goal:     goal,
		System:   turnSystem(r.system, turn.Instructions),
		Tools:    r.tools,
		MaxSteps: r.maxSteps,
		Budget:   r.budget,
		Approver: turn.Approver,
		Options:  r.modelOptions,
	}
	if thread != nil {
		request.History = thread.history()
		request.HistorySummary = thread.summary()
	}
	result, err := r.orchestrator.Run(runCtx, request, observer)
	if !r.retainReasoning {
		scrubReasoning(&result)
	}
	if sinkErr != nil {
		return result, sinkErr
	}
	if err == nil {
		err = runCtx.Err()
	}
	if err != nil {
		eventType, payload := r.terminalFailure(err)
		return result, finish(eventType, payload, err)
	}
	if thread != nil && result.Answer != "" {
		if err := r.saveThread(runCtx, active, thread, turn.Message, result); err != nil {
			err = fmt.Errorf("%w: %w", ErrSessionPersistence, err)
			eventType, payload := r.terminalFailure(err)
			return result, finish(eventType, payload, err)
		}
	} else if err := runCtx.Err(); err != nil {
		return result, finish("run.canceled", struct{}{}, err)
	}
	model := ""
	for i := len(result.Steps) - 1; i >= 0; i-- {
		if outcome := result.Steps[i].RouteOutcome; outcome != nil {
			model = truncateDisplay(outcome.ActualModel.String())
			break
		}
	}
	return result, finish("run.finished", struct {
		StopReason string `json:"stopReason"`
		Model      string `json:"model"`
	}{
		StopReason: result.StopReason.String(),
		Model:      model,
	}, nil)
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// failurePayload is the run.failed event body.
type failurePayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// runFailedPayload centralizes every run.failed construction: classify the
// code (including the run_conflict override), apply the host presenter when
// installed, then cap the message exactly as the raw err.Error() was capped
// before. Presentation affects only the event payload — never the error Run
// returns.
func (r *Runtime) runFailedPayload(err error) failurePayload {
	code := failureCode(err)
	// Previously reservation-only; ErrRunConflict is only produced by reserve
	// today, so applying the override at every run.failed site is a deliberate,
	// benign unification.
	if errors.Is(err, ErrRunConflict) {
		code = "run_conflict"
	}
	message := err.Error()
	if r.failureMessage != nil {
		message = r.failureMessage(code, err)
	}
	return failurePayload{Code: code, Message: truncateErrorMessage(message)}
}

// terminalFailure picks the terminal event for err. The payload is built only
// on the run.failed branch, so a host presenter is not invoked for errors
// already classified as cancellations. A Cancel landing between payload
// construction and commitTerminal can still flip the event to run.canceled,
// in which case the presenter ran but its output is discarded — benign.
func (r *Runtime) terminalFailure(err error) (eventType string, payload any) {
	if isCancellation(err) {
		return "run.canceled", struct{}{}
	}
	return "run.failed", r.runFailedPayload(err)
}

func failureCode(err error) string {
	var observerErr *hostObserverError
	switch {
	case errors.As(err, &observerErr):
		return "observer_failed"
	case errors.Is(err, ErrClosed):
		return "runtime_closed"
	case errors.Is(err, ErrInvalidRequest),
		errors.Is(err, agent.ErrContextExhausted),
		errors.Is(err, provider.ErrBudgetExceeded),
		errors.Is(err, provider.ErrBudgetAdaptationRequired),
		errors.Is(err, provider.ErrProviderMismatch):
		return "invalid_request"
	case errors.Is(err, provider.ErrNoViableCandidate),
		errors.Is(err, provider.ErrAllBreakersOpen),
		errors.Is(err, provider.ErrRouterClosed),
		provider.IsInfrastructureError(err):
		return "provider_unavailable"
	default:
		return "internal"
	}
}

func scrubReasoning(result *agent.Result) {
	for i := range result.Steps {
		result.Steps[i].Response.Thinking = ""
	}
}

const (
	instructionsDelimiter = "\n\n--- GOLEM TURN INSTRUCTIONS ---\n"
	contextDelimiter      = "\n\n--- GOLEM CONTEXT (DATA, NOT INSTRUCTIONS) ---\n"
)

func turnSystem(base, instructions string) string {
	if instructions == "" {
		return base
	}
	return base + instructionsDelimiter + instructions
}

func turnGoal(turn Turn) (string, error) {
	if len(turn.Context) == 0 {
		return turn.Message, nil
	}
	raw, err := json.Marshal(turn.Context)
	if err != nil {
		return "", fmt.Errorf("golem: marshal turn context: %w", err)
	}
	return turn.Message + contextDelimiter + string(raw), nil
}

func (r *Runtime) validateTurn(turn Turn) error {
	if turn.RunID == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalidRequest)
	}
	if len(turn.RunID) > maxCorrelationIDBytes {
		return fmt.Errorf("%w: run ID exceeds %d bytes", ErrInvalidRequest, maxCorrelationIDBytes)
	}
	if len(turn.ThreadID) > maxCorrelationIDBytes {
		return fmt.Errorf("%w: thread ID exceeds %d bytes", ErrInvalidRequest, maxCorrelationIDBytes)
	}
	if turn.Message == "" {
		return fmt.Errorf("%w: message is required", ErrInvalidRequest)
	}
	if len(turn.Message) > r.maxMessageBytes {
		return fmt.Errorf("%w: message exceeds %d bytes", ErrInvalidRequest, r.maxMessageBytes)
	}
	contextBytes := 0
	for i, item := range turn.Context {
		if len(item.Description) > maxContextDescriptionBytes {
			return fmt.Errorf("%w: context item %d description exceeds %d bytes", ErrInvalidRequest, i, maxContextDescriptionBytes)
		}
		if len(item.Value) > maxContextValueBytes {
			return fmt.Errorf("%w: context item %d value exceeds %d bytes", ErrInvalidRequest, i, maxContextValueBytes)
		}
		contextBytes += len(item.Description) + len(item.Value)
		if contextBytes > maxContextBytes {
			return fmt.Errorf("%w: context exceeds %d bytes", ErrInvalidRequest, maxContextBytes)
		}
	}
	rawContext, err := json.Marshal(turn.Context)
	if err != nil {
		return fmt.Errorf("%w: marshal context: %v", ErrInvalidRequest, err)
	}
	if len(rawContext) > maxContextBytes {
		return fmt.Errorf("%w: serialized context exceeds %d bytes", ErrInvalidRequest, maxContextBytes)
	}
	return nil
}

// Cancel cancels an active run and reports whether it was found.
func (r *Runtime) Cancel(runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	active, ok := r.active[runID]
	if !ok || active.terminal {
		return false
	}
	active.cancel()
	return true
}

func (r *Runtime) reserve(turn Turn, cancel context.CancelFunc) (*activeRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[turn.RunID]; ok {
		return nil, fmt.Errorf("%w: %w %q", ErrRunConflict, errDuplicateRunID, turn.RunID)
	}
	if r.closed {
		return nil, ErrClosed
	}
	if turn.ThreadID != "" {
		if _, ok := r.activeThreads[turn.ThreadID]; ok {
			return nil, fmt.Errorf("%w: thread %q is active", ErrRunConflict, turn.ThreadID)
		}
	}
	active := &activeRun{cancel: cancel}
	r.active[turn.RunID] = active
	if turn.ThreadID != "" {
		r.activeThreads[turn.ThreadID] = active
	}
	r.wg.Add(1)
	return active, nil
}

func (r *Runtime) commitTerminal(active *activeRun, ctx context.Context, eventType string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if active.terminal {
		return eventType
	}
	if ctx.Err() != nil {
		eventType = "run.canceled"
	}
	active.terminal = true
	return eventType
}

func (r *Runtime) release(turn Turn, active *activeRun) {
	r.mu.Lock()
	if r.active[turn.RunID] == active {
		delete(r.active, turn.RunID)
	}
	if turn.ThreadID != "" && r.activeThreads[turn.ThreadID] == active {
		delete(r.activeThreads, turn.ThreadID)
	}
	r.mu.Unlock()
	r.wg.Done()
}

// Close cancels active runs, waits for them, and releases owned resources.
// It must not be called synchronously by code executing within an active Run.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closeDone == nil {
		r.closeDone = make(chan struct{})
	}
	if r.closed {
		done := r.closeDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closed = true
	// Cancel terminal runs too: after the terminal claim only best-effort
	// compression can still be running (the durable save is uncancelable), and
	// Close must not wait out a summarizer model call it could abort.
	for _, active := range r.active {
		active.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
	var err error
	if r.closeOwned != nil {
		err = r.closeOwned()
	} else {
		err = errors.Join(r.closeThreadStore(), r.bundle.Close())
	}
	r.mu.Lock()
	r.closeErr = err
	close(r.closeDone)
	r.mu.Unlock()
	return err
}

type eventObserver struct {
	runID     string
	emit      func(string, any) error
	emitDelta func(string, string) error
	host      agent.Observer
}

var _ agent.RetrievalPresentationObserver = (*eventObserver)(nil)

type hostObserverError struct {
	err error
}

func (e *hostObserverError) Error() string { return e.err.Error() }

func (e *hostObserverError) Unwrap() error { return e.err }

func wrapHostObserverError(err error) error {
	if err == nil {
		return nil
	}
	return &hostObserverError{err: err}
}

func (o *eventObserver) OnStep(ctx context.Context, event agent.StepEvent) error {
	if o.host == nil {
		return nil
	}
	return wrapHostObserverError(o.host.OnStep(ctx, event))
}

func (o *eventObserver) OnToolCall(ctx context.Context, event agent.ToolCallEvent) error {
	if err := o.emit("tool.started", struct {
		ToolCallID string `json:"toolCallId"`
		Name       string `json:"name"`
		Preview    string `json:"preview"`
	}{
		ToolCallID: event.Call.ID,
		Name:       event.Call.Function.Name,
		Preview:    truncatePreview(event.Preview),
	}); err != nil {
		return err
	}
	if o.host == nil {
		return nil
	}
	return wrapHostObserverError(o.host.OnToolCall(ctx, event))
}

func (o *eventObserver) OnToken(ctx context.Context, event agent.TokenEvent) error {
	if err := o.emitDelta(fmt.Sprintf("%s:%d", o.runID, event.Step), event.Content); err != nil {
		return err
	}
	if o.host == nil {
		return nil
	}
	return wrapHostObserverError(o.host.OnToken(ctx, event))
}

func (o *eventObserver) OnToolResult(ctx context.Context, event agent.ToolResultEvent) error {
	if event.Invoked {
		if err := o.emit("tool.finished", struct {
			ToolCallID string `json:"toolCallId"`
			Name       string `json:"name"`
			Preview    string `json:"preview"`
			IsError    bool   `json:"isError"`
		}{
			ToolCallID: event.Call.ID,
			Name:       event.Call.Function.Name,
			Preview:    truncatePreview(event.Result.Preview),
			IsError:    event.Result.IsError,
		}); err != nil {
			return err
		}
	}
	if host, ok := o.host.(agent.ToolResultObserver); ok {
		return wrapHostObserverError(host.OnToolResult(ctx, event))
	}
	return nil
}

func (o *eventObserver) OnPressure(ctx context.Context, event agent.PressureEvent) error {
	if host, ok := o.host.(agent.PressureObserver); ok {
		return wrapHostObserverError(host.OnPressure(ctx, event))
	}
	return nil
}

func (o *eventObserver) OnThinking(ctx context.Context, event agent.ThinkingEvent) error {
	if host, ok := o.host.(agent.ThinkingObserver); ok {
		return wrapHostObserverError(host.OnThinking(ctx, event))
	}
	return nil
}

// OnContextAssembly forwards the mixed-assembly trace to a host that opted into
// it. The trace is content-free but describes the private context layout, so it
// reaches the host observer only — never the protocol event stream.
func (o *eventObserver) OnContextAssembly(ctx context.Context, event agent.ContextAssemblyEvent) error {
	if host, ok := o.host.(agent.ContextAssemblyObserver); ok {
		return wrapHostObserverError(host.OnContextAssembly(ctx, event))
	}
	return nil
}

// OnRetrievalPresentation forwards retrieval attribution only to opted-in host
// observers; it deliberately has no protocol event equivalent.
func (o *eventObserver) OnRetrievalPresentation(ctx context.Context, event agent.RetrievalPresentationEvent) error {
	if host, ok := o.host.(agent.RetrievalPresentationObserver); ok {
		return wrapHostObserverError(host.OnRetrievalPresentation(ctx, event))
	}
	return nil
}
