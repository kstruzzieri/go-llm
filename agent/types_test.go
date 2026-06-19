package agent

import (
	"errors"
	"testing"
)

func TestStopReasonString(t *testing.T) {
	cases := map[StopReason]string{
		Completed:           "completed",
		StepCapReached:      "step_cap_reached",
		BudgetReached:       "budget_reached",
		ToolErrorCapReached: "tool_error_cap_reached",
	}
	for sr, want := range cases {
		if got := sr.String(); got != want {
			t.Fatalf("StopReason(%d).String() = %q, want %q", sr, got, want)
		}
	}
}

func TestErrContextExhaustedIsSentinel(t *testing.T) {
	if !errors.Is(ErrContextExhausted, ErrContextExhausted) {
		t.Fatal("ErrContextExhausted must be a comparable sentinel error")
	}
}

func TestDefaultConsts(t *testing.T) {
	if defaultMaxSteps <= 0 || defaultToolTimeout <= 0 || defaultOutputCap <= 0 || defaultToolErrorCap <= 0 {
		t.Fatal("defaults must be positive")
	}
}
