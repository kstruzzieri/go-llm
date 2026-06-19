package agent

import (
	"context"
	"errors"
	"testing"
)

func TestTurnBudgetUsesDefaultWhenZero(t *testing.T) {
	b := turnBudget(Budget{})
	if b.Input <= 0 {
		t.Fatal("zero InputCeiling must fall back to a positive default")
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
