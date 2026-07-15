//go:build agentflow_integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
)

// smokeIR is the authored PlanIR for the end-to-end -goal smoke: a single step
// that targets src/answer.txt with a grep validation, matching the shape the
// testdata/agentflow fixture locks. It carries the requirement/criterion
// mappings the authoring gate now demands, so the smoke exercises the full
// traceable path against the real CLI.
func smokeIR(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(agentflow.PlanIR{
		TaskType:     "feature",
		Objective:    "ensure the answer token",
		Scope:        []string{"src"},
		Invariants:   []string{"only src/answer.txt changes"},
		RiskLevel:    "low",
		RollbackPlan: "git checkout -- .",
		AllowedFiles: []string{"src/*"},
		Requirements: []agentflow.RequirementIR{{
			ID: "REQ-1", Text: "src/answer.txt carries the expected token",
			AcceptanceCriteria: []agentflow.CriterionIR{{ID: "AC-1", Text: "grep finds the expected token"}},
		}},
		Steps: []agentflow.StepIR{{
			ID:           "P1",
			Action:       "ensure src/answer.txt contains the expected token",
			Files:        []string{"src/answer.txt"},
			ExpectedDiff: []string{"answer.txt changes pending to expected"},
			CriterionIDs: []string{"AC-1"},
			Validations:  []agentflow.GateIR{{Label: "unit-tests", Argv: []string{"grep", "-q", "expected", "src/answer.txt"}, CriterionIDs: []string{"AC-1"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGoalAuthor_RealCLI drives the real runAgentflowAuthor end to end against
// the fixture: a scripted submit_plan compiles, pre-checks, and locks a plan with
// the real agentflow CLI. It then re-locks that same artifact to pin the #209
// execute-separately handoff (spec §9 re-lock idempotence). agentflowRunnerOrSkip
// (from the same-tag smoke file) owns the skip when the CLI is unavailable.
func TestGoalAuthor_RealCLI(t *testing.T) {
	src := os.Getenv("AGENTFLOW_SRC")
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	runner := agentflowRunnerOrSkip(t, dir)

	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(smokeIR(t))}}
	sess := newTestSession(t, caller, dir)

	var out, errb bytes.Buffer
	// runAgentflowAuthor builds its own client from f.agentflowSrc the same way
	// agentflowRunnerOrSkip built runner, so both reach the same CLI. stdin
	// answers the interactive "Lock this plan? [y/N]" approval prompt.
	f := flags{goal: "ensure the answer token", goalSet: true, agentflowSrc: src}
	if err := runAgentflowAuthor(context.Background(), strings.NewReader("y\n"), &out, &errb, nil, sess, f, dir); err != nil {
		t.Fatalf("author flow: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "Lock this plan?") {
		t.Fatalf("approval prompt missing from output:\n%s", out.String())
	}

	lockedPath := filepath.Join(dir, ".agent", "plan.lock.json")
	b, err := os.ReadFile(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	var locked struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal(b, &locked); err != nil {
		t.Fatalf("locked plan is not valid JSON: %v\n%s", err, b)
	}
	if !locked.Locked {
		t.Errorf(".agent/plan.lock.json is not locked: %s", b)
	}

	// The locked bytes must satisfy PreflightP0: this proves the compiler's gates[]
	// survived the real lock, so the execute-separately handoff to #209 (which runs
	// PreflightP0 before driving) accepts what the author produced.
	var lockedPlan agentflow.Plan
	if err := json.Unmarshal(b, &lockedPlan); err != nil {
		t.Fatalf("locked plan does not unmarshal into agentflow.Plan: %v\n%s", err, b)
	}
	if err := agentflow.PreflightP0(&lockedPlan); err != nil {
		t.Fatalf("locked plan fails PreflightP0 (gates[] did not survive the lock): %v", err)
	}

	// Re-lock idempotence (spec §9): the locked artifact must survive a fresh
	// same-file LockPlan, proving the `golem -plan .agent/plan.lock.json`
	// execute-separately path can re-lock what the author produced.
	if err := agentflow.NewClient(runner, dir).LockPlan(context.Background(), lockedPath); err != nil {
		t.Fatalf("same-file re-lock: %v", err)
	}
}
