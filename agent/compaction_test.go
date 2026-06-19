package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestRecencyCompactorEvictsCompletedChainMidLoop proves a completed tool chain
// is evictable even when no plain (no-tool-call) assistant message follows it —
// the realistic shape of an in-progress agent transcript.
func TestRecencyCompactorEvictsCompletedChainMidLoop(t *testing.T) {
	chain := func(id, content string) []Message {
		return []Message{
			{ChatMessage: provider.ChatMessage{Role: "assistant", ToolCalls: []provider.ToolCall{{
				ID: id, Type: "function",
				Function: provider.ToolCallFunction{Name: "t", Arguments: json.RawMessage(`{}`)},
			}}}, Segment: Elastic},
			{ChatMessage: provider.ChatMessage{Role: "tool", ToolCallID: id, ToolName: "t", Content: content}, Segment: Elastic},
		}
	}
	msgs := []Message{pinned("user", "G")}
	msgs = append(msgs, chain("c1", "OLDRESULT")...) // oldest completed chain
	msgs = append(msgs, chain("c2", "NEWRESULT")...) // newest completed chain
	st := State{Messages: msgs}

	rc := RecencyCompactor{Estimate: runeEstimator}
	// Budget one token under the full total forces exactly one group to drop;
	// the oldest completed chain (c1) must be the one evicted.
	out, rep, err := rc.Compact(context.Background(), st, TokenBudget{Input: rc.total(st) - 1})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if rep.DroppedCount != 1 {
		t.Fatalf("DroppedCount = %d, want 1", rep.DroppedCount)
	}
	for _, m := range out.Messages {
		if m.ToolCallID == "c1" || (len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "c1") {
			t.Fatalf("oldest completed chain c1 must be evicted, got %+v", out.Messages)
		}
	}
	// atomicity: goal + the c2 chain (assistant + tool) survive, nothing orphaned
	if len(out.Messages) != 3 {
		t.Fatalf("want goal + c2 chain (3 messages), got %+v", out.Messages)
	}
}

// TestRecencyCompactorPreservesUnresolvedTail proves a chain whose tool_calls
// are not all answered yet is never dropped (would orphan a tool_call_id).
func TestRecencyCompactorPreservesUnresolvedTail(t *testing.T) {
	// assistant requests TWO tool calls but only ONE result is present -> unresolved.
	asst := Message{ChatMessage: provider.ChatMessage{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{
			{ID: "a", Type: "function", Function: provider.ToolCallFunction{Name: "t", Arguments: json.RawMessage(`{}`)}},
			{ID: "b", Type: "function", Function: provider.ToolCallFunction{Name: "t", Arguments: json.RawMessage(`{}`)}},
		},
	}, Segment: Elastic}
	onlyResult := Message{ChatMessage: provider.ChatMessage{Role: "tool", ToolCallID: "a", ToolName: "t", Content: "R"}, Segment: Elastic}
	st := State{Messages: []Message{pinned("user", "G"), asst, onlyResult}}

	rc := RecencyCompactor{Estimate: runeEstimator}
	out, _, err := rc.Compact(context.Background(), st, TokenBudget{Input: 1}) // brutally tight
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// the unresolved tail cannot be dropped, so it survives despite the tiny budget
	foundAsst, foundResult := false, false
	for _, m := range out.Messages {
		if len(m.ToolCalls) == 2 {
			foundAsst = true
		}
		if m.ToolCallID == "a" {
			foundResult = true
		}
	}
	if !foundAsst || !foundResult {
		t.Fatalf("unresolved tail must be preserved, got %+v", out.Messages)
	}
}

// bulkyTool returns a large observation so a tool turn blows past a tight budget.
type bulkyTool struct{}

func (bulkyTool) Spec() ToolSpec { return ToolSpec{Name: "bulky", Parameters: json.RawMessage(`{}`)} }
func (bulkyTool) Effect() Effect { return Effect{Class: Read} } // default cap (64KiB), not truncated
func (bulkyTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: strings.Repeat("X", 400)}, nil
}

// TestRunEmitsCompactionEvent drives a tool-heavy loop under a tight input
// ceiling so Assemble actually compacts, and asserts the compaction event fires.
func TestRunEmitsCompactionEvent(t *testing.T) {
	bulkyCall := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "1", Type: "function",
		Function: provider.ToolCallFunction{Name: "bulky", Arguments: json.RawMessage(`{}`)},
	}}}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: bulkyCall},
		{Response: bulkyCall},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Budget: Budget{InputCeiling: 80}, Tools: []Tool{bulkyTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	saw := false
	for _, e := range res.Events {
		if e.Kind == "compaction" {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("expected a compaction event in Result.Events, got %+v", res.Events)
	}
}
