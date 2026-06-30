package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestTurnBudgetUsesDefaultWhenZero(t *testing.T) {
	b := turnBudget(Budget{})
	if b.Input <= 0 {
		t.Fatal("zero InputCeiling must fall back to a positive default")
	}
}

func TestTurnBudgetSubtractsOutputReserve(t *testing.T) {
	b := turnBudget(Budget{InputCeiling: 100, OutputReserve: 25})
	if b.Input != 75 {
		t.Fatalf("Input = %d, want 75", b.Input)
	}
}

func TestAssemblePinnedOverflowReturnsErr(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{System: "system-prompt", Messages: []Message{pinned("user", "a-very-long-goal")}}
	// budget smaller than pinned tokens => exhausted.
	_, _, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: 3})
	if err == nil || !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("want ErrContextExhausted, got %v", err)
	}
}

func TestAssembleCountsToolSchemaAsPinned(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{System: "s", Messages: []Message{pinned("user", "g")}}
	// system(1)+goal(1)=2 fits in 5, but tool schema of 10 pushes pinned over.
	_, _, err := m.Assemble(context.Background(), st, 10, TokenBudget{Input: 5})
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("tool schema tokens must count toward pinned; got %v", err)
	}
}

func TestAssembleCompactsElastic(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{System: "s", Messages: []Message{
		pinned("user", "g"),
		elastic("assistant", "oldold"),
		elastic("user", "new"),
	}}
	out, pr, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: 1 + 1 + 3})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if pr.Evicted == 0 {
		t.Fatal("expected at least one eviction recorded in Pressure")
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want goal + newest elastic, got %+v", out.Messages)
	}
}

func TestAssembleIncludesDurableSummaryBeforeRawMessages(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{
		System:         "sys",
		DurableSummary: "old setup and decisions",
		Messages: []Message{
			elastic("user", "recent question"),
			pinned("user", "goal"),
		},
	}

	out, _, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: 100})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	req := buildChatRequest(out, nil, 0)
	if len(req.Messages) < 4 {
		t.Fatalf("messages = %+v, want system, durable summary, recent, goal", req.Messages)
	}
	if req.Messages[1].Role != "system" || req.Messages[1].Content != DurableSummaryPrompt("old setup and decisions") {
		t.Fatalf("summary message = %+v", req.Messages[1])
	}
	if req.Messages[2].Content != "recent question" {
		t.Fatalf("summary was not before raw messages: %+v", req.Messages)
	}
}

func TestAssembleReturnsErrWhenCompactedStateStillOverBudget(t *testing.T) {
	// An unresolved tail (2 tool_calls, only 1 result present) can never be
	// dropped. Even after evicting all droppable groups the state won't fit in a
	// budget that only covers the pinned goal.
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{Messages: []Message{
		pinned("user", "goal"),
		{
			ChatMessage: provider.ChatMessage{
				Role: "assistant",
				ToolCalls: []provider.ToolCall{
					{ID: "c1", Type: "function",
						Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(`{}`)}},
					{ID: "c2", Type: "function",
						Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(`{}`)}},
				},
			},
			Segment: Elastic,
		},
		{
			ChatMessage: provider.ChatMessage{
				Role:       "tool",
				Content:    "tool-result-too-large",
				ToolName:   "search",
				ToolCallID: "c1",
			},
			Segment: Elastic,
		},
	}}

	// Budget only covers the pinned goal; the unresolved tail (2 calls, 1 result)
	// cannot be dropped, so Assemble must return ErrContextExhausted.
	_, _, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: len("goal") + 1})
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("want ErrContextExhausted when compaction cannot fit, got %v", err)
	}
}

func TestContextManagerZeroValueUsesDefaultCompactor(t *testing.T) {
	var m ContextManager
	st := State{Messages: []Message{pinned("user", "goal")}}
	out, _, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: 100})
	if err != nil {
		t.Fatalf("zero-value ContextManager should use defaults: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Content != "goal" {
		t.Fatalf("unexpected assembled state: %+v", out.Messages)
	}
}

func TestAssembleEnrichedPressureNormalPath(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{System: "sys", Messages: []Message{
		{ChatMessage: provider.ChatMessage{Role: "user", Content: "hello"}, Segment: Pinned},
	}}
	budget := TokenBudget{Input: 1000, Thresholds: PressureThresholds{}.normalize()}
	_, p, err := m.Assemble(context.Background(), st, 0, budget)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if p.InputBudget != 1000 || p.InputTokens <= 0 {
		t.Fatalf("tokens/budget not stamped: %+v", p)
	}
	if p.Level != LevelOK || p.Mitigation != MitigationNone {
		t.Fatalf("low usage should be ok/none, got %v/%v", p.Level, p.Mitigation)
	}
	if p.Cause == CauseUnknown {
		t.Fatalf("cause should be attributed, got %v", p.Cause)
	}
}

func TestAssemblePinnedOverflowPressure(t *testing.T) {
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	st := State{System: "0123456789", Messages: []Message{
		{ChatMessage: provider.ChatMessage{Role: "user", Content: "q"}, Segment: Pinned},
	}}
	budget := TokenBudget{Input: 3, Thresholds: PressureThresholds{}.normalize()}
	_, p, err := m.Assemble(context.Background(), st, 0, budget)
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("want ErrContextExhausted, got %v", err)
	}
	if p.Level != LevelCritical || p.Mitigation != MitigationHalt {
		t.Fatalf("overflow should be critical/halt, got %v/%v", p.Level, p.Mitigation)
	}
	wantPinned := m.pinnedTokens(st, 0)
	if p.InputTokens != wantPinned {
		t.Fatalf("InputTokens=%d, want pinnedTokens=%d", p.InputTokens, wantPinned)
	}
	if p.Cause != CausePinned && p.Cause != CauseToolSchema {
		t.Fatalf("overflow cause should be pinned/tool_schema, got %v", p.Cause)
	}
}

func TestTurnBudgetCopiesNormalizedThresholds(t *testing.T) {
	tb := turnBudget(Budget{InputCeiling: 100, Pressure: PressureThresholds{Warn: 0.80}})
	if tb.Thresholds != (PressureThresholds{Watch: 0.60, Warn: 0.80, Critical: 0.90}) {
		t.Fatalf("thresholds not normalized into TokenBudget: %+v", tb.Thresholds)
	}
}
