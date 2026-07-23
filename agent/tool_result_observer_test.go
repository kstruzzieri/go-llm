package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// resultRec implements Observer AND ToolResultObserver, recording every
// OnToolResult. failAt (1-based) makes the Nth OnToolResult return an error.
type resultRec struct {
	results []ToolResultEvent
	failAt  int
	n       int
}

func (r *resultRec) OnStep(context.Context, StepEvent) error         { return nil }
func (r *resultRec) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (r *resultRec) OnToken(context.Context, TokenEvent) error       { return nil }
func (r *resultRec) OnToolResult(_ context.Context, e ToolResultEvent) error {
	r.n++
	r.results = append(r.results, e)
	if r.failAt > 0 && r.n == r.failAt {
		return fmt.Errorf("boom")
	}
	return nil
}

// plainObs implements ONLY Observer (regression guard: must not get results).
type plainObs struct{ calls int }

func (p *plainObs) OnStep(context.Context, StepEvent) error         { return nil }
func (p *plainObs) OnToolCall(context.Context, ToolCallEvent) error { p.calls++; return nil }
func (p *plainObs) OnToken(context.Context, TokenEvent) error       { return nil }

// denyTool is a mutating tool: with a nil approver the runtime denies it.
type denyTool struct{}

func (denyTool) Spec() ToolSpec {
	return ToolSpec{Name: "mutate", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (denyTool) Effect() Effect { return Effect{Class: Write} }
func (denyTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "did"}, nil
}

type errApprover struct{}

func (errApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return false, fmt.Errorf("approver down")
}

type planFailTool struct{}

func (planFailTool) Spec() ToolSpec {
	return ToolSpec{Name: "planfail", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (planFailTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (planFailTool) Plan(context.Context, json.RawMessage) (ToolPlan, error) {
	return ToolPlan{}, fmt.Errorf("plan exploded")
}
func (planFailTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "must not invoke"}, nil
}

type invokeErrTool struct{}

func (invokeErrTool) Spec() ToolSpec {
	return ToolSpec{Name: "errtool", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (invokeErrTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (invokeErrTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, fmt.Errorf("tool exploded")
}

func toolCallThenFinal(name, args string) *scriptedCaller {
	return &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
		}}}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
}

func TestOnToolResult_NormalInvoke(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("echo", `{"x":1}`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 {
		t.Fatalf("want 1 result, got %d", len(rec.results))
	}
	got := rec.results[0]
	if got.Call.Function.Name != "echo" || got.Result.IsError || got.Result.Content != `tool-said:{"x":1}` {
		t.Fatalf("unexpected result event: %+v", got)
	}
}

func TestOnToolResult_UnknownTool(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("nope", `{}`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(), Request{Goal: "q"}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].Result.IsError ||
		rec.results[0].Result.Content != "unknown tool: nope" {
		t.Fatalf("unknown-tool result not emitted: %+v", rec.results)
	}
}

func TestOnToolResult_MalformedArgs(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("echo", `{bad`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].Result.IsError ||
		rec.results[0].Result.Content != "malformed tool arguments (not valid JSON)" {
		t.Fatalf("malformed-args result not emitted: %+v", rec.results)
	}
}

func TestOnToolResult_PlanFailure(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("planfail", `{}`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{planFailTool{}}}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].Result.IsError ||
		rec.results[0].Result.Content != "plan failed: plan exploded" {
		t.Fatalf("plan-fail result not emitted: %+v", rec.results)
	}
}

func TestOnToolResult_Denied(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("mutate", `{}`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{denyTool{}}}, rec); err != nil { // Approver nil => denied
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].Result.IsError ||
		rec.results[0].Result.Content != "tool call denied by approver" {
		t.Fatalf("denied result not emitted: %+v", rec.results)
	}
}

func TestOnToolResult_InvokeError(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("errtool", `{}`))
	rec := &resultRec{}
	if _, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{invokeErrTool{}}}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].Result.IsError ||
		rec.results[0].Result.Content != "tool exploded" {
		t.Fatalf("invoke-error result not emitted: %+v", rec.results)
	}
}

func TestOnToolResult_ErrorAbortsRun(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("echo", `{}`))
	rec := &resultRec{failAt: 1}
	_, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("OnToolResult error must abort Run, got %v", err)
	}
}

func TestOnToolResult_HardAbortDoesNotEmit(t *testing.T) {
	o := newTestOrchestrator(toolCallThenFinal("mutate", `{}`))
	rec := &resultRec{}
	_, err := o.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{denyTool{}}, Approver: errApprover{}}, rec)
	if err == nil {
		t.Fatal("approver error must abort Run")
	}
	if len(rec.results) != 0 {
		t.Fatalf("hard abort must not emit OnToolResult, got %+v", rec.results)
	}
}

func TestPlainObserver_UnaffectedByToolResultSeam(t *testing.T) {
	var o Observer = &plainObs{}
	if _, ok := o.(ToolResultObserver); ok {
		t.Fatal("plainObs must not satisfy ToolResultObserver")
	}
	orch := newTestOrchestrator(toolCallThenFinal("echo", `{}`))
	res, err := orch.Run(context.Background(),
		Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, o)
	if err != nil || res.Answer != "final" {
		t.Fatalf("plain observer run failed: answer=%q err=%v", res.Answer, err)
	}
}
