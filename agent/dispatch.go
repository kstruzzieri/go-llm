package agent

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
)

// runToolCalls routes a batch to the parallel path when every call is read-only
// and independent (canRunParallel); otherwise to the serial path. Single-call
// batches always go serial (no benefit, zero behavior change).
func (o *Orchestrator) runToolCalls(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor) error {

	if len(calls) >= 2 && canRunParallel(reg, calls) {
		return o.runToolCallsParallel(ctx, res, state, reg, calls, approver, obs, step, gov)
	}
	return o.runToolCallsSerial(ctx, res, state, reg, calls, approver, obs, step, gov)
}

func (o *Orchestrator) runToolCallsSerial(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor) error {

	for _, call := range calls {
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_call"})
		out, rec, err := o.dispatch(ctx, reg, call, approver, obs, step)
		if err != nil {
			return err // hard abort (ctx cancel / approver error): no ToolResult, no OnToolResult
		}
		stop, err := recordResult(ctx, res, state, obs, gov, step, call, rec, out)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// recordResult appends one completed tool call's record, result event, optional
// observer callback, state observation, and governor update — the shared tail used
// by BOTH the serial and parallel paths so their model-visible semantics cannot
// drift. `out` is the exact ToolResult appended to State (Invoke results are
// already capOutput-ed; synthetic failures are short and uncapped) — so the
// observer sees what the model sees. It returns stop=true (and sets
// res.StopReason) when the governor trips, or an error to hard-abort the run.
func recordResult(ctx context.Context, res *Result, state *State, obs Observer,
	gov *restraintGovernor, step int, call provider.ToolCall, rec ToolCallRecord,
	out ToolResult) (stop bool, err error) {

	res.ToolCalls = append(res.ToolCalls, rec)
	res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_result"})
	if tro, ok := obs.(ToolResultObserver); ok {
		if err := tro.OnToolResult(ctx, ToolResultEvent{Step: step, Call: call, Result: out}); err != nil {
			return false, err
		}
	}
	state.Messages = append(state.Messages, toolObservation(call, out))
	gov.observe(call, out)
	if sr, tripped := gov.stopReason(); tripped {
		res.StopReason = sr
		return true, nil
	}
	return false, nil
}

// preparedCall is the outcome of the serial, observer/approval phase of one tool
// call. When prepareCall returns a nil error, exactly one of two states holds:
//   - result != nil: a synthetic outcome (unknown tool / bad JSON / plan failure /
//     approval denied). The caller must NOT Invoke; use result directly.
//   - result == nil: tool and effect are populated; the caller runs invokeCall.
type preparedCall struct {
	call   provider.ToolCall
	tool   Tool
	effect Effect
	rec    ToolCallRecord
	result *ToolResult
}

// prepareCall runs everything that must stay on the main goroutine and in loop
// order: lookup, JSON validation, per-call effect/Plan, approval, and OnToolCall.
// It NEVER appends EventRecords (callers own event ordering) and NEVER Invokes.
// A non-nil error is a hard abort (ctx cancel / approver failure / observer error);
// p.rec is still returned so the caller can record it.
func (o *Orchestrator) prepareCall(ctx context.Context, reg *toolRegistry, call provider.ToolCall,
	approver Approver, obs Observer, step int) (preparedCall, error) {

	name := call.Function.Name
	p := preparedCall{call: call, rec: ToolCallRecord{Step: step, Name: name}}

	tool, ok := reg.lookup(name)
	if !ok {
		p.rec.IsError = true
		p.result = &ToolResult{IsError: true, Content: "unknown tool: " + name}
		return p, nil
	}
	if !json.Valid(call.Function.Arguments) {
		p.rec.IsError = true
		p.result = &ToolResult{IsError: true, Content: "malformed tool arguments (not valid JSON)"}
		return p, nil
	}

	effect := tool.Effect()
	var preview string
	if pt, ok := tool.(PlanningTool); ok {
		plan, err := pt.Plan(ctx, call.Function.Arguments)
		if err != nil {
			p.rec.IsError = true
			p.result = &ToolResult{IsError: true, Content: "plan failed: " + err.Error()}
			return p, nil
		}
		effect, preview = plan.Effect, plan.Preview
	}
	effect = normalizeEffect(effect)

	if needsApproval(effect.Approval, effect.Class) {
		ok, err := approve(ctx, approver, call, preview)
		if err != nil {
			return p, err // ctx cancel / approver failure propagates
		}
		if !ok {
			p.rec.IsError, p.rec.Denied = true, true
			p.result = &ToolResult{IsError: true, Content: "tool call denied by approver"}
			return p, nil
		}
	}

	if err := obs.OnToolCall(ctx, ToolCallEvent{Step: step, Call: call, Effect: effect, Preview: preview}); err != nil {
		return p, err
	}

	p.tool, p.effect = tool, effect
	return p, nil
}

// invokeCall runs the tool body: per-call timeout, Invoke, output cap. Any tool
// error (including the per-call timeout) becomes a model-visible IsError result.
// It carries NO parent-cancellation policy — callers decide whether a cancelled
// parent ctx is a hard abort.
func (o *Orchestrator) invokeCall(ctx context.Context, tool Tool, effect Effect, args json.RawMessage) ToolResult {
	cctx, cancel := context.WithTimeout(ctx, effect.Timeout)
	defer cancel()
	out, err := tool.Invoke(cctx, args)
	if err != nil {
		return ToolResult{IsError: true, Content: err.Error()}
	}
	return capOutput(out, effect.OutputCap)
}

// dispatch is the serial composition: prepare on the main goroutine, then invoke.
// A cancelled PARENT context after invoke is a hard abort (distinct from a tool's
// own per-call timeout, which stays a model-visible IsError observation).
func (o *Orchestrator) dispatch(ctx context.Context, reg *toolRegistry, call provider.ToolCall,
	approver Approver, obs Observer, step int) (ToolResult, ToolCallRecord, error) {

	p, err := o.prepareCall(ctx, reg, call, approver, obs, step)
	if err != nil {
		return ToolResult{}, p.rec, err
	}
	if p.result != nil {
		return *p.result, p.rec, nil
	}
	out := o.invokeCall(ctx, p.tool, p.effect, call.Function.Arguments)
	if ctx.Err() != nil {
		return ToolResult{}, p.rec, ctx.Err() // parent cancelled -> hard abort
	}
	p.rec.IsError = out.IsError
	return out, p.rec, nil
}

// approve applies the fail-safe: a nil approver denies any call that reaches it
// (read-only tools never reach it because needsApproval is false for them).
func approve(ctx context.Context, approver Approver, call provider.ToolCall, preview string) (bool, error) {
	if approver == nil {
		return false, nil
	}
	return approver.Approve(ctx, call, preview)
}

func capOutput(r ToolResult, limit int) ToolResult {
	if limit > 0 && len(r.Content) > limit {
		end := limit
		// back up to a UTF-8 rune boundary so we never emit a split rune
		for end > 0 && !utf8.RuneStart(r.Content[end]) {
			end--
		}
		r.Content = r.Content[:end]
		r.Truncated = true
	}
	return r
}

func toolObservation(call provider.ToolCall, r ToolResult) Message {
	return Message{
		ChatMessage: provider.ChatMessage{
			Role:       "tool",
			Content:    r.Content,
			ToolName:   call.Function.Name,
			ToolCallID: call.ID,
		},
		Segment: Elastic,
		Attrib:  r.Attrib,
	}
}
