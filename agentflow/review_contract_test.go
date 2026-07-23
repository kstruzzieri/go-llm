package agentflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewAmendment_RealCLI(t *testing.T) {
	dir := t.TempDir()
	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	ctx := context.Background()
	if err := c.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ProbeReview(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}

	plan := Compile(PlanIR{
		Objective: "exercise review amendments", Scope: []string{"src"}, Invariants: []string{"stay in scope"},
		RiskLevel: "low", RollbackPlan: "restore the fixture", AllowedFiles: []string{"src/*"},
		Steps: []StepIR{{
			ID: "P1", Action: "repair the fixture", Files: []string{"src/a.go"}, ExpectedDiff: []string{"fixture repaired"},
			Validations: []GateIR{{Label: "true", Argv: []string{"true"}}},
		}},
	})
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatal(err)
	}
	if err := c.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := c.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RunGate(ctx, "P1", attempt, "true", []string{"true"}); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishStep(ctx, "P1", attempt); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join("docs", "ai", "state", "review-contract")
	absStateDir := filepath.Join(dir, stateDir)
	if err := os.MkdirAll(absStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(absStateDir, "findings-final.json"), []byte(`{"findings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema_version": "1.0.0", "review_run_id": "RR-20260714T120000Z-1234abcd",
		"state_dir": stateDir, "gate_status": "fail", "active_blocking": []string{"RF-1"},
		"depth_profile": "none", "amendment_ready": true,
		"findings": map[string]any{
			"counts_by_severity": map[string]int{"high": 1}, "counts_by_status": map[string]int{"accepted": 1},
			"index": []map[string]any{{
				"finding_id": "RF-1", "severity": "high", "status": "accepted", "owning_step": "P1",
				"claim": "the fixture is incomplete", "suggested_fix": "repair the fixture",
			}},
		},
		"artifacts": []map[string]string{{"path": "findings-final.json"}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(absStateDir, "review-manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := c.RecordReview(ctx, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !run.AmendmentReady || run.ReviewRunID != manifest["review_run_id"] || run.Findings.Index[0].OwningStep != "P1" {
		t.Fatalf("run = %+v", run)
	}
	ref := run.ReviewRunID + "#" + run.Findings.Index[0].FindingID
	amendmentID, err := c.AmendStep(ctx, "P1", []string{ref})
	if err != nil || amendmentID == "" {
		t.Fatalf("amendment=%q err=%v", amendmentID, err)
	}
	ledger, err := os.ReadFile(filepath.Join(dir, ".agent", "step-runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var correlated bool
	for _, line := range strings.Split(strings.TrimSpace(string(ledger)), "\n") {
		var event struct {
			Event       string `json:"event"`
			AttemptID   string `json:"attempt_id"`
			FindingRefs []struct {
				ReviewRunID string `json:"review_run_id"`
				FindingID   string `json:"finding_id"`
			} `json:"finding_refs"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "amendment_started" && event.AttemptID == amendmentID && len(event.FindingRefs) == 1 &&
			event.FindingRefs[0].ReviewRunID == run.ReviewRunID && event.FindingRefs[0].FindingID == "RF-1" {
			correlated = true
		}
	}
	if !correlated {
		t.Fatalf("finding reference %s not preserved in amendment %s", ref, amendmentID)
	}
}
