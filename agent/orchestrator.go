package agent

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Orchestrator runs the plan->act->observe loop.
type Orchestrator struct {
	model     ModelCaller
	ctxMgr    ContextManager
	now       func() time.Time // wall clock for latency; time.Now unless overridden
	toolLimit ToolInvocationLimit
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
	for _, opt := range opts {
		opt(o)
	}
	return o
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
	return State{System: req.System, DurableSummary: req.HistorySummary, Messages: msgs}
}

func buildChatRequest(st State, specs []provider.Tool, outputReserve int, opts provider.ModelOptions) provider.ChatRequest {
	msgs := make([]provider.ChatMessage, 0, len(st.Messages)+1)
	if st.System != "" {
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: st.System})
	}
	for _, m := range st.Messages {
		msgs = append(msgs, m.ChatMessage)
	}
	req := provider.ChatRequest{Messages: msgs, Tools: specs, Stream: true, Options: opts}
	if outputReserve > 0 {
		req.Options.NumPredict = outputReserve
	}
	return req
}

// Run executes the loop until the model produces a final answer or a cap is hit.
func (o *Orchestrator) Run(ctx context.Context, req Request, obs Observer) (Result, error) {
	obs = normalizeObserver(obs)
	if req.Goal == "" {
		return Result{}, fmt.Errorf("agent: empty goal")
	}
	if err := validateHistory(req.History); err != nil {
		return Result{}, err
	}
	maxSteps := req.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
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
	state := initState(req)
	historyLen := len(req.History)
	var res Result

	for step := 0; step < maxSteps; step++ {
		assembled, pressure, atrace, err := o.ctxMgr.AssembleWithTrace(ctx, state, toolSchemaTokens, budget)
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
		if pressure.Compactions > 0 {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "compaction"})
		}

		tokenLogged := false
		modelStart := o.now()
		modelResult, err := o.model.Chat(ctx, buildChatRequest(assembled, specs, req.Budget.OutputReserve, req.Options), func(c provider.ChatResponse) error {
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
		modelLatency := o.now().Sub(modelStart)
		if err != nil {
			res.Messages = resultMessages(state, historyLen)
			return res, err
		}
		resp := modelResult.Response

		res.Steps = append(res.Steps, StepRecord{
			Index: step, Response: resp, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure, Latency: modelLatency,
		})
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "step"})
		res.Usage = addUsage(res.Usage, resp.Usage)
		if err := obs.OnStep(ctx, StepEvent{
			Index: step, Response: resp, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure, Latency: modelLatency,
		}); err != nil {
			return res, err
		}

		if len(resp.ToolCalls) == 0 {
			state.Messages = append(state.Messages, assistantMessage(resp))
			res.Answer = resp.Content
			res.Messages = resultMessages(state, historyLen)
			if budgetExceeded(res.Usage, req.Budget) {
				res.StopReason = BudgetReached
			} else {
				res.StopReason = Completed
			}
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}

		state.Messages = append(state.Messages, assistantMessage(resp))
		if err := o.runToolCalls(ctx, &res, &state, reg, resp.ToolCalls, req.Approver, obs, step, gov); err != nil {
			return res, err
		}
		if res.StopReason != Completed {
			res.Messages = resultMessages(state, historyLen)
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
		if budgetExceeded(res.Usage, req.Budget) {
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

func assistantMessage(resp provider.ChatResponse) Message {
	return Message{ChatMessage: provider.ChatMessage{
		Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls,
	}, Segment: Elastic}
}

func addUsage(a, b provider.Usage) provider.Usage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	return a
}

func resultMessages(st State, historyLen int) []provider.ChatMessage {
	if historyLen >= len(st.Messages) {
		return nil
	}
	out := make([]provider.ChatMessage, 0, len(st.Messages)-historyLen)
	for _, m := range st.Messages[historyLen:] {
		out = append(out, cloneChatMessage(m.ChatMessage))
	}
	return out
}

func budgetExceeded(u provider.Usage, b Budget) bool {
	return b.TotalTokens > 0 && u.TotalTokens >= b.TotalTokens
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
