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
			ExpectedDiff: []string{"pending becomes expected"}, Validation: []string{"unit-tests", "file-exists"}, EvidenceIDs: []string{},
			Gates: []Gate{
				{Kind: "command", Run: []string{"grep", "-qx", "expected", "src/feature.txt"}},
				{Kind: "command", Run: []string{"test", "-f", "src/feature.txt"}},
			},
		}, {
			ID: "P2", Action: "confirm fixture", Files: []string{"src/feature.txt"}, Preconditions: []string{},
			ExpectedDiff: []string{"none"}, Validation: []string{"unit-tests-2"}, EvidenceIDs: []string{},
			Gates: []Gate{{Kind: "command", Run: []string{"test", "-f", "src/feature.txt"}}},
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
	// The joined-argv labels golem's recovery pairing requires, in plan order.
	// Receipts are recorded under the human validation[] labels, so these
	// assertions pin that the projection uses argv identity, not receipt names.
	wantGateLabels := []string{"grep -qx expected src/feature.txt", "test -f src/feature.txt"}
	assertCommandGates := func(state NextActionState, statuses ...string) {
		t.Helper()
		gates := state.Resumability.Gates
		if len(gates) != len(statuses) {
			t.Fatalf("state %s projected %d gates, want %d: %+v", state.State, len(gates), len(statuses), gates)
		}
		for i, gate := range gates {
			if gate.Kind != "command" || gate.Label != wantGateLabels[i] || gate.Status != statuses[i] {
				t.Fatalf("state %s gate %d = %+v, want command %q %s", state.State, i, gate, wantGateLabels[i], statuses[i])
			}
			if statuses[i] == "satisfied" && (gate.ReceiptID == nil || *gate.ReceiptID == "") {
				t.Fatalf("state %s satisfied gate %d has no receipt id: %+v", state.State, i, gate)
			}
		}
	}
	assertAttemptStates := func(state NextActionState, want string) {
		t.Helper()
		projection := state.Resumability
		if projection.Attempt == nil || projection.Step == nil ||
			projection.Attempt.State != want || projection.Step.State != want {
			t.Fatalf("state %s attempt/step = %+v / %+v, want state %q", state.State, projection.Attempt, projection.Step, want)
		}
	}
	missing := assertState("validation_missing")
	assertResumabilityShape(t, missing)
	assertCommandGates(missing, "missing", "missing")
	if missing.Resumability.Attempt == nil || missing.Resumability.Attempt.Owner != "golem" || !missing.Resumability.Attempt.Open {
		t.Fatalf("attempt projection = %+v", missing.Resumability.Attempt)
	}
	if err := client.RunGate(ctx, "P1", attempt, "unit-tests", []string{"grep", "-qx", "expected", "src/feature.txt"}); err != nil {
		t.Fatal(err)
	}
	// Crash-after-first-gate window: the satisfied gate must keep its
	// joined-argv label and plan position ahead of the still-missing gate.
	partial := assertState("validation_missing")
	assertResumabilityShape(t, partial)
	assertCommandGates(partial, "satisfied", "missing")
	assertAttemptStates(partial, "claimed")
	if err := client.RunGate(ctx, "P1", attempt, "file-exists", []string{"test", "-f", "src/feature.txt"}); err != nil {
		t.Fatal(err)
	}
	unverified := assertState("step_unverified")
	assertResumabilityShape(t, unverified)
	assertCommandGates(unverified, "satisfied", "satisfied")
	assertAttemptStates(unverified, "claimed")
	runRealAgentflow(t, runner, "verify-step", "P1", "--root", root, "--attempt", attempt, "--agent", "golem", "--json")
	uncompleted := assertState("step_uncompleted")
	assertResumabilityShape(t, uncompleted)
	assertAttemptStates(uncompleted, "verified")
	if err := client.CompleteStep(ctx, "P1", attempt); err != nil {
		t.Fatal(err)
	}
	// Mid-run interruption window: the next step projects as freshly
	// claimable with an automatic non-break-glass claim action.
	midRun := assertState("step_unclaimed")
	assertResumabilityShape(t, midRun)
	if midRun.Resumability.Attempt != nil || midRun.Resumability.Step == nil ||
		midRun.Resumability.Step.ID != "P2" || midRun.Resumability.Step.State != "pending" ||
		midRun.Resumability.Lease.State != "not_applicable" || midRun.Resumability.Lease.ExpiresAt != nil {
		t.Fatalf("mid-run step_unclaimed projection = %+v", midRun.Resumability)
	}
	assertAutomaticAction(t, midRun, "claim")
	attempt2, err := client.ClaimStep(ctx, "P2")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RecordFileChange(ctx, "P2", attempt2, "src/feature.txt"); err != nil {
		t.Fatal(err)
	}
	if err := client.RunGate(ctx, "P2", attempt2, "unit-tests-2", []string{"test", "-f", "src/feature.txt"}); err != nil {
		t.Fatal(err)
	}
	runRealAgentflow(t, runner, "verify-step", "P2", "--root", root, "--attempt", attempt2, "--agent", "golem", "--json")
	if err := client.CompleteStep(ctx, "P2", attempt2); err != nil {
		t.Fatal(err)
	}
	tail := assertState("run_unverified")
	assertResumabilityShape(t, tail)
	assertTerminalPhaseProjection(t, tail)
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
	proofPending := assertState("proof_missing")
	assertResumabilityShape(t, proofPending)
	assertTerminalPhaseProjection(t, proofPending)
	runRealAgentflow(t, runner, "build-proof", "--root", root)
	runRealAgentflow(t, runner, "verify-proof", "--root", root)
	done := assertState("complete")
	assertResumabilityShape(t, done)
	assertTerminalPhaseProjection(t, done)

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

// assertResumabilityShape pins the real CLI to the field-presence and lease
// invariants cmd/golem's validateRecoveryProjection hard-requires before it
// will report a resumable or complete state. If the CLI stops projecting any
// of these, golem fails closed everywhere while unit fixtures stay green —
// this assertion is what turns that drift into a test failure.
func assertResumabilityShape(t *testing.T, state NextActionState) {
	t.Helper()
	projection := state.Resumability
	if projection == nil || projection.Contract == nil || !projection.Contract.Locked ||
		projection.Contract.PlanSHA256 == "" || projection.Contract.ExecutionContractSHA256 == "" {
		t.Fatalf("state %s contract projection = %+v", state.State, projection)
	}
	if projection.AgentID != "golem" {
		t.Fatalf("state %s agent_id = %q", state.State, projection.AgentID)
	}
	if !projection.HasAttemptField() || !projection.HasLeaseField() || projection.Lease == nil ||
		!projection.HasGatesField() || !projection.HasRecoveryActionsField() || !projection.HasDiagnosticsField() {
		t.Fatalf("state %s omits a required resumability field: %+v", state.State, projection)
	}
	if len(projection.Diagnostics) != 0 {
		t.Fatalf("state %s projects diagnostics: %+v", state.State, projection.Diagnostics)
	}
	lease := projection.Lease
	if lease.Policy == nil || (*lease.Policy != "advisory" && *lease.Policy != "enforce") ||
		lease.TTLMinutes == nil || *lease.TTLMinutes <= 0 || lease.GraceSeconds == nil || *lease.GraceSeconds < 0 ||
		lease.Exclusive != (*lease.Policy == "enforce") {
		t.Fatalf("state %s lease = %+v", state.State, lease)
	}
}

// assertAutomaticAction pins that the named recovery action is projected
// exactly once as allowed, automatic, and not break-glass — the shape
// cmd/golem's validateAutomaticRecoveryAction requires before mutating.
func assertAutomaticAction(t *testing.T, state NextActionState, action string) {
	t.Helper()
	var matched *ResumabilityRecoveryAction
	for i := range state.Resumability.RecoveryActions {
		if state.Resumability.RecoveryActions[i].Action != action {
			continue
		}
		if matched != nil {
			t.Fatalf("state %s projects duplicate %q actions", state.State, action)
		}
		matched = &state.Resumability.RecoveryActions[i]
	}
	if matched == nil || !matched.Allowed || !matched.Automatic || matched.BreakGlass == nil || *matched.BreakGlass {
		t.Fatalf("state %s %q action = %+v, want allowed automatic non-break-glass", state.State, action, matched)
	}
}

// assertTerminalPhaseProjection pins that the run tail projects no step, no
// attempt, and no allowed recovery action: Agentflow's action vocabulary
// covers attempt lifecycle only, which is why cmd/golem intentionally has no
// recovery-action requirement before FinishRun in these states.
func assertTerminalPhaseProjection(t *testing.T, state NextActionState) {
	t.Helper()
	projection := state.Resumability
	if projection.Step != nil || projection.Attempt != nil {
		t.Fatalf("state %s projects step/attempt: %+v / %+v", state.State, projection.Step, projection.Attempt)
	}
	if projection.Lease.State != "not_applicable" || projection.Lease.ExpiresAt != nil {
		t.Fatalf("state %s lease = %+v", state.State, projection.Lease)
	}
	for _, action := range projection.RecoveryActions {
		if action.Allowed {
			t.Fatalf("state %s projects allowed action %+v", state.State, action)
		}
	}
}

// canonicalPlanDigestForContract mirrors Agentflow's plan-binding digest for
// THIS fixture only: Go's json.Marshal HTML-escapes <, >, and & and formats
// floats differently from Python's json.dumps, so this helper is only valid
// while the fixture plan stays ASCII-only with integer-only numbers. The
// production Python-parity canonicalizer lives in cmd/golem and is pinned
// against the real CLI with adversarial content by
// TestAgentflowResumeStatusAndProof_RealCLI.
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
