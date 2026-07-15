package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// agentflowRunnerForTest honors the explicit AGENTFLOW_SRC checkout, otherwise
// uses an installed binary or skips.
func agentflowRunnerForTest(t *testing.T, dir string) Runner {
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
		return NewSrcExecRunner(dir, src)
	}
	if _, err := exec.LookPath("agentflow"); err == nil {
		return NewExecRunner(dir)
	}
	t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	return nil
}

func TestAgentflowRunnerForTest_PrefersSourceOverride(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "agentflow"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("AGENTFLOW_SRC", "/preferred-checkout")

	runner, ok := agentflowRunnerForTest(t, t.TempDir()).(*ExecRunner)
	if !ok {
		t.Fatalf("runner = %T, want *ExecRunner", runner)
	}
	bin, argv, env := runner.commandFor([]string{"--version"})
	if bin != "python3" || !reflect.DeepEqual(argv, []string{"-m", "agentflow", "--version"}) ||
		!reflect.DeepEqual(env, []string{"PYTHONPATH=/preferred-checkout/src"}) {
		t.Fatalf("command = (%q, %v, %v), want explicit source checkout", bin, argv, env)
	}
}

func TestLockPlan_RealCLI(t *testing.T) {
	dir := t.TempDir()
	r := agentflowRunnerForTest(t, dir)
	c := NewClient(r, dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// Init twice: the -goal author flow re-runs init on every approved lock
	// attempt and relies on the real CLI's init being idempotent.
	if err := c.Init(ctx); err != nil {
		t.Fatalf("second init must be idempotent: %v", err)
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
		Requirements: []RequirementIR{{
			ID: "REQ-1", Text: "The compiler output locks with traceability.",
			AcceptanceCriteria: []CriterionIR{
				{ID: "AC-1", Text: "The expected token is present."},
				{ID: "AC-2", Text: "The input fixture exists."},
			},
		}},
		Steps: []StepIR{
			{
				ID:           "P1",
				Action:       "ensure src/answer.txt contains the expected token",
				Files:        []string{"src/answer.txt"},
				ExpectedDiff: []string{"src/answer.txt changes pending to expected"},
				DependsOn:    []string{"P0"}, // forward reference: P0 is declared below
				CriterionIDs: []string{"AC-1"},
				Validations:  []GateIR{{Label: "grep", Argv: []string{"grep", "-q", "expected", "src/answer.txt"}, CriterionIDs: []string{"AC-1"}}},
			},
			{
				ID:           "P0",
				Action:       "prepare src/input.txt",
				Files:        []string{"src/input.txt"},
				ExpectedDiff: []string{"src/input.txt is ready"},
				CriterionIDs: []string{"AC-2"},
				Validations:  []GateIR{{Label: "input", Argv: []string{"test", "-f", "src/input.txt"}, CriterionIDs: []string{"AC-2"}}},
			},
		},
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

func TestLockPlan_RealCLI_AllowsCriterionWithoutVerificationMapping(t *testing.T) {
	dir := t.TempDir()
	r := agentflowRunnerForTest(t, dir)
	c := NewClient(r, dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}

	plan := Compile(PlanIR{
		Objective: "pin intentional host strictness", Scope: []string{"src"}, Invariants: []string{"no mutation"},
		RiskLevel: "low", RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Requirements: []RequirementIR{{
			ID: "REQ-1", Text: "the behavior is implemented",
			AcceptanceCriteria: []CriterionIR{{ID: "AC-1", Text: "the behavior works"}},
		}},
		Steps: []StepIR{{
			ID: "P1", Action: "implement behavior", Files: []string{"src/a.go"}, ExpectedDiff: []string{"behavior changes"},
			CriterionIDs: []string{"AC-1"},
			Validations:  []GateIR{{Label: "unit", Argv: []string{"true"}}},
		}},
	})
	if ds := TraceabilityDiagnostics(plan); len(ds) != 1 || ds[0].Code != "unmapped_criterion_verification" {
		t.Fatalf("host diagnostics = %+v, want unmapped_criterion_verification", ds)
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "agentflow-permissive-plan.json")
	if err := os.WriteFile(planPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatalf("Agentflow contract changed: expected v0.4 to allow no verification mapping: %v", err)
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
	err := c.LockPlan(ctx, planPath)
	if err == nil {
		t.Fatal("expected real lock-plan to reject a dependency cycle")
	}
	// Pin the failure-envelope contract the -goal repair loop depends on: a real
	// lock-plan rejection must be a *CommandError carrying an Errors[] entry with
	// Code=="validation_error". cmd/golem's classifyLockError keys repair-vs-terminal
	// on exactly that code+array; if agentflow ever reports validation failures via
	// findings[]/diagnostics[] or renames the code, the 2-attempt repair loop
	// silently never engages. Assert it against the real CLI here rather than
	// trusting the comment in types.go.
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("lock-plan rejection must be *CommandError, got %T: %v", err, err)
	}
	found := false
	for _, se := range ce.Errors {
		if se.Code == "validation_error" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("lock-plan rejection must carry a validation_error in Errors[]; got %+v", ce.Errors)
	}
}
