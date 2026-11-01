package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/provider"
)

// Orchestrator runs the plan->act->observe loop. It must not be copied after use.
type Orchestrator struct {
	model     ModelCaller
	ctxMgr    ContextManager
	now       func() time.Time // wall clock for latency; time.Now unless overridden
	toolLimit ToolInvocationLimit
	// verifier is the optional post-write verification hook (#347). nil is the
	// default and leaves every batch byte-for-byte unchanged.
	verifierMu sync.RWMutex
	verifier   Verifier
	// interceptors is the deterministic middleware chain (#436), in
	// registration order. Empty leaves every hook a no-op.
	interceptors []Interceptor
}

// Option configures an Orchestrator at construction. New stays source-compatible
// for callers that pass none (per feedback_additive_constructors).
type Option func(*Orchestrator)

// WithClock overrides the wall clock used for step/tool latency measurement. A
// nil now is ignored so the default time.Now is preserved. Tests inject a fake
// clock for deterministic latencies.
func WithClock(now func() time.Time) Option {
	return func(o *Orchestrator) {
		if now != nil {
			o.now = now
		}
	}
}

// WithToolInvocationLimit caps actual Invoke calls for one named tool in each
// Run. Synthetic failures do not count, and the zero value disables the cap.
// A non-zero limit must name a tool registered in every Run's Request.Tools;
// a Run whose tool set omits it fails fast instead of silently ignoring the cap.
func WithToolInvocationLimit(limit ToolInvocationLimit) Option {
	return func(o *Orchestrator) { o.toolLimit = limit }
}

// New constructs an Orchestrator from a ModelCaller and ContextManager.
//
// The manager is stored UNNORMALIZED. Installing the default compactor here
// would make every mixed manager look like it carries a custom one, and mixed
// assembly rejects that combination (ErrMixedCompactor). Legacy behavior is
// unchanged: assembleLegacy applies the same default at the same effective
// point.
func New(model ModelCaller, ctxMgr ContextManager, opts ...Option) *Orchestrator {
	o := &Orchestrator{model: model, ctxMgr: ctxMgr, now: time.Now}
	// #430: this orchestrator's request builder frames every tool
	// observation, so its assembly charges the frame envelope (spec D5).
	o.ctxMgr.frameToolResults = true
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Verifier returns the currently installed hook, or nil. It is safe to call
// concurrently with WithVerifier and Run. To validate and run the same hook,
// pass this captured value to RunWithVerifier.
func (o *Orchestrator) Verifier() Verifier {
	o.verifierMu.RLock()
	defer o.verifierMu.RUnlock()
	return o.verifier
}

func initState(req Request) State {
	msgs := make([]Message, 0, len(req.History)+1)
	for _, h := range req.History {
		msgs = append(msgs, Message{ChatMessage: cloneChatMessage(h), Segment: Elastic})
	}
	msgs = append(msgs, Message{
		ChatMessage: provider.ChatMessage{Role: "user", Content: req.Goal},
		Segment:     Pinned,
	})
	return State{System: req.System, DurableSummary: req.HistorySummary, Messages: msgs, systemKeys: slices.Clone(req.SystemKeys)}
}

// visibleState is the assembly input of a low-water Run: st without the
// messages hidden[i] marks, each kept message tagged with its canonical
// index plus one (Message.idx). hidden is aligned to st.Messages and may be
// shorter; a message past its end is visible. The other fields are st's.
func visibleState(st State, hidden []bool) State {
	out := st
	out.Messages = make([]Message, 0, len(st.Messages))
	for i, m := range st.Messages {
		if i < len(hidden) && hidden[i] {
			continue
		}
		m.idx = i + 1
		out.Messages = append(out.Messages, m)
	}
	return out
}

// hideEvicted marks each Elastic message of the assembly input in that
// assembled left out, so it stays out of the Run's later requests. Pinned
// messages are never hidden, and no built-in assembly leaves out an
// unresolved tool tail. hidden grows to cover n, len(State.Messages).
func hideEvicted(hidden []bool, in, assembled State, n int) []bool {
	kept := make(map[int]bool, len(assembled.Messages))
	for _, m := range assembled.Messages {
		kept[m.idx] = true
	}
	if len(hidden) < n {
		hidden = append(hidden, make([]bool, n-len(hidden))...)
	}
	for _, m := range in.Messages {
		if m.Segment == Elastic && !kept[m.idx] {
			hidden[m.idx-1] = true
		}
	}
	return hidden
}

func buildChatRequest(st State, specs []provider.Tool, outputReserve int, opts provider.ModelOptions, advisory *Advisory, advisoryFence promptfence.Fence) provider.ChatRequest {
	msgs := make([]provider.ChatMessage, 0, len(st.Messages)+1)
	if st.System != "" {
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: st.System})
	}
	// #430: every tool observation is framed here, on the value copy, after
	// assembly, under the key mintFenceKeys stored on its message. The key is
	// the same on every render, so it never changes an earlier observation's
	// wire bytes. State keeps the raw bytes. A tool message with no key (a
	// custom Compactor rebuilt it) is framed under one key minted for this
	// render, shared by every unkeyed message in it.
	var unkeyed promptfence.Fence
	for _, m := range st.Messages {
		cm := m.ChatMessage
		if cm.Role == "tool" {
			fence := m.fence
			if fence == (promptfence.Fence{}) {
				if unkeyed == (promptfence.Fence{}) {
					unkeyed = promptfence.New()
				}
				fence = unkeyed
			}
			cm.Content = frameToolResult(fence, cm.Content)
		}
		// #382: the staged advisory is projected here too, on the same value
		// copy, under the Run's advisory key, which no tool message shares.
		// State keeps the raw goal. A zero key would render an empty, forgeable
		// marker, so it falls back to a key minted for this render.
		if advisory != nil && m.Segment == Pinned && cm.Role == "user" {
			if advisoryFence == (promptfence.Fence{}) {
				advisoryFence = promptfence.New()
			}
			cm.Content = renderAdvisory(advisoryFence, cm.Content, *advisory)
			advisory = nil
		}
		msgs = append(msgs, cm)
	}
	req := provider.ChatRequest{Messages: msgs, Tools: specs, Stream: true, Options: opts}
	if outputReserve > 0 {
		req.Options.NumPredict = outputReserve
	}
	return req
}

// Run executes the loop until the model produces a final answer or a cap is
// hit. Result.Risk is published on every return path, including blocks and
// errors. Nested Runs using the supplied context inherit capacities and finite
// token allowances. Returning seals that scope and cancels its context; callers
// must join nested work before returning to receive complete descendant usage.
func (o *Orchestrator) Run(ctx context.Context, req Request, obs Observer) (Result, error) {
	return o.RunWithVerifier(ctx, req, obs, o.Verifier())
}

// RunWithVerifier runs with exactly the supplied verification hook for every
// batch, including nil to disable verification. It never reads the installed
// hook, allowing a caller to validate a captured value before running it.
// The hook's internal state remains its own responsibility.
func (o *Orchestrator) RunWithVerifier(ctx context.Context, req Request, obs Observer, verifier Verifier) (res Result, err error) {
	ctx, req, budgetRun := newRunBudget(ctx, req)
	defer func() { res.DescendantUsage = budgetRun.close() }()
	ic := &interceptorRun{}
	res, err = o.run(ctx, req, obs, ic, budgetRun, verifier)
	res.Risk = ic.result()
	return res, err
}

func (o *Orchestrator) run(ctx context.Context, req Request, obs Observer, ic *interceptorRun, budgetRun *runBudget, verifier Verifier) (Result, error) {
	obs = normalizeObserver(obs)
	if req.Goal == "" {
		return Result{}, fmt.Errorf("agent: empty goal")
	}
	if v := o.ctxMgr.CompactionLowWater; v != 0 && !(v > 0 && v < 1) {
		return Result{}, fmt.Errorf("agent: compaction low-water %v outside (0,1)", v)
	}
	if err := validateHistory(req.History); err != nil {
		return Result{}, err
	}
	if req.Advisory != nil {
		if err := ValidateAdvisory(req.Advisory); err != nil {
			return Result{}, err
		}
	}
	if err := validateSystemKeys(req.SystemKeys); err != nil {
		return Result{}, err
	}
	maxSteps := req.MaxSteps
	reg, err := newToolRegistry(req.Tools)
	if err != nil {
		return Result{}, err
	}
	specs := reg.providerSpecs()
	toolSchemaTokens := o.ctxMgr.estimate(toolSchemaString(specs))
	budget := turnBudget(req.Budget)

	gov, err := newRestraintGovernor(o.toolLimit, reg)
	if err != nil {
		return Result{}, err
	}
	// #436: resolve per-run interceptors before initState so a system-prompt
	// addendum lands in the state the model sees.
	chain, addenda, err := resolveInterceptors(ctx, o.interceptors, RunScope{System: req.System})
	if err != nil {
		return Result{}, err
	}
	ic.chain = chain
	// #430 spec D3: the effective system prompt is caller text, then the base
	// trust contract, then interceptor addenda in chain order. Composed once
	// per run, before initState, so initial inspection and fitting both see
	// it; RunScope above carried the caller's text alone.
	req.System = withToolTrustContract(req.System) + addenda
	state := initState(req)
	historyLen := len(req.History)
	// Every return path reports the keys refreshFenceKeys last rotated in place.
	res := Result{SystemKeys: state.systemKeys}

	// #382: the staged advisory is re-inspected before the initial input, as
	// its own model-origin observation: a block refuses the run before any
	// assembly, and tags annotate the projected copy only.
	var advisory *Advisory
	if req.Advisory != nil {
		last := len(state.Messages) - 1
		annotated, aerr := o.prepareAdvisory(ctx, ic, obs, *req.Advisory, last)
		if aerr != nil {
			return finishWithError(&res, state, historyLen, aerr)
		}
		advisory = &annotated
	}
	// One advisory key per Run, reused on every render like a tool key.
	advisoryFence := promptfence.New()
	var liveAdvisory *promptfence.Fence
	if advisory != nil {
		liveAdvisory = &advisoryFence
	}

	// #436: the initial input is inspected once, before any assembly, so a
	// block never reaches the model and no pressure event is emitted for it.
	if err := ic.inspectInitial(ctx, obs, &state); err != nil {
		return finishWithError(&res, state, historyLen, err)
	}
	goal := state.Messages[len(state.Messages)-1].Content

	next := 0            // first State index refreshFenceKeys has not visited
	scanSafeThrough := 0 // messages older than a scan-limit key reset
	// Sticky low-water eviction (CompactionLowWater): what a compacting step
	// dropped stays out of later assemblies. Only the assembly input is
	// filtered; fence keys and Result.Messages keep using canonical state.
	sticky := o.ctxMgr.stickyEviction()
	var hidden []bool // aligned to state.Messages
	for step := 0; step < maxSteps; step++ {
		// Rotate every live key the previous step's text exposed, then key
		// the tool messages it appended, now that verifyBatch has finished
		// with them. Step 0 also scans the summary and staged advice for
		// advisory and system keys; history carries no tool messages.
		var rotated int
		var limited bool
		var preRotated map[int]bool
		var pinned []string
		if step == 0 {
			pinned = append(pinned, state.DurableSummary)
			if advisory != nil {
				pinned = append(pinned, advisory.Source, advisory.Tool, advisory.Model, advisory.Digest, advisory.Content, advisory.Annotation)
			}
		}
		next, rotated, limited, preRotated = refreshFenceKeys(&state, liveAdvisory, next, pinned...)
		if limited {
			scanSafeThrough = len(state.Messages)
		}
		in := state
		if sticky {
			in = visibleState(state, hidden)
		}
		manager := o.ctxMgr
		if advisory != nil {
			estimate := manager.estimate
			if manager.Mixed && hasStructuredAnchor(in) {
				estimate = guardedEstimate(manager.Estimate)
			}
			projected := renderAdvisoryLines(advisoryPlaceholderOpen, advisoryPlaceholderClose, goal, *advisory)
			extra, costOK := checkedTokenSub(estimate(projected), estimate(goal))
			if !costOK {
				return res, ErrContextExhausted
			}
			// Never create spare capacity from a non-monotonic estimator.
			manager.advisoryTokens = max(0, extra)
		}
		assembled, pressure, atrace, err := manager.AssembleWithTrace(ctx, in, toolSchemaTokens, budget)
		// Emit pressure before the model call on the success path and on the
		// exhaustion path; skip only opaque compactor failures (pressure is zero).
		if err == nil || errors.Is(err, ErrContextExhausted) {
			if po, ok := obs.(PressureObserver); ok {
				if perr := po.OnPressure(ctx, PressureEvent{Step: step, Pressure: pressure}); perr != nil {
					return res, perr
				}
			}
		}
		if err != nil {
			return res, err
		}
		if sticky && pressure.Compactions > 0 {
			hidden = hideEvicted(hidden, in, assembled, len(state.Messages))
		}
		if atrace.Subjects != nil {
			n, postLimited := refreshMaterializedFenceKeys(&state, &assembled, liveAdvisory, scanSafeThrough, preRotated)
			rotated += n
			if postLimited {
				scanSafeThrough = len(state.Messages)
				limited = true
			}
		}
		if advisory != nil {
			goals := 0
			for _, m := range assembled.Messages {
				if m.Segment == Pinned && m.Role == "user" {
					if m.Content != goal {
						return res, errors.New("agent: compactor changed the advisory's pinned goal")
					}
					goals++
				}
			}
			if goals != 1 {
				return res, errors.New("agent: compactor must preserve the advisory's pinned goal exactly once")
			}
		}
		// Mixed assemblies only. The discriminator is nil Subjects, not Mixed and
		// not a length: legacy, no-anchor and error paths return a zero trace,
		// while a successful mixed assembly always carries a non-nil slice — even
		// the all-omitted one an operator most wants to see, and even a zero-ROW
		// one (TestMixedTraceWithNoRowsIsStillNonNil), which is why length is the
		// wrong test.
		if cao, ok := obs.(ContextAssemblyObserver); ok && atrace.Subjects != nil {
			if aerr := cao.OnContextAssembly(ctx, ContextAssemblyEvent{Step: step, Trace: atrace}); aerr != nil {
				return res, aerr
			}
		}
		if rpo, ok := obs.(RetrievalPresentationObserver); ok {
			for _, msg := range assembled.Messages {
				if msg.ToolCallID == "" || msg.Attrib == nil || len(msg.Attrib.Sources) == 0 {
					continue
				}
				if rerr := rpo.OnRetrievalPresentation(ctx, retrievalPresentationEvent(step, msg)); rerr != nil {
					return res, rerr
				}
			}
		}
		for range rotated {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "fence_rotation"})
		}
		if limited {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "fence_scan_limit"})
		}
		if pressure.Compactions > 0 {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "compaction"})
		}

		tokenLogged := false
		modelStart := o.now()
		chatReq := buildChatRequest(assembled, specs, req.Budget.OutputReserve, req.Options, advisory, advisoryFence)
		// Session identity belongs to the run, independent of transcript rebuilding.
		chatReq.SessionID = req.SessionID
		// Both assembly modes report the checked, complete prompt cost. Spend
		// admission happens afterward, without changing static context capacity.
		reservation, err := budgetRun.reserve(ctx, pressure.InputTokens)
		if err != nil {
			res.Messages = resultMessages(state, historyLen) // as on a cancelled Chat
			if !errors.Is(err, errRunBudgetExhausted) {
				return res, err
			}
			res.StopReason = BudgetReached
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
		modelResult, err := o.model.Chat(ctx, chatReq, func(c provider.ChatResponse) error {
			if c.Thinking != "" {
				if to, ok := obs.(ThinkingObserver); ok {
					if terr := to.OnThinking(ctx, ThinkingEvent{Step: step, Content: c.Thinking}); terr != nil {
						return terr
					}
				}
			}
			if c.Content == "" {
				return nil
			}
			if !tokenLogged {
				res.Events = append(res.Events, EventRecord{Step: step, Kind: "token"})
				tokenLogged = true
			}
			return obs.OnToken(ctx, TokenEvent{Step: step, Content: c.Content})
		})
		reservation.settle(modelResult, err)
		modelLatency := o.now().Sub(modelStart)
		if err != nil {
			// A failed stream may still return collected text. Preserve only that
			// text; any tool-call fragments may be incomplete. #436: the partial
			// content and thinking are inspected before they are appended or
			// returned; a block is joined with the provider error.
			// #424 D4: a refused request was built from the copied messages, so
			// their copy keeps no field that carries protected text, on either
			// return below.
			denied := errors.Is(err, provider.ErrContentDenied) && ic.guardsText()
			partial := modelResult.Response
			if strings.TrimSpace(partial.Content) != "" || strings.TrimSpace(partial.Thinking) != "" {
				partial.ToolCalls = nil
				if ierr := ic.inspectOutput(ctx, obs, step, partial); ierr != nil {
					out, ferr := finishWithError(&res, state, historyLen, errors.Join(ierr, err))
					if denied {
						suppressSensitive(out.Messages, ic.sensitiveText)
					}
					return out, ferr
				}
			}
			if strings.TrimSpace(partial.Content) != "" {
				state.Messages = append(state.Messages, Message{
					ChatMessage: provider.ChatMessage{Role: "assistant", Content: partial.Content},
					Segment:     Elastic,
				})
			}
			res.Messages = resultMessages(state, historyLen)
			if denied {
				suppressSensitive(res.Messages, ic.sensitiveText)
			}
			return res, err
		}
		resp := modelResult.Response
		res.Usage = addUsage(res.Usage, resp.Usage)

		// #436: inspect before the turn is recorded or published. A blocked
		// response leaves a redacted StepRecord (usage and route only), never
		// reaches OnStep, and dispatches nothing. An interceptor error without
		// a block takes the same path: the response was never cleared.
		if oerr := ic.inspectOutput(ctx, obs, step, resp); oerr != nil {
			// #424 D3: the route is copied metadata, so it is kept only when no
			// string in it, Reason and attempts included, carries protected
			// text. An outcome that cannot be encoded cannot be checked.
			route := modelResult.RouteOutcome
			if route != nil && ic.guardsText() {
				if b, merr := json.Marshal(route); merr != nil || secretscan.MatchJSON(b, ic.sensitiveText) {
					route = nil
				}
			}
			res.Steps = append(res.Steps, StepRecord{
				Index: step, Response: provider.ChatResponse{Usage: resp.Usage, Done: resp.Done},
				RouteOutcome: route, Pressure: pressure, Latency: modelLatency,
			})
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "step"})
			return finishWithError(&res, state, historyLen, oerr)
		}

		res.Steps = append(res.Steps, StepRecord{
			Index: step, Response: resp, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure, Latency: modelLatency,
		})
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "step"})
		// #436 spec D6: the observer gets its own copy of the tool calls, so a
		// scribble there cannot change what the tool-call gate and Invoke see.
		published := resp
		published.ToolCalls = cloneToolCalls(resp.ToolCalls)
		if err := obs.OnStep(ctx, StepEvent{
			Index: step, Response: published, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure, Latency: modelLatency,
		}); err != nil {
			return res, err
		}

		if budgetRun.stopped() {
			if err := ctx.Err(); err != nil {
				res.Messages = resultMessages(state, historyLen)
				return res, err
			}
			// Keep accepted text, but never persist unexecuted tool-call residue.
			if resp.Content != "" {
				accepted := resp
				accepted.ToolCalls = nil
				state.Messages = append(state.Messages, assistantMessage(accepted))
			}
			res.Answer = resp.Content
			res.StopReason = BudgetReached
			res.Messages = resultMessages(state, historyLen)
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
		if len(resp.ToolCalls) == 0 {
			state.Messages = append(state.Messages, assistantMessage(resp))
			res.Answer = resp.Content
			res.Messages = resultMessages(state, historyLen)
			res.StopReason = Completed
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}

		state.Messages = append(state.Messages, assistantMessage(resp))
		if err := o.runToolCalls(ctx, &res, &state, reg, resp.ToolCalls, req.Approver, obs, step, gov, ic, verifier); err != nil {
			return finishWithError(&res, state, historyLen, err)
		}
		if res.StopReason != Completed {
			res.Messages = resultMessages(state, historyLen)
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
		if budgetRun.stopped() {
			res.StopReason = BudgetReached
			res.Messages = resultMessages(state, historyLen)
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
	}
	res.StopReason = StepCapReached
	res.Messages = resultMessages(state, historyLen)
	res.Events = append(res.Events, EventRecord{Step: maxSteps - 1, Kind: "stop"})
	return res, nil
}

// prepareAdvisory runs the observation hook over the advisory as a
// model-origin observation. A block refuses the advisory with
// ErrAdvisoryBlocked joined to the *BlockedError naming the rule; tags are
// SET on Annotation, never appended to Content, so Digest keeps labelling the
// admitted answer and re-inspecting an already-annotated receipt replaces its
// trailers instead of stacking a second copy. With no interceptors installed
// the receipt is returned unchanged.
//
// Role "tool" is the observation hook's contract for tool-authored text, not
// a wire role: nothing here ever becomes a role "tool" message. It is what
// makes the origin-sensitive detectors judge the advice the way they judge a
// tool result.
func (o *Orchestrator) prepareAdvisory(ctx context.Context, ic *interceptorRun, obs Observer, a Advisory, index int) (Advisory, error) {
	tags, block, sensitive, err := ic.inspectObservation(ctx, normalizeObserver(obs), 0, InspectedMessage{
		StateIndex: index, Role: "tool", Origin: a.Origin,
		ToolName: "consult/" + a.Source, Content: a.Content,
	})
	if err != nil {
		return Advisory{}, err
	}
	if block != nil {
		// Both facets travel: errors.Is finds the advisory sentinel a consult
		// consumer switches on, errors.As finds the structured block every
		// other interceptor refusal in this package carries, so the refusing
		// rule is nameable without re-running the chain.
		return Advisory{}, errors.Join(ErrAdvisoryBlocked,
			&BlockedError{Hook: HookInput, Step: 0, Findings: []Finding{*block}, sensitive: sensitive})
	}
	// distinctTrailers leads with a newline so it can be appended to a tool
	// result; here it is a block of whole lines, so the lead is dropped.
	annotation := strings.TrimPrefix(distinctTrailers(tags), "\n")
	// The trailer block is generated, so it bypasses ValidateAdvisory. Its
	// only unbounded input is the chain's distinct rule count, and a chain
	// that produces more trailers than the receipt may carry has outgrown
	// this seam: fail closed rather than ship a receipt whose own annotation
	// crowds out the advice. Not a policy refusal, so not ErrAdvisoryBlocked.
	if len(annotation) > maxAdvisoryAnnotation {
		return Advisory{}, fmt.Errorf("agent: advisory annotation exceeds %d bytes", maxAdvisoryAnnotation)
	}
	a.Annotation = annotation
	return a, nil
}

// InspectAdvisory applies this Orchestrator's interceptor chain to a consult
// receipt before it is displayed or staged, so a refusal reaches the user at
// consult time rather than at the next Run. It returns the annotated receipt,
// or a refusal satisfying both errors.Is(err, ErrAdvisoryBlocked) and
// errors.As(err, **BlockedError).
//
// It is a preview, not the authoritative check. The chain is resolved against
// an empty RunScope, so a RunScopedInterceptor sees no system prompt and any
// addendum it offers is discarded, and the findings are not published on any
// RiskReport. Run re-inspects the staged receipt at step 0 under the real
// scope, and that inspection is the one that gates the model call and lands
// on Result.Risk.
func (o *Orchestrator) InspectAdvisory(ctx context.Context, a Advisory) (Advisory, error) {
	if err := ValidateAdvisory(&a); err != nil {
		return Advisory{}, err
	}
	chain, _, err := resolveInterceptors(ctx, o.interceptors, RunScope{})
	if err != nil {
		return Advisory{}, err
	}
	return o.prepareAdvisory(ctx, &interceptorRun{chain: chain}, nil, a, 0)
}

func assistantMessage(resp provider.ChatResponse) Message {
	return Message{ChatMessage: provider.ChatMessage{
		Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls,
	}, Segment: Elastic}
}

// addUsage sums a and b's token counts. The cache counters are summed too and
// their Reported flags are ORed, so a run that mixes a reporting and a
// non-reporting backend still shows what was measured. Reasoning tokens are
// not accumulated.
func addUsage(a, b provider.Usage) provider.Usage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	a.CachedPromptTokens += b.CachedPromptTokens
	a.CachedPromptTokensReported = a.CachedPromptTokensReported || b.CachedPromptTokensReported
	a.CacheWriteTokens += b.CacheWriteTokens
	a.CacheWriteTokensReported = a.CacheWriteTokensReported || b.CacheWriteTokensReported
	return a
}

func resultMessages(st State, historyLen int) []provider.ChatMessage {
	if historyLen >= len(st.Messages) {
		return nil
	}
	out := make([]provider.ChatMessage, 0, len(st.Messages)-historyLen)
	for _, m := range st.Messages[historyLen:] {
		// #382: ChatMessage only -- Message.Advisory must never leave the run.
		out = append(out, cloneChatMessage(m.ChatMessage))
	}
	return out
}

// suppressedField replaces a copied field that carries protected text
// (#424); a tool call's arguments become this text as a JSON string.
const suppressedField = "[...]"

// suppressSensitive replaces wholesale every field of msgs that match
// accepts: content, tool name and call ID, and each tool call's ID, type,
// name and arguments. Arguments are checked as the outbound guard checks
// them (secretscan.MatchToolArguments): raw, decoded, and leniently unescaped
// when malformed. msgs must be an owned copy such as resultMessages returns;
// nothing is rewritten in place, so state and the caller's slices keep their
// values.
func suppressSensitive(msgs []provider.ChatMessage, match func(string) bool) {
	suppress := func(s *string) {
		if match(*s) {
			*s = suppressedField
		}
	}
	for i := range msgs {
		m := &msgs[i]
		suppress(&m.Content)
		suppress(&m.ToolName)
		suppress(&m.ToolCallID)
		for j := range m.ToolCalls {
			c := &m.ToolCalls[j]
			suppress(&c.ID)
			suppress(&c.Type)
			suppress(&c.Function.Name)
			if secretscan.MatchToolArguments(c.Function.Arguments, match) {
				c.Function.Arguments = json.RawMessage(`"` + suppressedField + `"`)
			}
		}
	}
}

func toolSchemaString(specs []provider.Tool) string {
	var b strings.Builder
	for _, sp := range specs {
		b.WriteString(sp.Function.Name)
		b.WriteString(sp.Function.Description)
		b.Write(sp.Function.Parameters)
	}
	return b.String()
}

// restraintGovernor bounds runaway loops with a weak local model. Per-Run state.
// It stops a run on too many consecutive tool errors, or on a tool call that
// repeats with no progress (identical name+args AND identical result).
type restraintGovernor struct {
	consecutiveErrors int
	lastSig           uint64
	hasSig            bool
	repeatCount       int
	invocationLimit   ToolInvocationLimit
	invocations       int
}

func newRestraintGovernor(limit ToolInvocationLimit, reg *toolRegistry) (*restraintGovernor, error) {
	g := &restraintGovernor{invocationLimit: limit}
	if limit == (ToolInvocationLimit{}) {
		return g, nil
	}
	if limit.Tool == "" {
		return nil, fmt.Errorf("agent: tool invocation budget has an empty tool name")
	}
	if limit.Max <= 0 {
		return nil, fmt.Errorf("agent: tool invocation budget %q must be positive, got %d", limit.Tool, limit.Max)
	}
	if _, ok := reg.lookup(limit.Tool); !ok {
		return nil, fmt.Errorf("agent: tool invocation budget names unregistered tool %q", limit.Tool)
	}
	return g, nil
}

func (g *restraintGovernor) reserveInvocation(name string) (int, bool) {
	if name != g.invocationLimit.Tool {
		return 0, true
	}
	if g.invocations >= g.invocationLimit.Max {
		return g.invocationLimit.Max, false
	}
	g.invocations++
	return g.invocationLimit.Max, true
}

func (g *restraintGovernor) parallelUncapped(calls []provider.ToolCall) bool {
	for _, call := range calls {
		if call.Function.Name == g.invocationLimit.Tool {
			return false
		}
	}
	return true
}

// toolCallSignature hashes the call identity together with its result so that a
// repeated call whose result CHANGES (e.g. polling that makes progress) is not
// mistaken for a stuck loop. Only identical call + identical result repeats.
func toolCallSignature(call provider.ToolCall, out ToolResult) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(call.Function.Name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(call.Function.Arguments)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(out.Content))
	return h.Sum64()
}

func (g *restraintGovernor) observe(call provider.ToolCall, out ToolResult) {
	if out.IsError {
		g.consecutiveErrors++
	} else {
		g.consecutiveErrors = 0
	}
	sig := toolCallSignature(call, out)
	if g.hasSig && sig == g.lastSig {
		g.repeatCount++
	} else {
		g.lastSig, g.hasSig, g.repeatCount = sig, true, 1
	}
}

// stopReason reports the cap the governor hit, if any. Consecutive tool errors
// take precedence over no-progress repeats. The bool is false when not tripped.
func (g *restraintGovernor) stopReason() (StopReason, bool) {
	if g.consecutiveErrors >= defaultToolErrorCap {
		return ToolErrorCapReached, true
	}
	if g.repeatCount >= defaultToolErrorCap {
		return RepeatLimitReached, true
	}
	return Completed, false
}
