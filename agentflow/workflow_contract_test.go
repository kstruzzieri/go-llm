//go:build agentflow_integration

package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowRecommendation_RealCLI_RoutesAndStaysReadOnly(t *testing.T) {
	dir := t.TempDir()
	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	ctx := context.Background()
	if err := c.ProbeWorkflow(ctx); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		brief TaskBrief
		want  string
	}{
		{"docs only", taskBrief("docs", "low", []string{"docs/readme.md"}, "", "", false), "docs-only"},
		{"bounded bugfix", taskBrief("bugfix", "low", []string{"a.go", "a_test.go"}, "local", "s", false), "small-bugfix"},
		{"medium feature", taskBrief("feature", "medium", nil, "", "", false), "medium-feature"},
		{"broad feature", taskBrief("feature", "low", nil, "cross_cutting", "", false), "large-feature"},
		{"security sensitive", taskBrief("feature", "low", nil, "", "", true), "high-risk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recommendation, err := c.RecommendWorkflow(ctx, tt.brief, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if recommendation.Recommended.Profile != tt.want || recommendation.Selected.Profile != tt.want ||
				recommendation.Contract.WorkflowProfile != tt.want {
				t.Fatalf("route = recommended:%s selected:%s candidate:%s, want %s",
					recommendation.Recommended.Profile, recommendation.Selected.Profile,
					recommendation.Contract.WorkflowProfile, tt.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent")); !os.IsNotExist(err) {
		t.Fatalf("recommendation mutated .agent: %v", err)
	}
}

func TestWorkflowRecommendation_RealCLI_UnderspecifiedBriefAndOverride(t *testing.T) {
	dir := t.TempDir()
	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	ctx := context.Background()
	brief := TaskBrief{SchemaVersion: TaskBriefSchemaVersion, TaskType: "bugfix", DeclaredRisk: "low"}

	floor, err := c.RecommendWorkflow(ctx, brief, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if floor.Selected.Profile != "medium-feature" ||
		!containsString(floor.Signals, "candidate_files=unknown") ||
		!containsString(floor.Signals, "blast_radius=unknown") ||
		!containsString(floor.Signals, "declared_size=unknown") {
		t.Fatalf("underspecified route = %+v", floor)
	}

	override, err := c.RecommendWorkflow(ctx, brief, "high-risk", "touches shared authorization")
	if err != nil {
		t.Fatal(err)
	}
	if override.Override == nil || override.Selected.Profile != "high-risk" ||
		override.Contract.ReviewDepth != "deep" || !override.Contract.ProofPolicy.RequireReviewRun {
		t.Fatalf("override = %+v", override)
	}
	if _, err := c.RecommendWorkflow(ctx, brief, "high-risk", ""); err == nil ||
		!strings.Contains(err.Error(), "override_requires_reason") {
		t.Fatalf("override without reason err = %v", err)
	}
}

func TestWorkflowContract_RealCLI_MaterializesReturnedCandidate(t *testing.T) {
	dir := t.TempDir()
	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	recommendation, err := c.RecommendWorkflow(context.Background(),
		taskBrief("feature", "medium", nil, "", "", false), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MaterializeWorkflowContract(context.Background(), recommendation); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, ".agent", "workflow.contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(written, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recommendation.CandidateJSON(), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("materialized contract = %v, want candidate %v", got, want)
	}
}

func TestWorkflowContract_RealCLI_DeepRouteRequiresAdequateReviewRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Compile(PlanIR{
		Objective: "exercise deep review proof", Scope: []string{"src"}, Invariants: []string{"stay in scope"},
		RiskLevel: "high", RollbackPlan: "restore the fixture", AllowedFiles: []string{"src/*"},
		Steps: []StepIR{{
			ID: "P1", Action: "change the fixture", Files: []string{"src/a.go"}, ExpectedDiff: []string{"fixture changes"},
			Validations: []GateIR{
				{Label: "unit-tests", Argv: []string{"true"}},
				{Label: "security-scan", Argv: []string{"true"}},
			},
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
	gitInitWorkflowContract(t, dir)

	c := NewClient(agentflowRunnerForTest(t, dir), dir)
	ctx := context.Background()
	recommendation, err := c.RecommendWorkflow(ctx,
		taskBrief("feature", "high", []string{"src/a.go"}, "local", "s", false), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatal(err)
	}
	if err := c.MaterializeWorkflowContract(ctx, recommendation); err != nil {
		t.Fatal(err)
	}
	if err := c.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := c.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordFileChange(ctx, "P1", attempt, "src/a.go"); err != nil {
		t.Fatal(err)
	}
	for i, gate := range plan.Steps[0].Gates {
		label := plan.Steps[0].Validation[i]
		if err := c.RunGate(ctx, "P1", attempt, label, gate.Run); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.FinishStep(ctx, "P1", attempt); err != nil {
		t.Fatal(err)
	}
	_, err = c.FinishRun(ctx)
	var stopped *FinishRunError
	if !errors.As(err, &stopped) || stopped.StoppedAt != "build-proof" {
		t.Fatalf("finish-run err = %#v, want proof build to stop on required review", err)
	}
	if !strings.Contains(stopped.Error(), "required_review_satisfied: review_depth=deep requires a review run") {
		t.Fatalf("finish-run omitted actionable review requirement: %v", stopped)
	}
	proofBytes, err := os.ReadFile(filepath.Join(dir, ".agent", "proof-pack.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proof struct {
		Checks []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(proofBytes, &proof); err != nil {
		t.Fatal(err)
	}
	for _, check := range proof.Checks {
		if check.ID == "required_review_satisfied" && check.Status == "failed" &&
			strings.Contains(check.Message, "review_depth=deep requires a review run") {
			return
		}
	}
	t.Fatalf("proof checks = %+v, want failed required_review_satisfied", proof.Checks)
}

func taskBrief(taskType, risk string, files []string, blast, size string, security bool) TaskBrief {
	brief := TaskBrief{SchemaVersion: TaskBriefSchemaVersion, TaskType: taskType, DeclaredRisk: risk}
	if files != nil {
		copyFiles := append([]string(nil), files...)
		brief.CandidateFiles = &copyFiles
	}
	if blast != "" {
		brief.BlastRadius = &blast
	}
	if size != "" {
		brief.DeclaredSize = &size
	}
	if security {
		brief.SecuritySensitive = &security
	}
	return brief
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func gitInitWorkflowContract(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=agentflow-contract", "GIT_AUTHOR_EMAIL=agentflow-contract@example.com",
			"GIT_COMMITTER_NAME=agentflow-contract", "GIT_COMMITTER_EMAIL=agentflow-contract@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("add", "-A")
	run("commit", "-m", "fixture baseline")
}
