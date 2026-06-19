package agent

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
)

func (o *Orchestrator) runToolCalls(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor) error {

	for _, call := range calls {
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_call"})
		out, rec, err := o.dispatch(ctx, reg, call, approver, obs, step)
		if err != nil {
			return err
		}
		res.ToolCalls = append(res.ToolCalls, rec)
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_result"})
		state.Messages = append(state.Messages, toolObservation(call, out))
		gov.observe(call, out)
	}
	if gov.tripped() {
		res.StopReason = ToolErrorCapReached
	}
	return nil
}

func (o *Orchestrator) dispatch(ctx context.Context, reg *toolRegistry, call provider.ToolCall,
	approver Approver, obs Observer, step int) (ToolResult, ToolCallRecord, error) {

	name := call.Function.Name
	rec := ToolCallRecord{Step: step, Name: name}

	tool, ok := reg.lookup(name)
	if !ok {
		rec.IsError = true
		return ToolResult{IsError: true, Content: "unknown tool: " + name}, rec, nil
	}
	if !json.Valid(call.Function.Arguments) {
		rec.IsError = true
		return ToolResult{IsError: true, Content: "malformed tool arguments (not valid JSON)"}, rec, nil
	}

	effect := tool.Effect()
	var preview string
	if pt, ok := tool.(PlanningTool); ok {
		plan, err := pt.Plan(ctx, call.Function.Arguments)
		if err != nil {
			rec.IsError = true
			return ToolResult{IsError: true, Content: "plan failed: " + err.Error()}, rec, nil
		}
		effect, preview = plan.Effect, plan.Preview
	}
	effect = normalizeEffect(effect)

	if needsApproval(effect.Approval, effect.Class) {
		ok, err := approve(ctx, approver, call, preview)
		if err != nil {
			return ToolResult{}, rec, err // ctx cancel / approver failure propagates
		}
		if !ok {
			rec.IsError, rec.Denied = true, true
			return ToolResult{IsError: true, Content: "tool call denied by approver"}, rec, nil
		}
	}

	if err := obs.OnToolCall(ctx, ToolCallEvent{Step: step, Call: call, Effect: effect, Preview: preview}); err != nil {
		return ToolResult{}, rec, err
	}

	cctx, cancel := context.WithTimeout(ctx, effect.Timeout)
	defer cancel()
	out, err := tool.Invoke(cctx, call.Function.Arguments)
	if err != nil {
		rec.IsError = true
		return ToolResult{IsError: true, Content: err.Error()}, rec, nil
	}
	out = capOutput(out, effect.OutputCap)
	rec.IsError = out.IsError
	return out, rec, nil
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
