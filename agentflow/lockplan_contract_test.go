package agentflow

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// agentflowRunnerForTest returns a Runner for the installed binary, or the
// python-src fallback checkout from AGENTFLOW_SRC, or skips.
func agentflowRunnerForTest(t *testing.T, dir string) Runner {
	if _, err := exec.LookPath("agentflow"); err == nil {
		return NewExecRunner(dir)
	}
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
		return NewSrcExecRunner(dir, src)
	}
	t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	return nil
}

func TestLockPlan_RealCLI(t *testing.T) {
	dir := t.TempDir()
	r := agentflowRunnerForTest(t, dir)
	c := NewClient(r, dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Lock OUR compiler's output, not a hand-authored fixture. This pins the
	// compiler against the real validator: a legitimate agentflow tightening
	// fails here, not in production, and we never pin accidental permissiveness.
	plan := Compile(PlanIR{
		Objective:    "smoke: compiler output must lock",
		Scope:        []string{"src"},
		Invariants:   []string{"only src/answer.txt changes"},
		RiskLevel:    "low",
		RollbackPlan: "git checkout -- .",
		AllowedFiles: []string{"src/*"},
		Steps: []StepIR{{
			ID:           "P1",
			Action:       "ensure src/answer.txt contains the expected token",
			Files:        []string{"src/answer.txt"},
			ExpectedDiff: []string{"src/answer.txt changes pending to expected"},
			Validations:  []GateIR{{Label: "grep", Argv: []string{"grep", "-q", "expected", "src/answer.txt"}}},
		}},
	})
	if ds := CheckPlan(plan); len(ds) != 0 {
		t.Fatalf("compiler output failed local pre-check: %v", ds)
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "compiled-plan.json")
	if err := os.WriteFile(planPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatalf("real lock-plan rejected compiler output: %v", err)
	}
}

func TestLockPlan_RealCLI_RejectsCycle(t *testing.T) {
	dir := t.TempDir()
	r := agentflowRunnerForTest(t, dir)
	c := NewClient(r, dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// Two mutually dependent steps: the real CLI must reject the lock.
	plan := Compile(PlanIR{
		Objective: "cycle", Scope: []string{"src"}, Invariants: []string{"x"},
		RiskLevel: "low", RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Steps: []StepIR{
			{ID: "A", Action: "a", Files: []string{"src/a"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"B"}},
			{ID: "B", Action: "b", Files: []string{"src/b"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"A"}},
		},
	})
	b, _ := json.MarshalIndent(plan, "", "  ")
	planPath := filepath.Join(dir, "cycle-plan.json")
	if err := os.WriteFile(planPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err == nil {
		t.Fatal("expected real lock-plan to reject a dependency cycle")
	}
}
