//go:build agentflow_integration

package agentflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResumabilityProjection_RealCLI_AllStatesAndDigests(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "feature.txt"), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".agent/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		SchemaVersion: "0.3.0", Objective: "exercise every next-action state", Scope: []string{"src"},
		NonGoals: []string{}, Invariants: []string{"only the fixture changes"}, RiskLevel: "low",
		DriftBudget:  DriftBudget{UnrelatedEdits: 0, NewDependencies: 0, FormattingDrift: "minimal", ArchitectureDrift: "requires_approval"},
		AllowedFiles: []string{"src/feature.txt"}, BlockedFiles: []string{}, ValidationGates: []string{"unit-tests"},
		RollbackPlan: "git checkout -- .", EvidenceIDs: []string{},
		Steps: []Step{{
			ID: "P1", Action: "write expected", Files: []string{"src/feature.txt"}, Preconditions: []string{},
			ExpectedDiff: []string{"pending becomes expected"}, Validation: []string{"unit-tests"}, EvidenceIDs: []string{},
			Gates: []Gate{{Kind: "command", Run: []string{"grep", "-qx", "expected", "src/feature.txt"}}},
		}},
	}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	initResumabilityGit(t, root)

	runner := agentflowRunnerForTest(t, root)
	client := NewOwnedClient(runner, root, "golem")
	ctx := context.Background()
	seen := map[string]bool{}
	assertState := func(want string) NextActionState {
		t.Helper()
		before := snapshotAgentflowTree(t, root)
		state, err := client.NextAction(ctx)
		if err != nil {
			t.Fatalf("NextAction(%s): %v", want, err)
		}
		after := snapshotAgentflowTree(t, root)
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("next-action mutated .agent for state %s", want)
		}
		if state.State != want {
			t.Fatalf("state = %q, want %q; reason=%s diagnostics=%v", state.State, want, state.Reason, state.Diagnostics)
		}
		seen[want] = true
		return state
	}

	assertState("uninitialized")
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}
	assertState("plan_unlocked")
	if err := client.LockPlan(ctx, planPath); err != nil {
		t.Fatal(err)
	}
	assertState("execution_uninitialized")
	if err := client.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	ready := assertState("step_unclaimed")
	if ready.Resumability == nil || ready.Resumability.Contract == nil || ready.Resumability.AgentID != "golem" {
		t.Fatalf("owned resumability projection = %+v", ready.Resumability)
	}
	lockedPlan, err := os.ReadFile(filepath.Join(root, ".agent", "plan.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantPlanDigest := canonicalPlanDigestForContract(t, lockedPlan)
	if ready.Resumability.Contract.PlanSHA256 != wantPlanDigest {
		t.Fatalf("plan digest = %s, want %s", ready.Resumability.Contract.PlanSHA256, wantPlanDigest)
	}
	execution, err := os.ReadFile(filepath.Join(root, ".agent", "execution.contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantExecutionDigest := fmt.Sprintf("%x", sha256.Sum256(execution))
	if ready.Resumability.Contract.ExecutionContractSHA256 != wantExecutionDigest {
		t.Fatalf("execution digest = %s, want %s", ready.Resumability.Contract.ExecutionContractSHA256, wantExecutionDigest)
	}

	attempt, err := client.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "feature.txt"), []byte("expected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("file_receipts_missing")
	if err := client.RecordFileChange(ctx, "P1", attempt, "src/feature.txt"); err != nil {
		t.Fatal(err)
	}
	missing := assertState("validation_missing")
	if missing.Resumability == nil || len(missing.Resumability.Gates) != 1 ||
		missing.Resumability.Gates[0].Label != "grep -qx expected src/feature.txt" ||
		missing.Resumability.Gates[0].Status != "missing" {
		t.Fatalf("command gate projection = %+v", missing.Resumability)
	}
	if missing.Resumability.Attempt == nil || missing.Resumability.Attempt.Owner != "golem" || !missing.Resumability.Attempt.Open {
		t.Fatalf("attempt projection = %+v", missing.Resumability.Attempt)
	}
	if err := client.RunGate(ctx, "P1", attempt, "unit-tests", []string{"grep", "-qx", "expected", "src/feature.txt"}); err != nil {
		t.Fatal(err)
	}
	assertState("step_unverified")
	runRealAgentflow(t, runner, "verify-step", "P1", "--root", root, "--attempt", attempt, "--agent", "golem", "--json")
	assertState("step_uncompleted")
	if err := client.CompleteStep(ctx, "P1", attempt); err != nil {
		t.Fatal(err)
	}
	assertState("run_unverified")
	rogue := filepath.Join(root, "rogue.txt")
	if err := os.WriteFile(rogue, []byte("out of scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("drift_failing")
	if err := os.Remove(rogue); err != nil {
		t.Fatal(err)
	}
	assertState("run_unverified")
	runRealAgentflow(t, runner, "verify-run", "--root", root, "--json")
	assertState("proof_missing")
	runRealAgentflow(t, runner, "build-proof", "--root", root)
	runRealAgentflow(t, runner, "verify-proof", "--root", root)
	assertState("complete")

	proofPath := filepath.Join(root, ".agent", "proof-pack.json")
	proofBytes, err := os.ReadFile(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	lockedBytes, err := os.ReadFile(filepath.Join(root, ".agent", "plan.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var changed map[string]any
	if err := json.Unmarshal(lockedBytes, &changed); err != nil {
		t.Fatal(err)
	}
	changed["objective"] = "changed after proof"
	changedBytes, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agent", "plan.lock.json"), changedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("proof_stale")
	if err := os.WriteFile(filepath.Join(root, ".agent", "plan.lock.json"), lockedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("complete")
	if err := os.WriteFile(proofPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("proof_failing")
	if err := os.WriteFile(proofPath, proofBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	assertState("complete")

	invalidRoot := t.TempDir()
	invalidClient := NewOwnedClient(agentflowRunnerForTest(t, invalidRoot), invalidRoot, "golem")
	if err := invalidClient.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidRoot, ".agent", "plan.lock.json"), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid, err := invalidClient.NextAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if invalid.State != "state_invalid" {
		t.Fatalf("invalid state = %q, want state_invalid", invalid.State)
	}
	seen[invalid.State] = true

	wantStates := []string{
		"uninitialized", "plan_unlocked", "execution_uninitialized", "state_invalid", "step_unclaimed",
		"file_receipts_missing", "validation_missing", "step_unverified", "step_uncompleted", "drift_failing",
		"run_unverified", "proof_missing", "proof_stale", "proof_failing", "complete",
	}
	for _, state := range wantStates {
		if !seen[state] {
			t.Errorf("real Agentflow fixture did not emit %s", state)
		}
	}
}

func runRealAgentflow(t *testing.T, runner Runner, args ...string) []byte {
	t.Helper()
	out, errOut, exit, err := runner.Run(context.Background(), args, nil)
	if err != nil || exit != 0 {
		t.Fatalf("agentflow %v: exit=%d err=%v stderr=%s stdout=%s", args, exit, err, errOut, out)
	}
	return out
}

func snapshotAgentflowTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	agentDir := filepath.Join(root, ".agent")
	err := filepath.WalkDir(agentDir, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[rel] = fmt.Sprintf("%04o:%x", info.Mode().Perm(), sha256.Sum256(data))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return snapshot
}

func canonicalPlanDigestForContract(t *testing.T, data []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var plan map[string]any
	if err := decoder.Decode(&plan); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("trailing plan JSON: %v", err)
	}
	delete(plan, "locked")
	delete(plan, "locked_at")
	canonical, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical))
}

func initResumabilityGit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "agentflow@example.com"},
		{"config", "user.name", "agentflow"},
		{"add", "-A"},
		{"commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
