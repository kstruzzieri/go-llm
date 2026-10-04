package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// agentflowTestSource decides how integration tests reach Agentflow. mode is
// GO_LLM_REQUIRE_AGENTFLOW: empty skips when Agentflow is absent; "installed"
// or "source" turns absence or a mode mismatch into a failure, so a CI step
// cannot pass by skipping or by testing the other mode.
func agentflowTestSource(mode, src string, installed bool) (useSrc, skip bool, err error) {
	switch mode {
	case "":
	case "installed":
		if src != "" {
			return false, false, errors.New("GO_LLM_REQUIRE_AGENTFLOW=installed but AGENTFLOW_SRC is set")
		}
		if !installed {
			return false, false, errors.New("GO_LLM_REQUIRE_AGENTFLOW=installed but agentflow is not on PATH")
		}
	case "source":
		if src == "" {
			return false, false, errors.New("GO_LLM_REQUIRE_AGENTFLOW=source but AGENTFLOW_SRC is empty")
		}
	default:
		return false, false, fmt.Errorf("GO_LLM_REQUIRE_AGENTFLOW=%q, want installed or source", mode)
	}
	if src != "" {
		return true, false, nil
	}
	return false, !installed, nil
}

// agentflowRunnerForTest honors GO_LLM_REQUIRE_AGENTFLOW and the explicit
// AGENTFLOW_SRC checkout, otherwise uses an installed binary or skips. The
// chosen CLI must pass the AgentFlow 1.x version gate: a non-1.x install skips
// (or fails under GO_LLM_REQUIRE_AGENTFLOW) instead of failing on a raw schema
// rejection (#612). CI's agentflow-compat job selects real-CLI tests by name,
// so a test using this must be named Test*_RealCLI or Test*_RealCLI_<scenario>.
func agentflowRunnerForTest(t *testing.T, dir string) Runner {
	t.Helper()
	src := os.Getenv("AGENTFLOW_SRC")
	_, lookErr := exec.LookPath("agentflow")
	useSrc, skip, err := agentflowTestSource(os.Getenv("GO_LLM_REQUIRE_AGENTFLOW"), src, lookErr == nil)
	switch {
	case err != nil:
		t.Fatal(err)
	case skip:
		t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	}
	runner := NewExecRunner(dir)
	if useSrc {
		runner = NewSrcExecRunner(dir, src)
	}
	if err := NewClient(runner, dir).CheckVersion(context.Background()); err != nil {
		if os.Getenv("GO_LLM_REQUIRE_AGENTFLOW") != "" {
			t.Fatalf("agentflow is not usable for the real-CLI tests: %v", err)
		}
		t.Skipf("agentflow is not AgentFlow 1.x (%v); install AgentFlow 1.x or set AGENTFLOW_SRC=<1.x checkout>", err)
	}
	return runner
}

func TestAgentflowRunnerForTest_PrefersSourceOverride(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "agentflow"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Source mode runs `python3 -m agentflow --version` for the version gate.
	if err := os.WriteFile(filepath.Join(binDir, "python3"), []byte("#!/bin/sh\necho 'agentflow 1.0.0'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := writeSourceCheckoutFixture(t)
	t.Setenv("PATH", binDir)
	t.Setenv("AGENTFLOW_SRC", checkout)
	t.Setenv("GO_LLM_REQUIRE_AGENTFLOW", "")

	runner, ok := agentflowRunnerForTest(t, t.TempDir()).(*ExecRunner)
	if !ok {
		t.Fatalf("runner = %T, want *ExecRunner", runner)
	}
	bin, argv, env := runner.commandFor([]string{"--version"})
	if bin != "python3" || !reflect.DeepEqual(argv, []string{"-P", "-m", "agentflow", "--version"}) ||
		!reflect.DeepEqual(env, []string{"PYTHONPATH=" + filepath.Join(checkout, "src")}) {
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

func TestLockPlan_RealCLI_AcceptsTypedDesignDecisionTraceability(t *testing.T) {
	dir := t.TempDir()
	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}

	plan := Compile(PlanIR{
		Objective: "lock typed design decisions", Scope: []string{"src"}, Invariants: []string{"no mutation"},
		RiskLevel: "low", RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Steps: []StepIR{{
			ID: "P1", Action: "implement the selected design", Files: []string{"src/a.go"},
			ExpectedDiff: []string{"selected design is implemented"},
			Validations:  []GateIR{{Label: "unit", Argv: []string{"true"}}},
		}},
	})
	references := []string{"docs/agent-workflow.md", "docs/golem-integration.md"}
	decisions := []DesignDecision{
		{ID: "DD-1", Text: "Use the existing receipt ledger.", References: &references},
		{ID: "DD-UNSELECTED", Text: "An optional unselected declaration."},
	}
	selected := []string{"DD-1"}
	plan.SchemaVersion = "1.0.0"
	plan.DesignDecisions = &decisions
	plan.Steps[0].DesignDecisionIDs = &selected
	if ds := TraceabilityDiagnostics(plan); len(ds) != 0 {
		t.Fatalf("typed design plan failed local pre-check: %+v", ds)
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "design-plan.json")
	if err := os.WriteFile(planPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatalf("real lock-plan rejected typed design traceability: %v", err)
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
		t.Fatalf("Agentflow contract changed: expected v1.0 to allow no verification mapping: %v", err)
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
