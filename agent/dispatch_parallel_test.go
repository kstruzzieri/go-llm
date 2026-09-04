package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// --- shared test helpers -----------------------------------------------------

// tc builds a function tool call with the given name and raw JSON arguments.
func tc(name, args string) provider.ToolCall {
	return provider.ToolCall{
		ID: name, Type: "function",
		Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
	}
}

// readTool is a distinct-named, stateless read-only tool.
type readTool struct{ name string }

func (r readTool) Spec() ToolSpec {
	return ToolSpec{Name: r.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (readTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (r readTool) Invoke(_ context.Context, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "r:" + r.name}, nil
}

// (writeTool is the mutating PlanningTool declared in approval_test.go, name "writer".)

// slowReadTool is a distinct-named read tool with a controllable delay so we can
// invert completion order vs model order.
type slowReadTool struct {
	name string
	d    time.Duration
}

func (s slowReadTool) Spec() ToolSpec {
	return ToolSpec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (slowReadTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (s slowReadTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	select {
	case <-time.After(s.d):
		return ToolResult{Content: "done:" + s.name}, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

func toolMessages(msgs []provider.ChatMessage) []provider.ChatMessage {
	var out []provider.ChatMessage
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// --- Task 1: serial parent-cancel hard abort ---------------------------------

// parentBlockTool blocks on its context, then returns ctx.Err(). With a long
// per-call timeout, only a PARENT cancellation ends it.
type parentBlockTool struct{ name string }

func (b parentBlockTool) Spec() ToolSpec {
	return ToolSpec{Name: b.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (parentBlockTool) Effect() Effect {
	return Effect{Class: Read, Approval: ApprovalNever, Timeout: time.Hour}
}
func (parentBlockTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	<-ctx.Done()
	return ToolResult{}, ctx.Err()
}

// TestSerialDispatchParentCancelHardAborts: a SINGLE-call batch (serial path);
// cancelling the parent context makes Run return the cancellation error rather
// than turning it into an IsError observation.
func TestSerialDispatchParentCancelHardAborts(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "block", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "unreached", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	ctx, cancel := context.WithCancel(context.Background())

	type runResult struct {
		res Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, err := o.Run(ctx, Request{Goal: "q", Tools: []Tool{parentBlockTool{name: "block"}}}, nil)
		done <- runResult{res, err}
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case rr := <-done:
		if !errors.Is(rr.err, context.Canceled) {
			t.Fatalf("parent cancel must hard-abort Run with context.Canceled, got err=%v stop=%v", rr.err, rr.res.StopReason)
		}
		if len(rr.res.ToolCalls) != 1 || !rr.res.ToolCalls[0].Invoked {
			t.Fatalf("parent cancellation lost completed Invoke metadata: %+v", rr.res.ToolCalls)
		}
		if got := kinds(rr.res.Events); got != "step,tool_call,tool_result" {
			t.Fatalf("events = %s, want durable tool_result metadata", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after parent cancellation (possible goroutine leak)")
	}
}

// --- Task 2: eligibility gate ------------------------------------------------

func TestCanRunParallelGate(t *testing.T) {
	reg, err := newToolRegistry([]Tool{
		readTool{name: "a"}, readTool{name: "b"}, writeTool{},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	tests := []struct {
		name  string
		calls []provider.ToolCall
		want  bool
	}{
		{"two distinct reads", []provider.ToolCall{tc("a", `{}`), tc("b", `{}`)}, true},
		{"duplicate name", []provider.ToolCall{tc("a", `{}`), tc("a", `{"x":1}`)}, false},
		{"contains write", []provider.ToolCall{tc("a", `{}`), tc("writer", `{}`)}, false},
		{"unknown tool", []provider.ToolCall{tc("a", `{}`), tc("zzz", `{}`)}, false},
		{"bad json", []provider.ToolCall{tc("a", `{}`), tc("b", `{not json`)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canRunParallel(reg, tt.calls); got != tt.want {
				t.Fatalf("canRunParallel = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Task 3: model-order preservation ----------------------------------------

// TestParallelPreservesModelOrder: completion order (e,d,c,b,a) is inverted from
// model order (a,b,c,d,e); results and tool observations must follow model order.
func TestParallelPreservesModelOrder(t *testing.T) {
	tools := []Tool{
		slowReadTool{name: "a", d: 60 * time.Millisecond},
		slowReadTool{name: "b", d: 45 * time.Millisecond},
		slowReadTool{name: "c", d: 30 * time.Millisecond},
		slowReadTool{name: "d", d: 15 * time.Millisecond},
		slowReadTool{name: "e", d: 1 * time.Millisecond},
	}
	want := []string{"a", "b", "c", "d", "e"}
	calls := make([]provider.ToolCall, len(want))
	for i, n := range want {
		calls[i] = tc(n, `{}`)
	}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: tools}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed {
		t.Fatalf("stop = %v, want Completed", res.StopReason)
	}
	if len(res.ToolCalls) != len(want) {
		t.Fatalf("tool call records = %d, want %d", len(res.ToolCalls), len(want))
	}
	for i, n := range want {
		if res.ToolCalls[i].Name != n {
			t.Fatalf("ToolCalls[%d].Name = %q, want %q", i, res.ToolCalls[i].Name, n)
		}
	}
	tms := toolMessages(res.Messages)
	if len(tms) != len(want) {
		t.Fatalf("tool messages = %d, want %d", len(tms), len(want))
	}
	for i, n := range want {
		if tms[i].Content != "done:"+n {
			t.Fatalf("tool message[%d] = %q, want %q", i, tms[i].Content, "done:"+n)
		}
	}
}

// --- Task 4: faster than serial ----------------------------------------------

// TestParallelFasterThanSerial: N distinct read tools (> the cap, so the bound is
// exercised) each sleep `unit`. Serial would take ~N*unit; the parallel path runs
// in a few waves and must finish well under that.
func TestParallelFasterThanSerial(t *testing.T) {
	const (
		n    = 10 // > parallelToolCallLimit (8)
		unit = 50 * time.Millisecond
	)
	tools := make([]Tool, n)
	calls := make([]provider.ToolCall, n)
	for i := range n {
		name := "rt" + string(rune('A'+i))
		tools[i] = slowReadTool{name: name, d: unit}
		calls[i] = tc(name, `{}`)
	}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)

	start := time.Now()
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: tools}, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != n {
		t.Fatalf("tool call records = %d, want %d", len(res.ToolCalls), n)
	}
	if elapsed >= 5*unit {
		t.Fatalf("parallel dispatch took %v; expected < %v (serial would be ~%v)", elapsed, 5*unit, n*unit)
	}
}

// --- Task 5: event ordering --------------------------------------------------

// TestParallelEventOrdering: a parallel batch emits all tool_call events, then all
// tool_result events (no interleave) — the documented ordering for a parallel batch.
func TestParallelEventOrdering(t *testing.T) {
	tools := []Tool{readTool{name: "a"}, readTool{name: "b"}, readTool{name: "c"}}
	calls := []provider.ToolCall{tc("a", `{}`), tc("b", `{}`), tc("c", `{}`)}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: tools}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var seq []string
	for _, e := range res.Events {
		if e.Kind == "tool_call" || e.Kind == "tool_result" {
			seq = append(seq, e.Kind)
		}
	}
	want := []string{"tool_call", "tool_call", "tool_call", "tool_result", "tool_result", "tool_result"}
	if len(seq) != len(want) {
		t.Fatalf("tool event sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("tool event[%d] = %q, want %q (full: %v)", i, seq[i], want[i], seq)
		}
	}
}

// --- Task 6: governor serial tail within a parallel batch --------------------

// erroringReadTool is a distinct-named read tool that always errors.
type erroringReadTool struct{ name string }

func (e erroringReadTool) Spec() ToolSpec {
	return ToolSpec{Name: e.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (erroringReadTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (erroringReadTool) Invoke(_ context.Context, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{}, errors.New("boom")
}

// TestParallelGovernorSerialTail: 5 distinct erroring read tools run concurrently,
// but Phase 3 applies the governor in model order and short-circuits after
// defaultToolErrorCap consecutive errors. The trailing results stay out of State,
// while every already-computed outcome remains in the audit record.
func TestParallelGovernorSerialTail(t *testing.T) {
	const n = 5
	tools := make([]Tool, n)
	calls := make([]provider.ToolCall, n)
	for i := range n {
		name := "boom" + string(rune('0'+i))
		tools[i] = erroringReadTool{name: name}
		calls[i] = tc(name, `{}`)
	}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
	}}
	blockFourth := &stubInterceptor{name: "guard", toolCall: func(call ToolCallInspection) []Finding {
		if call.Call.Function.Name == "boom3" {
			return []Finding{{Rule: "deny", Verdict: VerdictBlock, Risk: 100}}
		}
		return nil
	}}
	o := newTestOrchestrator(mc, WithInterceptors(blockFourth))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: tools}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != ToolErrorCapReached {
		t.Fatalf("stop = %v, want ToolErrorCapReached", res.StopReason)
	}
	if len(res.ToolCalls) != n {
		t.Fatalf("audit records = %d, want all %d completed Invokes", len(res.ToolCalls), n)
	}
	for i, rec := range res.ToolCalls {
		if i == 3 {
			if rec.Invoked || !rec.Blocked || !rec.IsError {
				t.Fatalf("blocked synthetic call = %+v", rec)
			}
		} else if !rec.Invoked {
			t.Fatalf("completed parallel call is not marked invoked: %+v", res.ToolCalls)
		}
	}
	if got := len(toolMessages(res.Messages)); got != defaultToolErrorCap {
		t.Fatalf("model-visible tool observations = %d, want governor cap %d", got, defaultToolErrorCap)
	}
}

// --- Task 7: context cancellation, no leak -----------------------------------

// startSignalTool blocks on its context (long per-call timeout) and signals start
// and exit through atomic counters, so the test can prove every started worker
// exited after a parent cancellation (no goroutine leak).
type startSignalTool struct {
	name    string
	started *int32
	exited  *int32
}

func (b startSignalTool) Spec() ToolSpec {
	return ToolSpec{Name: b.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (startSignalTool) Effect() Effect {
	return Effect{Class: Read, Approval: ApprovalNever, Timeout: time.Hour}
}
func (b startSignalTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	atomic.AddInt32(b.started, 1)
	<-ctx.Done()
	atomic.AddInt32(b.exited, 1)
	return ToolResult{}, ctx.Err()
}

// TestParallelContextCancel: a batch of blocking read tools (count <= cap so all
// start) is interrupted by a parent cancellation. Run must return context.Canceled,
// and every worker that started must exit (errgroup.Wait joins them).
func TestParallelContextCancel(t *testing.T) {
	const n = 4 // <= parallelToolCallLimit so all workers start
	var started, exited int32
	tools := make([]Tool, n)
	calls := make([]provider.ToolCall, n)
	for i := range n {
		name := "blk" + string(rune('0'+i))
		tools[i] = startSignalTool{name: name, started: &started, exited: &exited}
		calls[i] = tc(name, `{}`)
	}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "unreached", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	ctx, cancel := context.WithCancel(context.Background())

	type runResult struct {
		res Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, err := o.Run(ctx, Request{Goal: "q", Tools: tools}, nil)
		done <- runResult{res, err}
	}()

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&started) < int32(n) {
		select {
		case <-deadline:
			t.Fatalf("only %d/%d workers started", atomic.LoadInt32(&started), n)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case rr := <-done:
		if !errors.Is(rr.err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", rr.err)
		}
		if len(rr.res.ToolCalls) != n {
			t.Fatalf("parent cancellation lost completed parallel Invoke metadata: %+v", rr.res.ToolCalls)
		}
		for _, rec := range rr.res.ToolCalls {
			if !rec.Invoked {
				t.Fatalf("parallel cancellation record is not marked invoked: %+v", rr.res.ToolCalls)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel (goroutine leak)")
	}
	if got := atomic.LoadInt32(&exited); got != int32(n) {
		t.Fatalf("exited workers = %d, want %d (Wait must join every started worker)", got, n)
	}
}
