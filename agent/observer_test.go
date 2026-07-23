package agent

import (
	"context"
	"testing"
)

func TestNopObserverIsNotToolResultObserver(t *testing.T) {
	var o Observer = nopObserver{}
	if _, ok := o.(ToolResultObserver); ok {
		t.Fatal("nopObserver must not implement ToolResultObserver (regression guard)")
	}
}

func TestNormalizeObserverNilIsNoop(t *testing.T) {
	obs := normalizeObserver(nil)
	if obs == nil {
		t.Fatal("normalizeObserver(nil) must return a non-nil no-op")
	}
	if err := obs.OnStep(context.Background(), StepEvent{}); err != nil {
		t.Fatalf("no-op OnStep must not error: %v", err)
	}
	if err := obs.OnToolCall(context.Background(), ToolCallEvent{}); err != nil {
		t.Fatalf("no-op OnToolCall must not error: %v", err)
	}
	if err := obs.OnToken(context.Background(), TokenEvent{}); err != nil {
		t.Fatalf("no-op OnToken must not error: %v", err)
	}
}
