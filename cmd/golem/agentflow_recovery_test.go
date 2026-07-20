package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

type recoveryRunner struct {
	payload []byte
	calls   [][]string
}

type failingRecoveryRunner struct{ stderr []byte }

func (r *failingRecoveryRunner) Run(_ context.Context, _ []string, _ []byte) ([]byte, []byte, int, error) {
	return nil, append([]byte(nil), r.stderr...), 1, nil
}

func (r *recoveryRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return append([]byte(nil), r.payload...), nil, 0, nil
}

func TestResumeDispositionCoversAgentflowStates(t *testing.T) {
	tests := map[string]int{
		"uninitialized": 3, "plan_unlocked": 3, "execution_uninitialized": 3,
		"state_invalid": 3, "step_unclaimed": 2, "file_receipts_missing": 3,
		"validation_missing": 2, "step_unverified": 2, "step_uncompleted": 2,
		"drift_failing": 3, "run_unverified": 2, "proof_missing": 2,
		"proof_stale": 3, "proof_failing": 3, "complete": 0, "future_state": 3,
	}
	for state, exit := range tests {
		t.Run(state, func(t *testing.T) {
			if got := resumeDisposition(state); got.exitCode != exit {
				t.Fatalf("exit = %d, want %d", got.exitCode, exit)
			}
		})
	}
}

func TestAgentflowStatus_HumanIsReadOnlyAndOwned(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, ".agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(agentDir, "state.json")
	before := []byte("durable\n")
	if err := os.WriteFile(statePath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	planBytes, err := json.Marshal(agentflow.Plan{Steps: []agentflow.Step{{
		ID: "P1", Validation: []string{"unit-tests"},
		Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./..."}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	planBytes = append(planBytes[:len(planBytes)-1], []byte(`,"future_numeric":Infinity}`)...)
	if err := os.WriteFile(filepath.Join(agentDir, "plan.lock.json"), planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"state":"validation_missing","reason":"gate missing","blocking":true,"command":"sh","args":["-c","touch /tmp/owned"],"step_id":"P1","gate":"unit-tests","diagnostics":["missing gate"],"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem","step":{"id":"P1","state":"claimed","completed":false},"attempt":{"id":"A1","state":"claimed","owner":"golem","open":true},"lease":{"policy":"advisory","ttl_minutes":30,"grace_seconds":0,"expires_at":null,"state":"no_deadline","exclusive":false},"gates":[{"kind":"command","label":"go test ./...","status":"missing","receipt_id":null,"evidence_id":null}],"recovery_actions":[{"action":"continue","allowed":true,"automatic":true,"break_glass":false,"reason":"owner holds lease"}],"diagnostics":[]}}`)
	runner := &recoveryRunner{payload: payload}
	var out bytes.Buffer

	err = runAgentflowStatusWithRunner(context.Background(), &out, root, false, runner)
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 2 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"state: validation_missing", "reason: gate missing", "step: P1", "attempt: A1 owner=golem",
		"lease: policy=advisory state=no_deadline", "gate: command go test ./... (missing)",
		"diagnostic: missing gate", "advisory (display only):", "resume:",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	wantCalls := [][]string{{"next-action", "--root", root, "--json", "--agent", "golem"}}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", runner.calls, wantCalls)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("proof state changed: %q err=%v", after, err)
	}
}

func TestAgentflowStatus_JSONRelaysRawProjection(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("{\"state\":\"uninitialized\",\"unknown\":{\"kept\":true}}\n"),
		[]byte("{\"state\":\"complete\",\"resumability\":null,\"unknown\":[1]}\n"),
	} {
		runner := &recoveryRunner{payload: payload}
		var out bytes.Buffer
		err := runAgentflowStatusWithRunner(context.Background(), &out, t.TempDir(), true, runner)
		var statusErr *agentflowStatusExit
		if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
			t.Fatalf("err = %v", err)
		}
		if !bytes.Equal(out.Bytes(), payload) {
			t.Fatalf("output = %q, want %q", out.Bytes(), payload)
		}
	}
}

func TestAgentflowStatus_CompleteRequiresAndSummarizesProof(t *testing.T) {
	fixture := newResumeFixture(t)
	root := fixture.root
	proof := `{"checks":[{"status":"passed"},{"status":"warning"},{"status":"passed"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	state := fixture.noAttemptState(t, "complete", false)
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recoveryRunner{payload: payload}
	var out bytes.Buffer
	if err := runAgentflowStatusWithRunner(context.Background(), &out, root, false, runner); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proof: verified", filepath.Join(root, ".agent", "proof-pack.json"), "checks: passed=2 warning=1 failed=0 not_run=0 skipped=0 not_applicable=0 total=3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestAgentflowStatus_NonCompleteNeverReadsPresentProof(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recoveryRunner{payload: []byte(`{"state":"proof_failing","reason":"proof verification failed","blocking":true}`)}
	var out bytes.Buffer
	err := runAgentflowStatusWithRunner(context.Background(), &out, root, false, runner)
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(out.String(), "proof: verified") {
		t.Fatalf("stale proof reported verified:\n%s", out.String())
	}
}

func TestAgentflowStatus_UnsafeActionableProjectionExitsBlocked(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(projection map[string]any)
	}{
		{name: "foreign advisory owner", mutate: func(projection map[string]any) {
			projection["attempt"].(map[string]any)["owner"] = "other"
			lease := projection["lease"].(map[string]any)
			lease["policy"], lease["exclusive"] = "advisory", false
		}},
		{name: "expired lease", mutate: func(projection map[string]any) {
			projection["lease"].(map[string]any)["state"] = "expired"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			test.mutate(fixture.projection())
			payload, err := json.Marshal(fixture.payload)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
			var statusErr *agentflowStatusExit
			if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
				t.Fatalf("err = %v, want status exit 3", err)
			}
			if !strings.Contains(out.String(), "resume: blocked") {
				t.Fatalf("unsafe projection rendered as resumable:\n%s", out.String())
			}
		})
	}
}

func TestAgentflowStatus_FiniteEnforcedRecoveryIsBlocked(t *testing.T) {
	fixture := newResumeFixture(t)
	state := fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
	})
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 || !strings.Contains(out.String(), "resume: blocked") {
		t.Fatalf("err = %v, want blocked status exit 3\n%s", err, out.String())
	}
}

func TestAgentflowStatus_RejectsUnknownGateStatusForAdvisoryLease(t *testing.T) {
	fixture := newResumeFixture(t)
	lease := fixture.projection()["lease"].(map[string]any)
	lease["policy"], lease["state"], lease["expires_at"], lease["exclusive"] = "advisory", "no_deadline", nil, false
	state := fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "failed"},
		{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
	})
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 || !strings.Contains(out.String(), "resume: blocked") {
		t.Fatalf("unknown advisory gate status was not blocked: err=%v\n%s", err, out.String())
	}
}

func TestAgentflowStatus_CompleteRequiresResumabilityProjection(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.payload["state"] = "complete"
	delete(fixture.payload, "resumability")
	payload, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, ".agent", "proof-pack.json"), []byte(`{"checks":[{"status":"passed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
		t.Fatalf("err = %v, want status exit 3", err)
	}
	if strings.Contains(out.String(), "proof: verified") {
		t.Fatalf("malformed complete projection reported verified proof:\n%s", out.String())
	}
}

func TestAgentflowStatus_ProofSummaryFailureUsesBlockedExit(t *testing.T) {
	for name, proof := range map[string]string{
		"malformed":    `{`,
		"failed check": `{"checks":[{"status":"failed"}]}`,
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", name, jsonOutput), func(t *testing.T) {
				fixture := newResumeFixture(t)
				state := fixture.noAttemptState(t, "complete", false)
				payload, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, jsonOutput, &recoveryRunner{payload: payload})
				var statusErr *agentflowStatusExit
				if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
					t.Fatalf("err = %v, want status exit 3", err)
				}
				if jsonOutput {
					if !bytes.Equal(out.Bytes(), payload) {
						t.Fatalf("JSON output = %q, want exact payload %q", out.Bytes(), payload)
					}
				} else if strings.Contains(out.String(), "proof: verified") ||
					!strings.Contains(out.String(), "resume: blocked") || !strings.Contains(out.String(), "blocking: true") {
					t.Fatalf("proof inconsistency was not rendered blocked:\n%s", out.String())
				}
			})
		}
	}
}

func TestAgentflowStatus_JSONRelaysMalformedProjectionAndFailsClosed(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	_ = fixture.noAttemptState(t, "step_unclaimed", true)
	fixture.projection()["gates"] = nil
	payload, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, true, &recoveryRunner{payload: payload})
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
		t.Fatalf("err = %v, want status exit 3", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("JSON output = %q, want exact payload %q", out.Bytes(), payload)
	}
}

func TestAgentflowStatus_EmptySuccessfulOutputFailsClosed(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOutput), func(t *testing.T) {
			var out bytes.Buffer
			err := runAgentflowStatusWithRunner(context.Background(), &out, t.TempDir(), jsonOutput, &recoveryRunner{})
			var statusErr *agentflowStatusExit
			if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
				t.Fatalf("err = %v, want status exit 3", err)
			}
			if jsonOutput && out.Len() != 0 {
				t.Fatalf("JSON output = %q, want exact empty output", out.Bytes())
			}
			if !jsonOutput && (!strings.Contains(out.String(), "blocking: true") || !strings.Contains(out.String(), "resume: blocked")) {
				t.Fatalf("empty output was not rendered blocked:\n%s", out.String())
			}
		})
	}
}

func TestValidateAutomaticRecoveryActionRequiresExplicitNonBreakGlass(t *testing.T) {
	explicitFalse := false
	for _, action := range []string{"claim", "continue"} {
		t.Run(action, func(t *testing.T) {
			projection := &agentflow.ResumabilityProjection{RecoveryActions: []agentflow.ResumabilityRecoveryAction{{
				Action: action, Allowed: true, Automatic: true,
			}}}
			if err := validateAutomaticRecoveryAction(projection, action); err == nil {
				t.Fatal("missing break_glass unexpectedly authorized recovery")
			}
			projection.RecoveryActions[0].BreakGlass = &explicitFalse
			if err := validateAutomaticRecoveryAction(projection, action); err != nil {
				t.Fatalf("explicit non-break-glass action rejected: %v", err)
			}
		})
	}
}

func TestAgentflowStatus_ResumeDispositionRequiresGolemPlanPreflight(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	plan := recoveryPlan()
	plan.Steps[0].Gates[0].Kind = "inspection"
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, ".agent", "plan.lock.json"), planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	state := fixture.noAttemptState(t, "step_unclaimed", true)
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 || !strings.Contains(out.String(), "resume: blocked") {
		t.Fatalf("status did not block a plan resume would reject: err=%v\n%s", err, out.String())
	}
}

func TestAgentflowRecoveryOutputEscapesTerminalControls(t *testing.T) {
	evil := "P1\nproof: verified\x1b[2J\u0085"
	state := agentflow.NextActionState{
		State: "future\x1b[2J", Reason: evil, Command: evil, Args: []string{evil}, Diagnostics: []string{evil},
		Resumability: &agentflow.ResumabilityProjection{
			Attempt: &agentflow.ResumabilityAttempt{ID: evil, Owner: evil, Open: true},
			Lease:   &agentflow.ResumabilityLease{State: evil, ExpiresAt: &evil},
			Gates:   []agentflow.ResumabilityGate{{Kind: evil, Label: evil, Status: evil}},
		},
	}
	var rendered bytes.Buffer
	renderAgentflowStatus(&rendered, state, nil, recoveryDisposition{failClosed, 3, evil})
	for _, control := range []string{"\x1b", "\u0085", "\nproof: verified\n"} {
		if strings.Contains(rendered.String(), control) {
			t.Fatalf("raw terminal control or forged line reached status output: %q", rendered.String())
		}
	}

	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var report bytes.Buffer
	reportAgentflowRecovery(context.Background(), &report, agentflow.NewOwnedClient(&recoveryRunner{payload: payload}, t.TempDir(), "golem"))
	if strings.Contains(report.String(), "\x1b") || strings.Contains(report.String(), "\u0085") || strings.Contains(report.String(), "\nproof: verified\n") {
		t.Fatalf("raw terminal control or forged line reached recovery report: %q", report.String())
	}
	if !strings.Contains(report.String(), "display only") {
		t.Fatalf("advisory command was not labeled display-only: %q", report.String())
	}

	var resumeError bytes.Buffer
	reportAgentflowResumeError(&resumeError, errors.New(evil))
	if strings.Contains(resumeError.String(), "\x1b") || strings.Contains(resumeError.String(), "\u0085") || strings.Contains(resumeError.String(), "\nproof: verified\n") {
		t.Fatalf("raw terminal control or forged line reached resume error: %q", resumeError.String())
	}

	statusErr := runAgentflowStatusWithRunner(context.Background(), io.Discard, t.TempDir(), false, &failingRecoveryRunner{stderr: []byte(evil)})
	if statusErr == nil || strings.Contains(statusErr.Error(), "\x1b") || strings.Contains(statusErr.Error(), "\u0085") || strings.Contains(statusErr.Error(), "\nproof: verified\n") {
		t.Fatalf("raw terminal control or forged line reached status error: %q", statusErr)
	}
}

var _ agentflow.Runner = (*recoveryRunner)(nil)

type resumeFixture struct {
	root     string
	planJSON []byte
	payload  map[string]any
}

func newResumeFixture(t *testing.T) resumeFixture {
	t.Helper()
	return newResumeFixtureWithPlan(t, recoveryPlan())
}

func newResumeFixtureWithPlan(t *testing.T, plan *agentflow.Plan) resumeFixture {
	t.Helper()
	root := t.TempDir()
	agentDir := filepath.Join(root, ".agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planDigest, err := canonicalPlanJSONSHA256(planJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "plan.lock.json"), planJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	execution := []byte(`{"schema_version":"0.4.0","plan":".agent/plan.lock.json","command_policy":{"command_timeout_seconds":600}}`)
	if err := os.WriteFile(filepath.Join(agentDir, "execution.contract.json"), execution, 0o600); err != nil {
		t.Fatal(err)
	}
	workflow, err := json.Marshal(defaultWorkflowRecommendation().Contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "workflow.contract.json"), workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	executionDigest := fmt.Sprintf("%x", sha256.Sum256(execution))
	return resumeFixture{
		root: root, planJSON: planJSON,
		payload: map[string]any{
			"state": "validation_missing", "reason": "gate missing", "blocking": true, "step_id": "P1",
			"resumability": map[string]any{
				"contract": map[string]any{"plan_sha256": planDigest, "locked": true, "execution_contract_sha256": executionDigest},
				"agent_id": "golem",
				"step":     map[string]any{"id": "P1", "state": "claimed", "completed": false},
				"attempt":  map[string]any{"id": "A1", "state": "claimed", "owner": "golem", "open": true},
				"lease": map[string]any{
					"policy": "enforce", "ttl_minutes": 30, "grace_seconds": 0,
					"expires_at": "2099-07-19T20:00:00Z", "state": "live", "exclusive": true,
				},
				"gates": []any{},
				"recovery_actions": []any{
					map[string]any{"action": "continue", "allowed": true, "automatic": true, "break_glass": false, "reason": "owner holds lease"},
				},
				"diagnostics": []any{},
			},
		},
	}
}

func (f resumeFixture) state(t *testing.T) agentflow.NextActionState {
	t.Helper()
	b, err := json.Marshal(f.payload)
	if err != nil {
		t.Fatal(err)
	}
	var state agentflow.NextActionState
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f resumeFixture) projection() map[string]any {
	return f.payload["resumability"].(map[string]any)
}

func (f *resumeFixture) useAdvisoryLease() {
	lease := f.projection()["lease"].(map[string]any)
	lease["policy"], lease["state"], lease["expires_at"], lease["exclusive"] = "advisory", "no_deadline", nil, false
}

func TestValidateResumeProjection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f *resumeFixture)
		valid  bool
	}{
		{name: "valid owned live attempt", valid: true},
		{name: "valid owned no-deadline attempt", valid: true, mutate: func(_ *testing.T, f *resumeFixture) {
			lease := f.projection()["lease"].(map[string]any)
			lease["state"], lease["expires_at"] = "no_deadline", nil
		}},
		{name: "missing projection", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.payload, "resumability") }},
		{name: "missing contract", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "contract") }},
		{name: "unlocked contract", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["locked"] = false
		}},
		{name: "missing plan digest", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["plan_sha256"] = ""
		}},
		{name: "plan digest mismatch", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["plan_sha256"] = strings.Repeat("0", 64)
		}},
		{name: "execution digest mismatch", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["execution_contract_sha256"] = strings.Repeat("0", 64)
		}},
		{name: "missing execution contract", mutate: func(t *testing.T, f *resumeFixture) {
			if err := os.Remove(filepath.Join(f.root, ".agent", "execution.contract.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing workflow contract", mutate: func(t *testing.T, f *resumeFixture) {
			if err := os.Remove(filepath.Join(f.root, ".agent", "workflow.contract.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "malformed workflow contract", mutate: func(t *testing.T, f *resumeFixture) {
			if err := os.WriteFile(filepath.Join(f.root, ".agent", "workflow.contract.json"), []byte(`{`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "foreign projected agent", mutate: func(_ *testing.T, f *resumeFixture) { f.projection()["agent_id"] = "other" }},
		{name: "missing attempt field", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "attempt") }},
		{name: "foreign attempt owner", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["owner"] = "other"
		}},
		{name: "ownerless attempt", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["owner"] = ""
		}},
		{name: "closed attempt", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["open"] = false
		}},
		{name: "missing lease", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "lease") }},
		{name: "expired lease", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["lease"].(map[string]any)["state"] = "expired"
		}},
		{name: "unknown lease", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["lease"].(map[string]any)["state"] = "unknown"
		}},
		{name: "missing gates", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "gates") }},
		{name: "missing recovery actions", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "recovery_actions") }},
		{name: "missing continue action", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["recovery_actions"] = []any{}
		}},
		{name: "denied continue action", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["recovery_actions"].([]any)[0].(map[string]any)["allowed"] = false
		}},
		{name: "manual continue action", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["recovery_actions"].([]any)[0].(map[string]any)["automatic"] = false
		}},
		{name: "break-glass continue action", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["recovery_actions"].([]any)[0].(map[string]any)["break_glass"] = true
		}},
		{name: "duplicate continue action", mutate: func(_ *testing.T, f *resumeFixture) {
			actions := f.projection()["recovery_actions"].([]any)
			f.projection()["recovery_actions"] = append(actions, actions[0])
		}},
		{name: "missing diagnostics", mutate: func(_ *testing.T, f *resumeFixture) { delete(f.projection(), "diagnostics") }},
		{name: "projected diagnostics", mutate: func(_ *testing.T, f *resumeFixture) {
			f.projection()["diagnostics"] = []any{map[string]any{"code": "bad", "message": "invalid", "artifact": "state"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			if test.mutate != nil {
				test.mutate(t, &fixture)
			}
			err := validateResumeProjection(fixture.root, fixture.planJSON, fixture.state(t), nil)
			if test.valid && err != nil {
				t.Fatalf("validateResumeProjection() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("validateResumeProjection() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateResumeProjection_RejectsOptionalHandoffMismatch(t *testing.T) {
	fixture := newResumeFixture(t)
	recommendation := defaultWorkflowRecommendation()
	recommendation.Contract.ReviewDepth = "deep"
	if err := validateResumeProjection(fixture.root, fixture.planJSON, fixture.state(t), &recommendation); err == nil {
		t.Fatal("validateResumeProjection() accepted a mismatched optional handoff")
	}
}

func TestValidateRecoveryProjectionAcceptsEligibleRetryStates(t *testing.T) {
	for _, stepState := range []string{"pending", "blocked", "failed", "abandoned"} {
		t.Run(stepState, func(t *testing.T) {
			fixture := newResumeFixture(t)
			state := fixture.noAttemptState(t, "step_unclaimed", true)
			state.Resumability.Step.State = stepState
			if err := validateRecoveryProjection(state); err != nil {
				t.Fatalf("eligible retry state rejected: %v", err)
			}
		})
	}
	fixture := newResumeFixture(t)
	state := fixture.noAttemptState(t, "step_unclaimed", true)
	state.Resumability.RecoveryActions = nil
	if err := validateRecoveryProjection(state); err == nil {
		t.Fatal("step_unclaimed projection without an allowed claim action was accepted")
	}
}

func recoveryAttemptState(state string, gates []agentflow.ResumabilityGate) agentflow.NextActionState {
	stepID := "P1"
	completed := false
	attemptState := "claimed"
	if state == "step_uncompleted" {
		attemptState = "verified"
	}
	return agentflow.NextActionState{
		State: state, StepID: &stepID, Command: "sh", Args: []string{"-c", "touch /tmp/advisory-must-not-run"},
		Resumability: &agentflow.ResumabilityProjection{
			Step:    &agentflow.ResumabilityStep{ID: stepID, State: attemptState, Completed: &completed},
			Attempt: &agentflow.ResumabilityAttempt{ID: "A1", State: attemptState, Owner: "golem", Open: true},
			Gates:   gates,
		},
	}
}

func recoveryPlan() *agentflow.Plan {
	return &agentflow.Plan{Steps: []agentflow.Step{{
		ID:         "P1",
		Validation: []string{"unit-tests", "lint"},
		Gates: []agentflow.Gate{
			{Kind: "command", Run: []string{"go", "test", "./..."}},
			{Kind: "command", Run: []string{"go", "vet", "./..."}},
		},
	}}}
}

func TestResumeCrashWindows(t *testing.T) {
	commandGates := func(first, second string) []agentflow.ResumabilityGate {
		return []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: first},
			{Kind: "command", Label: "go vet ./...", Status: second},
		}
	}
	tests := []struct {
		name    string
		state   agentflow.NextActionState
		mutated bool
		want    []string
	}{
		{
			name: "after claim or edits", state: recoveryAttemptState("validation_missing", commandGates("missing", "missing")), mutated: true,
			want: []string{"gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A1"},
		},
		{
			name: "after first gate", state: recoveryAttemptState("validation_missing", commandGates("satisfied", "missing")), mutated: true,
			want: []string{"gate:P1:lint", "finish-step:P1:A1"},
		},
		{
			name: "after gates", state: recoveryAttemptState("step_unverified", commandGates("satisfied", "satisfied")), mutated: true,
			want: []string{"finish-step:P1:A1"},
		},
		{
			name: "after verification", state: recoveryAttemptState("step_uncompleted", commandGates("satisfied", "satisfied")), mutated: true,
			want: []string{"complete-step:P1:A1"},
		},
		{name: "after finish step", state: recoveryAttemptState("step_unclaimed", nil)},
		{name: "after final step", state: recoveryAttemptState("run_unverified", nil)},
		{name: "after run verification", state: recoveryAttemptState("proof_missing", nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			af := &fakeAF{}
			mutated, err := settleAgentflowAttempt(context.Background(), af, recoveryPlan(), test.state)
			if err != nil {
				t.Fatal(err)
			}
			if mutated != test.mutated {
				t.Fatalf("mutated = %t, want %t", mutated, test.mutated)
			}
			if !reflect.DeepEqual(af.seq, test.want) {
				t.Fatalf("seq = %v, want %v", af.seq, test.want)
			}
			for _, argv := range af.gateArgv {
				if reflect.DeepEqual(argv, test.state.Args) || slicesContain(argv, "touch /tmp/advisory-must-not-run") {
					t.Fatalf("executed advisory argv: %q", argv)
				}
			}
		})
	}
}

func recoveryPlanTwoSteps() *agentflow.Plan {
	plan := recoveryPlan()
	plan.Steps = append(plan.Steps, agentflow.Step{
		ID:         "P2",
		Validation: []string{"unit-tests-2"},
		Gates: []agentflow.Gate{
			{Kind: "command", Run: []string{"go", "test", "./second/..."}},
		},
	})
	return plan
}

func TestProjectedCommandGatesRejectsDuplicateArgv(t *testing.T) {
	plan := &agentflow.Plan{Steps: []agentflow.Step{{
		ID:         "P1",
		Validation: []string{"unit A", "unit B"},
		Gates: []agentflow.Gate{
			{Kind: "command", Run: []string{"make", "check"}},
			{Kind: "command", Run: []string{"make", "check"}},
		},
	}}}
	state := recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "make check", Status: "satisfied"},
		{Kind: "command", Label: "make check", Status: "missing"},
	})
	af := &fakeAF{}
	if _, err := settleAgentflowAttempt(context.Background(), af, plan, state); err == nil || !strings.Contains(err.Error(), "duplicate command-gate argv") {
		t.Fatalf("err = %v, want duplicate command-gate argv rejection", err)
	}
	if len(af.seq) != 0 {
		t.Fatalf("mutated before rejecting duplicate argv: %v", af.seq)
	}
}

func TestResumeGatePairingUsesFilteredPositionAndPlanArgv(t *testing.T) {
	state := recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "satisfied"},
		{Kind: "inspection", Label: "review diff", Status: "satisfied"},
		{Kind: "legacy", Label: "legacy check", Status: "satisfied"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	af := &fakeAF{}
	mutated, err := settleAgentflowAttempt(context.Background(), af, recoveryPlan(), state)
	if err != nil || !mutated {
		t.Fatalf("mutated=%t err=%v", mutated, err)
	}
	if want := []string{"gate:P1:lint", "finish-step:P1:A1"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
	if want := [][]string{{"go", "vet", "./..."}}; !reflect.DeepEqual(af.gateArgv, want) {
		t.Fatalf("gate argv = %v, want %v", af.gateArgv, want)
	}
}

func TestSettleAgentflowAttemptRejectsAmbiguousGatesBeforeMutation(t *testing.T) {
	tests := []struct {
		name  string
		gates []agentflow.ResumabilityGate
	}{
		{name: "unknown kind", gates: []agentflow.ResumabilityGate{{Kind: "future", Label: "x", Status: "missing"}}},
		{name: "failed status", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "failed"},
		}},
		{name: "label mismatch", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "unit-tests", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
		}},
		{name: "count mismatch", gates: []agentflow.ResumabilityGate{{Kind: "command", Label: "go test ./...", Status: "missing"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			af := &fakeAF{}
			if _, err := settleAgentflowAttempt(context.Background(), af, recoveryPlan(), recoveryAttemptState("validation_missing", test.gates)); err == nil {
				t.Fatal("settleAgentflowAttempt() unexpectedly succeeded")
			}
			if len(af.seq) != 0 {
				t.Fatalf("mutated before rejecting gates: %v", af.seq)
			}
		})
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (f *resumeFixture) noAttemptState(t *testing.T, state string, step bool) agentflow.NextActionState {
	t.Helper()
	f.payload["state"] = state
	f.payload["reason"] = state
	projection := f.projection()
	projection["attempt"] = nil
	projection["gates"] = []any{}
	projection["recovery_actions"] = []any{}
	lease := projection["lease"].(map[string]any)
	lease["state"], lease["expires_at"] = "not_applicable", nil
	if step {
		projection["step"] = map[string]any{"id": "P1", "state": "pending", "completed": false}
		projection["recovery_actions"] = []any{
			map[string]any{"action": "claim", "allowed": true, "automatic": true, "break_glass": false, "reason": "eligible"},
		}
		f.payload["step_id"] = "P1"
	} else {
		projection["step"] = nil
		delete(f.payload, "step_id")
	}
	return f.state(t)
}

func (f *resumeFixture) attemptState(t *testing.T, state string, gates []agentflow.ResumabilityGate) agentflow.NextActionState {
	t.Helper()
	if gates == nil {
		gates = []agentflow.ResumabilityGate{}
	}
	f.payload["state"] = state
	f.payload["reason"] = state
	f.payload["command"] = "sh"
	f.payload["args"] = []string{"-c", "touch /tmp/advisory-must-not-run"}
	projection := f.projection()
	attemptState := "claimed"
	if state == "step_uncompleted" {
		attemptState = "verified"
	}
	projection["step"].(map[string]any)["state"] = attemptState
	projection["attempt"].(map[string]any)["state"] = attemptState
	projection["gates"] = gates
	return f.state(t)
}

func TestResumeReentersExistingSerialStepLoop(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.noAttemptState(t, "step_unclaimed", true)
	complete := fixture.noAttemptState(t, "complete", false)
	inFlightFixture := newResumeFixture(t)
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{initial, inFlight, complete}}
	d := &driver{af: af, plan: recoveryPlan()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("state=%q err=%v", final.State, err)
	}
	want := []string{
		"next-action", "next-step", "claim:P1", "model:P1",
		"next-action",
		"gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "finish-run", "next-action",
	}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

func TestResumeRunsMultipleRemainingStepsSerially(t *testing.T) {
	fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	fixture.useAdvisoryLease()
	initial := fixture.noAttemptState(t, "step_unclaimed", true)
	complete := fixture.noAttemptState(t, "complete", false)

	inFlightP1Fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	inFlightP1Fixture.useAdvisoryLease()
	inFlightP1 := inFlightP1Fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlightP1.Resumability.Attempt.ID = "A-P1"

	inFlightP2Fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	inFlightP2Fixture.useAdvisoryLease()
	inFlightP2 := inFlightP2Fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./second/...", Status: "missing"},
	})
	stepID2 := "P2"
	inFlightP2.StepID = &stepID2
	inFlightP2.Resumability.Step.ID = "P2"
	inFlightP2.Resumability.Attempt.ID = "A-P2"

	af := &fakeAF{
		nextSteps:   []string{"P1", "P2"},
		nextActions: []agentflow.NextActionState{initial, inFlightP1, inFlightP2, complete},
	}
	d := &driver{af: af, plan: recoveryPlanTwoSteps()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("state=%q err=%v", final.State, err)
	}
	want := []string{
		"next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "claim:P2", "model:P2",
		"next-action", "gate:P2:unit-tests-2", "finish-step:P2:A-P2",
		"next-step", "finish-run", "next-action",
	}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
	wantArgv := [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}, {"go", "test", "./second/..."}}
	if !reflect.DeepEqual(af.gateArgv, wantArgv) {
		t.Fatalf("gate argv = %v, want %v", af.gateArgv, wantArgv)
	}
}

func TestResumeRejectsFiniteEnforcedRecoveryBeforeMutation(t *testing.T) {
	for _, stateName := range []string{"step_unclaimed", "validation_missing", "step_unverified", "step_uncompleted"} {
		t.Run(stateName, func(t *testing.T) {
			fixture := newResumeFixture(t)
			var state agentflow.NextActionState
			if stateName == "step_unclaimed" {
				state = fixture.noAttemptState(t, stateName, true)
			} else {
				state = fixture.attemptState(t, stateName, []agentflow.ResumabilityGate{
					{Kind: "command", Label: "go test ./...", Status: "missing"},
					{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
				})
			}
			af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{state}}
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}

			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), "finite enforced lease") {
				t.Fatalf("resume error = %v, want finite enforced lease refusal", err)
			}
			if want := []string{"next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("mutations before lease refusal = %v, want %v", af.seq, want)
			}
		})
	}
}

func TestResumeReentryRefusesAlreadySatisfiedGateInsteadOfDuplicatingIt(t *testing.T) {
	initialFixture := newResumeFixture(t)
	initialFixture.useAdvisoryLease()
	initial := initialFixture.noAttemptState(t, "step_unclaimed", true)

	inFlightFixture := newResumeFixture(t)
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "satisfied"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"

	af := &fakeAF{
		nextSteps:   []string{"P1"},
		nextActions: []agentflow.NextActionState{initial, inFlight},
	}
	d := &driver{af: af, plan: recoveryPlan()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	if _, err := d.resume(context.Background(), initialFixture.root, initialFixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), "refusing duplicate execution") {
		t.Fatalf("resume error = %v, want duplicate gate refusal", err)
	}
	want := []string{"next-action", "next-step", "claim:P1", "model:P1", "next-action"}
	if !reflect.DeepEqual(af.seq, want) || len(af.gateArgv) != 0 {
		t.Fatalf("calls before duplicate refusal = %v argv=%v, want %v and no gate argv", af.seq, af.gateArgv, want)
	}
}

func TestResumeProgressReadOccursOnlyAfterSettlement(t *testing.T) {
	t.Run("settlement rereads and rejects unchanged state", func(t *testing.T) {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		initial := fixture.attemptState(t, "step_unverified", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "satisfied"},
			{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
		})
		af := &fakeAF{nextActions: []agentflow.NextActionState{initial, initial}}
		d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
		if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), "did not progress") {
			t.Fatalf("resume error = %v", err)
		}
		want := []string{"next-action", "finish-step:P1:A1", "next-action"}
		if !reflect.DeepEqual(af.seq, want) {
			t.Fatalf("seq = %v, want %v", af.seq, want)
		}
	})

	for _, stateName := range []string{"run_unverified", "proof_missing"} {
		t.Run(stateName+" skips progress reread", func(t *testing.T) {
			fixture := newResumeFixture(t)
			initial := fixture.noAttemptState(t, stateName, false)
			complete := fixture.noAttemptState(t, "complete", false)
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, complete}}
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil {
				t.Fatal(err)
			}
			want := []string{"next-action", "next-step", "finish-run", "next-action"}
			if !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
}

func TestResumeSettlementContinuesSeriallyWithoutRepeatingMutation(t *testing.T) {
	tests := []struct {
		state string
		gates []agentflow.ResumabilityGate
		want  []string
	}{
		{
			state: "validation_missing",
			gates: []agentflow.ResumabilityGate{
				{Kind: "command", Label: "go test ./...", Status: "satisfied"},
				{Kind: "command", Label: "go vet ./...", Status: "missing"},
			},
			want: []string{"next-action", "gate:P1:lint", "finish-step:P1:A1", "next-action", "next-step", "finish-run", "next-action"},
		},
		{
			state: "step_unverified",
			want:  []string{"next-action", "finish-step:P1:A1", "next-action", "next-step", "finish-run", "next-action"},
		},
		{
			state: "step_uncompleted",
			want:  []string{"next-action", "complete-step:P1:A1", "next-action", "next-step", "finish-run", "next-action"},
		},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			initial := fixture.attemptState(t, test.state, test.gates)
			progressed := fixture.noAttemptState(t, "run_unverified", false)
			complete := fixture.noAttemptState(t, "complete", false)
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, progressed, complete}}
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(af.seq, test.want) {
				t.Fatalf("seq = %v, want %v", af.seq, test.want)
			}
		})
	}
}

func TestResumeBlockedStatesDoNotMutate(t *testing.T) {
	for _, stateName := range []string{"file_receipts_missing", "drift_failing", "proof_stale", "proof_failing", "future_state"} {
		t.Run(stateName, func(t *testing.T) {
			fixture := newResumeFixture(t)
			state := fixture.attemptState(t, stateName, nil)
			af := &fakeAF{nextActions: []agentflow.NextActionState{state}}
			d := &driver{af: af, plan: recoveryPlan()}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil {
				t.Fatal("resume unexpectedly succeeded")
			}
			if want := []string{"next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want read-only %v", af.seq, want)
			}
		})
	}
}
