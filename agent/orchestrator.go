package agent

import (
	"context"
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
	return State{
		System: req.System,
		Messages: []Message{
			{ChatMessage: provider.ChatMessage{Role: "user", Content: req.Goal}, Segment: Pinned},
		},
	}
}

func buildChatRequest(st State, specs []provider.Tool) provider.ChatRequest {
	msgs := make([]provider.ChatMessage, 0, len(st.Messages)+1)
	if st.System != "" {
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: st.System})
	}
	for _, m := range st.Messages {
		msgs = append(msgs, m.ChatMessage)
	}
	return provider.ChatRequest{Messages: msgs, Tools: specs, Stream: true}
}

// Run executes the loop until the model produces a final answer or a cap is hit.
func (o *Orchestrator) Run(ctx context.Context, req Request, obs Observer) (Result, error) {
	obs = normalizeObserver(obs)
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
	gov := &restraintGovernor{} // per-Run loop state; never shared across Run calls
	var res Result

	for step := 0; step < maxSteps; step++ {
		assembled, pressure, err := o.ctxMgr.Assemble(ctx, state, toolSchemaTokens, budget)
		if err != nil {
			return res, err
		}

		modelResult, err := o.model.Chat(ctx, buildChatRequest(assembled, specs), func(c provider.ChatResponse) error {
			if c.Content == "" {
				return nil
			}
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "token"})
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
			res.Answer = resp.Content
			res.StopReason = Completed
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}

		state.Messages = append(state.Messages, assistantMessage(resp))
		if err := o.runToolCalls(ctx, &res, &state, reg, resp.ToolCalls, req.Approver, obs, step, gov); err != nil {
			return res, err
		}
		if res.StopReason == ToolErrorCapReached || res.StopReason == BudgetReached {
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
		if req.Budget.TotalTokens > 0 && res.Usage.TotalTokens >= req.Budget.TotalTokens {
			res.StopReason = BudgetReached
			res.Events = append(res.Events, EventRecord{Step: step, Kind: "stop"})
			return res, nil
		}
	}
	res.StopReason = StepCapReached
	res.Events = append(res.Events, EventRecord{Step: maxSteps, Kind: "stop"})
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
type restraintGovernor struct {
	consecutiveErrors int
	lastSig           string
	repeatCount       int
}

func (g *restraintGovernor) observe(call provider.ToolCall, out ToolResult) {
	if out.IsError {
		g.consecutiveErrors++
	} else {
		g.consecutiveErrors = 0
	}
	sig := call.Function.Name + "\x00" + string(call.Function.Arguments)
	if sig == g.lastSig {
		g.repeatCount++
	} else {
		g.lastSig, g.repeatCount = sig, 1
	}
}

func (g *restraintGovernor) tripped() bool {
	return g.consecutiveErrors >= defaultToolErrorCap || g.repeatCount >= defaultToolErrorCap
}
