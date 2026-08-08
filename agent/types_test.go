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
		RepeatLimitReached:  "repeat_limit_reached",
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

func TestBudgetRetainsPositionalLiteralShape(t *testing.T) {
	got := Budget{1, 2, 3, PressureThresholds{}}
	if got.InputCeiling != 1 || got.OutputReserve != 2 || got.TotalTokens != 3 {
		t.Fatalf("Budget = %+v", got)
	}
}
