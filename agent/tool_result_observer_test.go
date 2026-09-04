package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// resultRec implements Observer AND ToolResultObserver, recording every
// OnToolResult. failAt (1-based) makes the Nth OnToolResult return an error.
type resultRec struct {
	results []ToolResultEvent
	failAt  int
	n       int
}

type resultIsolationObserver struct {
	resultEffectPath string
}

func (*resultIsolationObserver) OnStep(context.Context, StepEvent) error { return nil }
func (*resultIsolationObserver) OnToolCall(_ context.Context, e ToolCallEvent) error {
	e.Effect.Scope.Paths[0] = "tool-call-observer-path"
	return nil
}
func (*resultIsolationObserver) OnToken(context.Context, TokenEvent) error { return nil }
func (o *resultIsolationObserver) OnToolResult(_ context.Context, e ToolResultEvent) error {
	o.resultEffectPath = e.Effect.Scope.Paths[0]
	e.Call.Function.Arguments[1] = 'X'
	e.Effect.Scope.Paths[0] = "observer-path"
	e.Result.RouteOutcome.Reason = "observer-reason"
	e.Result.RouteOutcome.Attempts[0].ErrorClass = "observer-error"
	e.Result.RouteOutcome.ScoreBreakdown.FeedbackMode = "observer-mode"
	*e.Result.RouteOutcome.ScoreBreakdown.FeedbackUpdatedAt = time.Time{}
	return nil
}

type resultIsolationTool struct {
	paths []string
	route *provider.RouteOutcome
}

func (resultIsolationTool) Spec() ToolSpec {
	return ToolSpec{Name: "isolated", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t *resultIsolationTool) Effect() Effect {
	return Effect{Class: Read, Approval: ApprovalNever, Scope: Scope{Paths: t.paths}}
}
func (t *resultIsolationTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "done", RouteOutcome: t.route}, nil
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

func TestOnToolResultReceivesDeeplyIsolatedPayload(t *testing.T) {
	updatedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tool := &resultIsolationTool{
		paths: []string{"workspace/file.go"},
		route: &provider.RouteOutcome{
			Reason:   "original-reason",
			Attempts: []provider.RouteAttempt{{ErrorClass: "original-error"}},
			ScoreBreakdown: &provider.ScoreBreakdown{
				FeedbackMode:      "original-mode",
				FeedbackUpdatedAt: &updatedAt,
			},
		},
	}
	first := &stubInterceptor{name: "first", toolCall: func(in ToolCallInspection) []Finding {
		in.Effect.Scope.Paths[0] = "interceptor-path"
		return nil
	}}
	var secondInterceptorPath string
	second := &stubInterceptor{name: "second", toolCall: func(in ToolCallInspection) []Finding {
		secondInterceptorPath = in.Effect.Scope.Paths[0]
		return nil
	}}
	observer := &resultIsolationObserver{}
	o := newTestOrchestrator(toolCallThenFinal("isolated", `{"x":1}`), WithInterceptors(first, second))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{tool}}, observer)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Steps[0].Response.ToolCalls[0].Function.Arguments); got != `{"x":1}` {
		t.Fatalf("observer mutated StepRecord call arguments: %s", got)
	}
	if got := string(res.Messages[1].ToolCalls[0].Function.Arguments); got != `{"x":1}` {
		t.Fatalf("observer mutated State call arguments: %s", got)
	}
	if tool.paths[0] != "workspace/file.go" {
		t.Fatalf("callback mutated tool-owned effect scope: %q", tool.paths[0])
	}
	if secondInterceptorPath != "workspace/file.go" {
		t.Fatalf("one interceptor mutated the next interceptor's effect: %q", secondInterceptorPath)
	}
	if observer.resultEffectPath != "workspace/file.go" {
		t.Fatalf("OnToolCall mutated the effect later published by the run: %q", observer.resultEffectPath)
	}
	got := res.ToolCalls[0].RouteOutcome
	if got.Reason != "original-reason" || got.Attempts[0].ErrorClass != "original-error" ||
		got.ScoreBreakdown.FeedbackMode != "original-mode" || !got.ScoreBreakdown.FeedbackUpdatedAt.Equal(updatedAt) {
		t.Fatalf("observer mutated canonical route outcome: %+v", got)
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
