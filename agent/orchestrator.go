package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// Orchestrator runs the plan->act->observe loop.
type Orchestrator struct {
	model  ModelCaller
	ctxMgr ContextManager
}

// New constructs an Orchestrator from a ModelCaller and ContextManager.
func New(model ModelCaller, ctxMgr ContextManager) *Orchestrator {
	return &Orchestrator{model: model, ctxMgr: normalizeContextManager(ctxMgr)}
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
	return State{System: req.System, Messages: msgs}
}

func buildChatRequest(st State, specs []provider.Tool, outputReserve int) provider.ChatRequest {
	msgs := make([]provider.ChatMessage, 0, len(st.Messages)+1)
	if st.System != "" {
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: st.System})
	}
	for _, m := range st.Messages {
		msgs = append(msgs, m.ChatMessage)
	}
	req := provider.ChatRequest{Messages: msgs, Tools: specs, Stream: true}
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

	state := initState(req)
	historyLen := len(req.History)
	gov := &restraintGovernor{} // per-Run loop state; never shared across Run calls
	var res Result

	for step := 0; step < maxSteps; step++ {
		assembled, pressure, err := o.ctxMgr.Assemble(ctx, state, toolSchemaTokens, budget)
		if err != nil {
			return res, err
		}
		if pressure.Compactions > 0 {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "compaction"})
		}

		tokenLogged := false
		modelResult, err := o.model.Chat(ctx, buildChatRequest(assembled, specs, req.Budget.OutputReserve), func(c provider.ChatResponse) error {
			if c.Content == "" {
				return nil
			}
			if !tokenLogged {
				res.Events = append(res.Events, EventRecord{Step: step, Kind: "token"})
				tokenLogged = true
			}
			return obs.OnToken(ctx, TokenEvent{Step: step, Content: c.Content})
		})
		if err != nil {
			return res, err
		}
		resp := modelResult.Response

		res.Steps = append(res.Steps, StepRecord{
			Index: step, Response: resp, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure,
		})
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "step"})
		res.Usage = addUsage(res.Usage, resp.Usage)
		if err := obs.OnStep(ctx, StepEvent{
			Index: step, Response: resp, RouteOutcome: modelResult.RouteOutcome, Pressure: pressure,
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
