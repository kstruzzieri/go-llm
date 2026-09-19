package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
)

// runToolCalls routes a batch to the parallel path when every call is read-only
// and independent (canRunParallel); otherwise to the serial path. Single-call
// batches always go serial (no benefit, zero behavior change).
//
// It also owns the post-batch policy (#347). The hook lives HERE, at the shared
// boundary, rather than at the bottom of the serial runner: mutating batches
// are serial today only because parallelSafe demands a strictly-Read effect and
// canRunParallel rejects planning tools, and write effect and planning
// capability are separate concepts.
func (o *Orchestrator) runToolCalls(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor, ic *interceptorRun) error {

	b := newBatch()
	var err error
	if len(calls) >= 2 && canRunParallel(reg, calls) && gov.parallelUncapped(calls) {
		err = o.runToolCallsParallel(ctx, res, state, reg, calls, approver, obs, step, gov, &b, ic)
	} else {
		err = o.runToolCallsSerial(ctx, res, state, reg, calls, approver, obs, step, gov, &b, ic)
	}
	if err != nil {
		return err
	}
	if res.StopReason != Completed {
		// The governor tripped mid-batch: the run is stopping and the model
		// gets no further turn, so the observation would never be read.
		return nil
	}
	return o.verifyBatch(ctx, state, approver, &b, obs, step, ic)
}

func (o *Orchestrator) runToolCallsSerial(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor, b *batch, ic *interceptorRun) error {

	for _, call := range calls {
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_call"})
		out, effect, rec, inspectResult, err := o.dispatch(ctx, reg, call, approver, obs, step, gov, ic)
		if err != nil {
			appendInvokedToolCallRecord(res, step, rec, out.RouteOutcome)
			return err // hard abort: preserve post-Invoke metadata, but publish no observation or OnToolResult
		}
		stop, err := o.recordResult(ctx, res, state, obs, gov, step, call, effect, rec, out, inspectResult, b, ic)
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
// a flag threaded from each caller could drift between the two paths. For the
// same reason b (the post-batch policy accumulator, #347) is updated here and
// not in either runner.
func (o *Orchestrator) recordResult(ctx context.Context, res *Result, state *State, obs Observer,
	gov *restraintGovernor, step int, call provider.ToolCall, effect Effect, rec ToolCallRecord,
	out ToolResult, inspectResult bool, b *batch, ic *interceptorRun) (stop bool, err error) {
	provenance := bytes.Clone(out.Provenance)

	if o.ctxMgr.Mixed {
		if err := validateContextSetCardinality(call.ID, out.Context); err != nil {
			appendToolCallRecord(res, step, rec, out.RouteOutcome)
			return false, err
		}
		// #436 spec D6: freeze the set into the canonical copy State will own.
		// The structured payload is deep-copied only when mixed assembly is on:
		// with Mixed off nothing reads it, so cloning would make State retain a
		// guaranteed-dead copy of every alternative for the rest of the run.
		out.Context = out.Context.clone() // clone is nil-safe
	}
	out.Attrib = cloneAttrib(out.Attrib)
	outputCap := effect.OutputCap
	// #436 spec D7: inspect tool-authored and model-derived observations at
	// ingress, BEFORE the observer, State, the batch policy and the governor.
	// Fixed orchestrator-authored outcomes (bad JSON, denial, blocked call) carry
	// no uncontrolled content and skip the hook. Alternatives are model-visible
	// only under mixed assembly, so legacy runs inspect the fallback alone.
	if rec.Invoked || inspectResult {
		msg := InspectedMessage{
			StateIndex: len(state.Messages), Role: "tool", Origin: out.Origin,
			ToolName: call.Function.Name, ToolCallID: call.ID, Content: out.Content,
		}
		if o.ctxMgr.Mixed {
			msg.Alternatives = alternativesOf(out.Context)
		}
		tags, block, err := ic.inspectObservation(ctx, obs, step, msg)
		if err != nil {
			var blocked *BlockedError
			if errors.As(err, &blocked) {
				rec.IsError, rec.Blocked = true, true
			}
			appendToolCallRecord(res, step, rec, out.RouteOutcome)
			return false, err
		}
		if block != nil {
			out = ToolResult{IsError: true, Content: blockedResultContent(*block), Origin: out.Origin, RouteOutcome: out.RouteOutcome}
			rec.IsError, rec.Blocked = true, true
		} else if len(tags) > 0 {
			outputCap = addSaturating(outputCap, annotateResult(&out, tags, o.ctxMgr.Mixed))
		}
	}

	if !rec.Blocked {
		rec.Provenance = provenance
	}
	appendToolCallRecord(res, step, rec, out.RouteOutcome)
	if tro, ok := obs.(ToolResultObserver); ok {
		// The observer gets its own copy of the final result. The set is cloned
		// only under mixed assembly, where State owns the canonical clone; in
		// legacy mode nothing reads the set, the cardinality guard did not run,
		// and a deep copy would let an untrusted tool make this path
		// arbitrarily expensive.
		published := out
		published.Provenance = bytes.Clone(rec.Provenance)
		published.Attrib = cloneAttrib(out.Attrib)
		published.RouteOutcome = cloneRouteOutcome(out.RouteOutcome)
		if o.ctxMgr.Mixed {
			published.Context = out.Context.clone()
		}
		if err := tro.OnToolResult(ctx, ToolResultEvent{
			Step: step, Call: cloneToolCall(call), Effect: cloneEffect(effect), Result: published,
			Denied: rec.Denied, Invoked: rec.Invoked, Latency: rec.Latency,
			AutoApproved: rec.AutoApproved, Blocked: rec.Blocked,
		}); err != nil {
			return false, err
		}
	}
	msg := toolObservation(call, out)
	msg.OutputCap = outputCap
	if o.ctxMgr.Mixed {
		msg.Context = out.Context // the canonical clone; State owns it now
	}
	state.Messages = append(state.Messages, msg)
	b.note(state, call, rec, out)
	gov.observe(call, out)
	if sr, tripped := gov.stopReason(); tripped {
		res.StopReason = sr
		return true, nil
	}
	return false, nil
}

func appendToolCallRecord(res *Result, step int, rec ToolCallRecord, routeOutcome *provider.RouteOutcome) {
	rec.RouteOutcome = routeOutcome
	res.ToolCalls = append(res.ToolCalls, rec)
	res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_result"})
}

func appendInvokedToolCallRecord(res *Result, step int, rec ToolCallRecord, routeOutcome *provider.RouteOutcome) {
	if rec.Invoked {
		appendToolCallRecord(res, step, rec, routeOutcome)
	}
}

// preparedCall is the outcome of the serial, observer/approval phase of one tool
// call. When prepareCall returns a nil error, exactly one of two states holds:
//   - result != nil: a synthetic outcome (unknown tool / bad JSON / interceptor
//     block / plan failure / approval denied / invocation budget exhausted). The
//     caller must NOT Invoke; use result directly.
//   - result == nil: tool and effect are populated; the caller runs invokeCall.
type preparedCall struct {
	call   provider.ToolCall
	tool   Tool
	effect Effect
	rec    ToolCallRecord
	result *ToolResult
	// inspectResult marks a non-invoked diagnostic that includes model- or
	// tool-derived text rather than a fixed orchestrator message.
	inspectResult bool
}

// prepareCall runs everything that must stay on the main goroutine and in loop
// order: lookup, JSON validation, the interceptor gate, per-call effect/Plan,
// approval, and OnToolCall. It NEVER appends EventRecords (callers own event
// ordering) and NEVER Invokes. A non-nil error is a hard abort (ctx cancel /
// approver failure / observer error / interceptor abort); p.rec is still
// returned so the caller can record it.
func (o *Orchestrator) prepareCall(ctx context.Context, reg *toolRegistry, call provider.ToolCall,
	approver Approver, obs Observer, step int, gov *restraintGovernor, ic *interceptorRun) (preparedCall, error) {

	// #436 spec D6: one canonical copy for inspection and invocation. Every
	// callback below (Plan, approver, OnToolCall) receives its own clone, so
	// nothing can change the bytes between inspection and Invoke.
	call = cloneToolCall(call)
	name := call.Function.Name
	p := preparedCall{call: call, rec: ToolCallRecord{Step: step, Name: name}}

	tool, ok := reg.lookup(name)
	if !ok {
		p.rec.IsError = true
		p.result = &ToolResult{IsError: true, Content: "unknown tool: " + name, Origin: OriginModel}
		p.inspectResult = true
		return p, nil
	}
	if !json.Valid(call.Function.Arguments) {
		p.rec.IsError = true
		p.result = &ToolResult{IsError: true, Content: "malformed tool arguments (not valid JSON)"}
		return p, nil
	}

	// #436: capture the static effect BEFORE the gate so a blocked call still
	// records the effect it was dispatched under, as a denial does, then
	// inspect in this single serial prepare phase shared by both dispatch
	// paths, BEFORE Plan and approval: a blocked call never plans, never
	// prompts, and never invokes.
	p.effect = normalizeEffect(tool.Effect())
	blockedCall, err := ic.inspectToolCall(ctx, obs, step, call, p.effect)
	if err != nil {
		return p, err
	}
	if blockedCall != nil {
		p.rec.IsError, p.rec.Blocked = true, true
		p.result = blockedCall
		return p, nil
	}

	effect := tool.Effect()
	var preview, approvalKey string
	if pt, ok := tool.(PlanningTool); ok {
		plan, err := pt.Plan(ctx, cloneToolCall(call).Function.Arguments)
		if err != nil {
			p.rec.IsError = true
			p.result = &ToolResult{IsError: true, Content: "plan failed: " + err.Error(), Origin: staticOrigin(tool)}
			p.inspectResult = true
			return p, nil
		}
		effect, preview, approvalKey = plan.Effect, plan.Preview, plan.ApprovalKey
	}
	effect = normalizeEffect(effect)
	p.effect = effect // capture now so a denial (which returns before invoke) still carries the effect

	if needsApproval(effect.Approval, effect.Class) {
		d, err := approve(ctx, approver, cloneToolCall(call), preview, approvalKey, ic.snapshot())
		if err != nil {
			return p, err // ctx cancel / approver failure propagates
		}
		if !d.Approved {
			p.rec.IsError, p.rec.Denied = true, true
			p.result = &ToolResult{IsError: true, Content: "tool call denied by approver"}
			return p, nil
		}
		p.rec.AutoApproved = d.ViaGrant
	}
	if limit, ok := gov.reserveInvocation(name); !ok {
		p.rec.IsError = true
		p.result = &ToolResult{IsError: true, Content: fmt.Sprintf("tool invocation budget reached for %s (%d per run)", name, limit)}
		return p, nil
	}

	if err := obs.OnToolCall(ctx, ToolCallEvent{Step: step, Call: cloneToolCall(call), Effect: cloneEffect(effect), Preview: preview}); err != nil {
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
		return ToolResult{IsError: true, Content: err.Error(), Origin: staticOrigin(tool)}
	}
	out = capOutput(out, effect.OutputCap)
	// #436 spec D4: an unset per-invocation origin defers to the static
	// declaration; a set but invalid one is unknown provenance, never the
	// declaration (a tool emitting garbage earns no trust). A per-invocation
	// value may only move toward LESS trust: a tool declared foreign or
	// undeclared cannot promote its own output into the tagged class.
	static := staticOrigin(tool)
	switch {
	case out.Origin == OriginUnknown:
		out.Origin = static
	case !trusted(static) && trusted(normalizeOrigin(out.Origin)):
		out.Origin = static
	default:
		out.Origin = normalizeOrigin(out.Origin)
	}
	return out
}

// dispatch is the serial composition: prepare on the main goroutine, then invoke.
// A cancelled PARENT context after invoke is a hard abort (distinct from a tool's
// own per-call timeout, which stays a model-visible IsError observation).
func (o *Orchestrator) dispatch(ctx context.Context, reg *toolRegistry, call provider.ToolCall,
	approver Approver, obs Observer, step int, gov *restraintGovernor, ic *interceptorRun) (ToolResult, Effect, ToolCallRecord, bool, error) {

	p, err := o.prepareCall(ctx, reg, call, approver, obs, step, gov, ic)
	if err != nil {
		return ToolResult{}, p.effect, p.rec, p.inspectResult, err
	}
	if p.result != nil {
		return *p.result, p.effect, p.rec, p.inspectResult, nil
	}
	start := o.now()
	out := o.invokeCall(ctx, p.tool, p.effect, p.call.Function.Arguments) // the canonical, inspected bytes
	p.rec.Invoked = true
	p.rec.Latency = o.now().Sub(start)
	p.rec.IsError = out.IsError
	if ctx.Err() != nil {
		return out, p.effect, p.rec, false, ctx.Err() // parent cancelled -> hard abort
	}
	return out, p.effect, p.rec, false, nil
}

// approve applies the fail-safe: a nil approver denies any call that reaches it
// (read-only tools never reach it because needsApproval is false for them).
// A RiskApprover receives the run's cumulative RiskReport snapshot (#436); a
// KeyedApprover receives the plan's structural ApprovalKey and returns the full
// decision; a plain Approver keeps its existing signature, its bare bool
// lifted into a decision with no grant provenance.
func approve(ctx context.Context, approver Approver, call provider.ToolCall, preview, approvalKey string, risk RiskReport) (ApprovalDecision, error) {
	if approver == nil {
		return ApprovalDecision{}, nil
	}
	if ra, ok := approver.(RiskApprover); ok {
		return ra.ApproveWithRisk(ctx, call, preview, approvalKey, risk)
	}
	if ka, ok := approver.(KeyedApprover); ok {
		return ka.ApproveKeyed(ctx, call, preview, approvalKey)
	}
	ok, err := approver.Approve(ctx, call, preview)
	return ApprovalDecision{Approved: ok}, err
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
