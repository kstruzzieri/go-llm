package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// scriptedModel returns a pre-set response per step (step 0 first). It never
// advances the clock, so only tool invokes move time in these tests.
type scriptedModel struct {
	resps []provider.ChatResponse
	step  int
}

func (m *scriptedModel) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
	r := m.resps[m.step]
	m.step++
	return ModelResult{Response: r}, nil
}

// clockTool advances the shared fake clock by d on Invoke; effect is read-only.
type clockTool struct {
	clk  *fakeClock
	d    time.Duration
	name string
}

func (t clockTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t clockTool) Effect() Effect { return Effect{Class: Read} }
func (t clockTool) Invoke(_ context.Context, _ json.RawMessage) (ToolResult, error) {
	t.clk.advance(t.d)
	return ToolResult{Content: "ok"}, nil
}

// toolResultCapture records ToolResultEvents (implements ToolResultObserver).
type toolResultCapture struct {
	stepCapture
	results []ToolResultEvent
}

func (c *toolResultCapture) OnToolResult(_ context.Context, e ToolResultEvent) error {
	c.results = append(c.results, e)
	return nil
}

func toolCall(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func TestRun_InvokedToolLatencyAndEffect(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	model := &scriptedModel{resps: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{toolCall("c1", "tick", "{}")}},
		{Content: "final"},
	}}
	o := New(model, ContextManager{}, WithClock(clk.now))

	cap := &toolResultCapture{}
	res, err := o.Run(context.Background(), Request{
		Goal:  "go",
		Tools: []Tool{clockTool{clk: clk, d: 40 * time.Millisecond, name: "tick"}},
	}, cap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(res.ToolCalls))
	}
	rec := res.ToolCalls[0]
	if !rec.Invoked || rec.Latency != 40*time.Millisecond {
		t.Fatalf("rec Invoked=%v Latency=%v, want true/40ms", rec.Invoked, rec.Latency)
	}
	if len(cap.results) != 1 {
		t.Fatalf("ToolResultEvents = %d, want 1", len(cap.results))
	}
	ev := cap.results[0]
	if !ev.Invoked || ev.Latency != 40*time.Millisecond || ev.Effect.Class != Read || ev.Denied {
		t.Fatalf("event = %+v, want Invoked/40ms/Read/!Denied", ev)
	}
}

func TestRun_SyntheticToolNotInvoked(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	model := &scriptedModel{resps: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{toolCall("c1", "ghost", "{}")}}, // unknown tool
		{Content: "final"},
	}}
	o := New(model, ContextManager{}, WithClock(clk.now))

	cap := &toolResultCapture{}
	res, err := o.Run(context.Background(), Request{Goal: "go", Tools: []Tool{clockTool{clk: clk, d: time.Second, name: "tick"}}}, cap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec := res.ToolCalls[0]
	if rec.Invoked || rec.Latency != 0 || !rec.IsError {
		t.Fatalf("synthetic rec = %+v, want !Invoked/0/IsError", rec)
	}
	if ev := cap.results[0]; ev.Invoked || ev.Latency != 0 {
		t.Fatalf("synthetic event = %+v, want !Invoked/0", ev)
	}
}
