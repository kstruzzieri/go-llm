package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// sleepTool sleeps d on Invoke; read-only so the batch runs in parallel.
type sleepTool struct {
	d    time.Duration
	name string
}

func (t sleepTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t sleepTool) Effect() Effect { return Effect{Class: Read} }
func (t sleepTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	select {
	case <-time.After(t.d):
	case <-ctx.Done():
	}
	return ToolResult{Content: "ok"}, nil
}

func TestRun_ParallelPerToolLatencyIsolated(t *testing.T) {
	// Two read-only tools in one assistant turn run concurrently (canRunParallel).
	model := &scriptedModel{resps: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{
			toolCall("c1", "slow", "{}"),
			toolCall("c2", "fast", "{}"),
		}},
		{Content: "final"},
	}}
	o := New(model, ContextManager{}) // real time.Now

	res, err := o.Run(context.Background(), Request{
		Goal: "go",
		Tools: []Tool{
			sleepTool{d: 60 * time.Millisecond, name: "slow"},
			sleepTool{d: 5 * time.Millisecond, name: "fast"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(res.ToolCalls))
	}
	byName := map[string]ToolCallRecord{}
	for _, r := range res.ToolCalls {
		byName[r.Name] = r
	}
	slow, fast := byName["slow"], byName["fast"]
	if !slow.Invoked || !fast.Invoked {
		t.Fatalf("both must be Invoked: slow=%v fast=%v", slow.Invoked, fast.Invoked)
	}
	// Per-tool isolation: fast must be far below slow. A sink that timed
	// OnToolCall->OnToolResult would report ~slow for fast too (recorded in
	// phase 3 after the slow invoke), so fast >= slow/2 there.
	if fast.Latency >= slow.Latency/2 {
		t.Fatalf("fast.Latency=%v not isolated from slow.Latency=%v", fast.Latency, slow.Latency)
	}
}
