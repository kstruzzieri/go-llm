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
		out, effect, rec, err := o.dispatch(ctx, reg, call, approver, obs, step)
		if err != nil {
			return err // hard abort (ctx cancel / approver error): no ToolResult, no OnToolResult
		}
		stop, err := o.recordResult(ctx, res, state, obs, gov, step, call, effect, rec, out)
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
// drift. `out` is the capped canonical fallback immediately after execution
// (synthetic failures are short and uncapped). The observer sees it before its
// Context is cloned onto State and before mixed assembly, so it is not promised
// byte-identical to the model's later input. It returns stop=true (and sets
// res.StopReason) when the governor trips, or an error to hard-abort the run.
// It is a method so the mixed-assembly flag is read from o.ctxMgr in ONE place:
// a flag threaded from each caller could drift between the two paths.
func (o *Orchestrator) recordResult(ctx context.Context, res *Result, state *State, obs Observer,
	gov *restraintGovernor, step int, call provider.ToolCall, effect Effect, rec ToolCallRecord,
	out ToolResult) (stop bool, err error) {

	rec.RouteOutcome = out.RouteOutcome
	res.ToolCalls = append(res.ToolCalls, rec)
	res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_result"})
	if tro, ok := obs.(ToolResultObserver); ok {
		if err := tro.OnToolResult(ctx, ToolResultEvent{
			Step: step, Call: call, Effect: effect, Result: out,
			Denied: rec.Denied, Invoked: rec.Invoked, Latency: rec.Latency,
		}); err != nil {
			return false, err
		}
	}
	if o.ctxMgr.Mixed {
		if err := validateContextSetCardinality(call.ID, out.Context); err != nil {
			return false, err
		}
	}
	msg := toolObservation(call, out)
	msg.OutputCap = effect.OutputCap
	// The structured payload is deep-copied only when mixed assembly is on:
	// with Mixed off nothing reads it, so cloning would make State retain a
	// guaranteed-dead copy of every alternative for the rest of the run.
	if o.ctxMgr.Mixed {
		msg.Context = out.Context.clone() // clone is nil-safe
	}
	state.Messages = append(state.Messages, msg)
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
	p.effect = effect // capture now so a denial (which returns before invoke) still carries the effect

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

	p.tool = tool
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
	approver Approver, obs Observer, step int) (ToolResult, Effect, ToolCallRecord, error) {

	p, err := o.prepareCall(ctx, reg, call, approver, obs, step)
	if err != nil {
		return ToolResult{}, p.effect, p.rec, err
	}
	if p.result != nil {
		return *p.result, p.effect, p.rec, nil
	}
	start := o.now()
	out := o.invokeCall(ctx, p.tool, p.effect, call.Function.Arguments)
	p.rec.Invoked = true
	p.rec.Latency = o.now().Sub(start)
	if ctx.Err() != nil {
		return ToolResult{}, p.effect, p.rec, ctx.Err() // parent cancelled -> hard abort
	}
	p.rec.IsError = out.IsError
	return out, p.effect, p.rec, nil
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
