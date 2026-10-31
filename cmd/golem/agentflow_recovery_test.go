package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

type recoveryRunner struct {
	payload []byte
	version string // --version reply; "" means a supported "agentflow 1.2.0"
	calls   [][]string
}

// failingRecoveryRunner reports a supported version, then fails every other
// call with stderr, so the next-action failure path still sees that stderr.
type failingRecoveryRunner struct{ stderr []byte }

func (r *failingRecoveryRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	if len(args) > 0 && args[0] == "--version" {
		return []byte("agentflow 1.2.0\n"), nil, 0, nil
	}
	return nil, append([]byte(nil), r.stderr...), 1, nil
}

func (r *recoveryRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "--version" {
		if r.version == "" {
			return []byte("agentflow 1.2.0\n"), nil, 0, nil
		}
		return []byte(r.version), nil, 0, nil
	}
	return append([]byte(nil), r.payload...), nil, 0, nil
}

func TestResumeDispositionCoversAgentflowStates(t *testing.T) {
	tests := map[string]int{
		"uninitialized": 3, "plan_unlocked": 3, "execution_uninitialized": 3,
		"state_invalid": 3, "step_unclaimed": 2, "file_receipts_missing": 3,
		"validation_missing": 2, "step_unverified": 2, "step_uncompleted": 2,
		"drift_failing": 3, "run_unverified": 2, "proof_missing": 2,
		"proof_stale": 3, "proof_failing": 3, "complete": 0, "future_state": 3,
		// AgentFlow 1.1 (agentflow#70).
		"attempt_timed_out": 2, "step_amendment_needed": 2,
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
	planBytes, err := json.Marshal(agentflow.Plan{SchemaVersion: agentflow.PlanSchemaVersion, Steps: []agentflow.Step{{
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
	wantCalls := [][]string{{"--version"}, {"next-action", "--root", root, "--json", "--agent", "golem"}}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", runner.calls, wantCalls)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("proof state changed: %q err=%v", after, err)
	}
}

// #612 R6/U6: status refuses an unsupported AgentFlow before next-action. The
// rejection is Golem's own text, so JSON mode reports it on stderr.
func TestAgentflowStatus_RejectsUnsupportedVersionBeforeNextAction(t *testing.T) {
	for _, tt := range []struct{ version, want string }{
		{"agentflow 0.4.0\n", "agentflow 0.4 is too old; need >= 1.2"},
		{"agentflow 1.1.1\n", "agentflow 1.1 is too old; need >= 1.2"},
		{"agentflow 2.0.0\n", "agentflow 2.0 is newer than this Golem supports; need 1.x"},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s json=%t", strings.TrimSpace(tt.version), jsonOutput), func(t *testing.T) {
				runner := &recoveryRunner{version: tt.version, payload: []byte(`{"state":"complete"}`)}
				var out bytes.Buffer
				err := runAgentflowStatusWithRunner(context.Background(), &out, t.TempDir(), jsonOutput, runner)
				var exit *agentflowStatusExit
				if !errors.As(err, &exit) || exit.ExitCode() != 3 {
					t.Fatalf("err = %v, want exit 3", err)
				}
				if jsonOutput {
					if out.Len() != 0 || exit.diagnostic != tt.want {
						t.Fatalf("json stdout=%q diagnostic=%q, want empty / %q", out.String(), exit.diagnostic, tt.want)
					}
				} else if out.String() != "agentflow status unavailable: "+tt.want+"\n" || exit.diagnostic != "" {
					t.Fatalf("human stdout=%q diagnostic=%q", out.String(), exit.diagnostic)
				}
				if !reflect.DeepEqual(runner.calls, [][]string{{"--version"}}) {
					t.Fatalf("calls = %v, want only --version", runner.calls)
				}
			})
		}
	}
}

// versionLaunchFailRunner fails every launch with text that must never reach
// JSON status output (#612 G17).
type versionLaunchFailRunner struct{}

func (versionLaunchFailRunner) Run(context.Context, []string, []byte) ([]byte, []byte, int, error) {
	return nil, nil, 0, errors.New("exec /opt/RUNNER-SECRET-577/agentflow: permission denied")
}

func TestAgentflowStatus_JSONHidesVersionLaunchFailure(t *testing.T) {
	var out bytes.Buffer
	err := runAgentflowStatusWithRunner(context.Background(), &out, t.TempDir(), true, versionLaunchFailRunner{})
	var exit *agentflowStatusExit
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit 3", err)
	}
	if exit.diagnostic != "" || out.Len() != 0 {
		t.Fatalf("diagnostic=%q stdout=%q, want silent exit 3", exit.diagnostic, out.String())
	}
}

// versionExitRunner fails `agentflow --version` with an exit code and stderr:
// a broken install, where AgentFlow's own message is the diagnosis (#612).
type versionExitRunner struct{ stderr string }

func (r versionExitRunner) Run(context.Context, []string, []byte) ([]byte, []byte, int, error) {
	return nil, []byte(r.stderr), 1, nil
}

// Human status keeps (sanitized) --version stderr as develop did; JSON status
// stays silent because that text is not Golem's own.
func TestAgentflowStatus_VersionExitKeepsStderrInHumanOnly(t *testing.T) {
	runner := versionExitRunner{stderr: "boom: RUNNER-SECRET-577 missing module\x1b[2J\n"}

	var human bytes.Buffer
	err := runAgentflowStatusWithRunner(context.Background(), &human, t.TempDir(), false, runner)
	var exit *agentflowStatusExit
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || exit.diagnostic != "" {
		t.Fatalf("human err = %v, want exit 3 without a stderr diagnostic", err)
	}
	const want = "agentflow status unavailable: agentflow unavailable (--version failed): " +
		"agentflow --version: exit 1: boom: RUNNER-SECRET-577 missing module\\x1b[2J\n"
	if human.String() != want {
		t.Fatalf("human stdout = %q, want %q", human.String(), want)
	}

	var jsonOut bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &jsonOut, t.TempDir(), true, runner)
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("json err = %v, want exit 3", err)
	}
	if exit.diagnostic != "" || jsonOut.Len() != 0 {
		t.Fatalf("json diagnostic=%q stdout=%q, want silent exit 3", exit.diagnostic, jsonOut.String())
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

// #652 A6: status offers the failed-gate recovery (exit 2) only when resume
// would perform it; each exception, and a genuinely unknown status, exits 3.
// JSON mode relays AgentFlow's bytes either way.
func TestAgentflowStatus_FailedGateDisposition(t *testing.T) {
	type row struct {
		name     string
		advisory bool
		mutate   func(f *resumeFixture)
		gates    []agentflow.ResumabilityGate // nil means failedGateState
		stepRuns string                       // non-empty: written to .agent/step-runs.jsonl first
		code     int
		want     string
	}
	rows := []row{{name: "advisory owned attempt", advisory: true, code: 2,
		want: "\nresume: block the attempt whose gate failed, then re-run the step serially\n"}}
	for _, r := range failedGateRefusals {
		rows = append(rows, row{name: r.name, advisory: r.advisory, mutate: r.mutate, code: 3, want: "\nresume: blocked " + r.status})
	}
	// AgentFlow 1.1 refuses to block a closed attempt itself (agentflow#61),
	// so neither status nor resume reads the step-run ledger first.
	rows = append(rows,
		row{name: "step-run ledger is not read", advisory: true, stepRuns: "{\n", code: 2,
			want: "\nresume: block the attempt whose gate failed, then re-run the step serially\n"})
	rows = append(rows, row{name: "unknown status", advisory: true, gates: []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "future"},
	}, code: 3, want: `resume: blocked by unsafe or malformed projection: agentflow gate \"go vet ./...\" has unknown status \"future\"`})
	for _, tt := range rows {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", tt.name, jsonOutput), func(t *testing.T) {
				fixture := newResumeFixture(t)
				if tt.advisory {
					fixture.useAdvisoryLease()
				}
				if tt.gates != nil {
					_ = fixture.attemptState(t, "validation_missing", tt.gates)
				} else {
					_ = failedGateState(t, &fixture)
				}
				if tt.mutate != nil {
					tt.mutate(&fixture)
				}
				if tt.stepRuns != "" {
					writeAgentFile(t, fixture.root, "step-runs.jsonl", tt.stepRuns)
				}
				payload, err := json.Marshal(fixture.payload)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, jsonOutput, &recoveryRunner{payload: payload})
				var exit *agentflowStatusExit
				if !errors.As(err, &exit) || exit.ExitCode() != tt.code || exit.diagnostic != "" {
					t.Fatalf("err = %v, want status exit %d without a diagnostic\n%s", err, tt.code, out.String())
				}
				if jsonOutput {
					if !bytes.Equal(out.Bytes(), payload) {
						t.Fatalf("JSON output = %q, want exact payload %q", out.Bytes(), payload)
					}
					return
				}
				// Exactly one resume line, and a refusal never also offers the
				// recovery (consult: Contains alone accepts an appended line).
				const offer = "block the attempt whose gate failed"
				if !strings.Contains(out.String(), tt.want) || strings.Count(out.String(), "\nresume: ") != 1 ||
					(tt.code == 3 && strings.Contains(out.String(), offer)) {
					t.Fatalf("output lacks %q, has other than one resume line, or offers recovery under exit 3:\n%s", tt.want, out.String())
				}
			})
		}
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
	reportAgentflowRecovery(context.Background(), &report, agentflow.NewOwnedClient(&recoveryRunner{payload: payload}, t.TempDir(), "golem"), errors.New("step P1 failed"))
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

	var unavailable bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(context.Background(), &unavailable, t.TempDir(), false, &failingRecoveryRunner{stderr: []byte(evil)})
	var exit *agentflowStatusExit
	if !errors.As(statusErr, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("status error = %v, want exit 3", statusErr)
	}
	if strings.Contains(unavailable.String(), "\x1b") || strings.Contains(unavailable.String(), "\u0085") || strings.Contains(unavailable.String(), "\nproof: verified\n") {
		t.Fatalf("raw terminal control or forged line reached status output: %q", unavailable.String())
	}
	if !strings.Contains(unavailable.String(), "agentflow status unavailable") {
		t.Fatalf("status output = %q, want unavailable diagnostic", unavailable.String())
	}
	// The evil stderr must reach the output, escaped. Otherwise the absence
	// checks above prove nothing (for example, if --version failed first).
	if !strings.Contains(unavailable.String(), `proof: verified\x1b[2J`) {
		t.Fatalf("status output = %q, want the escaped next-action stderr", unavailable.String())
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
	execution := []byte(`{"schema_version":"1.0.0","plan":".agent/plan.lock.json","command_policy":{"command_timeout_seconds":600}}`)
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
	return &agentflow.Plan{SchemaVersion: agentflow.PlanSchemaVersion, Steps: []agentflow.Step{{
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

// #642 A2: a settlement gate error blocks the attempt with the one fixed
// reason, runs no later gate and no finish, and blocks on a fresh bounded
// context, since settlement runs on resume's run context.
func TestSettleAgentflowAttemptBlocksOnGateError(t *testing.T) {
	for _, c := range callerContexts {
		t.Run(c.name, func(t *testing.T) {
			ctx := c.ctx(t)
			if ctx.Err() != c.wantErr {
				t.Fatalf("fixture context err = %v, want %v", ctx.Err(), c.wantErr)
			}
			gateErr := errors.New("gate failed")
			af := &fakeAF{failAt: map[string]error{"gate": gateErr}}
			state := recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
				{Kind: "command", Label: "go test ./...", Status: "missing"},
				{Kind: "command", Label: "go vet ./...", Status: "missing"},
			})
			mutated, err := settleAgentflowAttempt(ctx, af, recoveryPlan(), state)
			if !mutated {
				t.Fatal("mutated = false, want true")
			}
			if want := []string{"gate:P1:unit-tests", "block-step:P1:A1:golem: gate execution failed"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
			if want := `step P1 attempt A1: gate "unit-tests" failed: gate failed; attempt recorded as blocked`; err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if !errors.Is(err, gateErr) {
				t.Fatalf("err = %v does not wrap the gate error", err)
			}
			assertFreshBlockContext(t, af.blocks)
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
	for _, statuses := range [][2]string{{"satisfied", "missing"}, {"failed", "missing"}} {
		t.Run(statuses[0], func(t *testing.T) {
			state := recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
				{Kind: "command", Label: "make check", Status: statuses[0]},
				{Kind: "command", Label: "make check", Status: statuses[1]},
			})
			af := &fakeAF{}
			if _, err := settleAgentflowAttempt(context.Background(), af, plan, state); err == nil || !strings.Contains(err.Error(), "duplicate command-gate argv") {
				t.Fatalf("err = %v, want duplicate command-gate argv rejection", err)
			}
			if len(af.seq) != 0 {
				t.Fatalf("mutated before rejecting duplicate argv: %v", af.seq)
			}
		})
	}
}

// #652 A6: failed is AgentFlow's third gate status. A failed command gate is
// flagged; a genuinely unknown status is still refused.
func TestProjectedCommandGatesKnowsFailed(t *testing.T) {
	state := recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "failed"},
	})
	commands, projected, err := projectedCommandGates(recoveryPlan(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[1].Label != "lint" || projected[0].Status != "missing" || projected[1].Status != "failed" {
		t.Fatalf("commands = %+v, projected = %+v", commands, projected)
	}
	if !hasFailedCommandGate(state) {
		t.Fatal("hasFailedCommandGate = false for a failed command gate")
	}
	for _, tt := range []struct {
		name  string
		state agentflow.NextActionState
	}{
		{"no failed gate", recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "satisfied"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
		})},
		{"not validation_missing", recoveryAttemptState("step_unverified", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "failed"},
		})},
		{"failed non-command gate", recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
			{Kind: "inspection", Label: "review notes", Status: "failed"},
		})},
	} {
		if hasFailedCommandGate(tt.state) {
			t.Errorf("%s: hasFailedCommandGate = true, want false", tt.name)
		}
	}
	_, _, err = projectedCommandGates(recoveryPlan(), recoveryAttemptState("validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "future"},
	}))
	if want := `agentflow gate "go vet ./..." has unknown status "future"`; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
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
		{name: "unknown status", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "future"},
		}},
		{name: "label mismatch", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "unit-tests", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
		}},
		{name: "count mismatch", gates: []agentflow.ResumabilityGate{{Kind: "command", Label: "go test ./...", Status: "missing"}}},
		// #652: validation before the block. A failed command gate beside a
		// malformed one must be refused with zero mutations, so the A6 block can
		// never run ahead of projectedCommandGates.
		{name: "failed plus unknown kind", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "failed"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
			{Kind: "future", Label: "x", Status: "missing"},
		}},
		{name: "failed plus unknown status", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "failed"},
			{Kind: "command", Label: "go vet ./...", Status: "future"},
		}},
		{name: "failed plus label mismatch", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "failed"},
			{Kind: "command", Label: "lint", Status: "missing"},
		}},
		{name: "failed plus count mismatch", gates: []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "failed"},
		}},
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

// finishRunGuard is the run-phase state resume's pre-finish-run check reads
// when no step work remains (#651, #688). A resume test that reaches
// finish-run scripts it before its final state: without it the check consumes
// that state, and resume's last next-action gets fakeAF's bare unscripted
// default, failing with "agentflow resumability projection is missing".
var finishRunGuard = agentflow.NextActionState{State: "run_unverified"}

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
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{initial, inFlight, finishRunGuard, complete}}
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
		"probe", "next-action", "next-step", "claim:P1", "model:P1",
		"next-action",
		"gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "next-action", "finish-run", "next-action",
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
		nextActions: []agentflow.NextActionState{initial, inFlightP1, inFlightP2, finishRunGuard, complete},
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
		"probe", "next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "claim:P2", "model:P2",
		"next-action", "gate:P2:unit-tests-2", "finish-step:P2:A-P2",
		"next-step", "next-action", "finish-run", "next-action",
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

			want := "finite enforced lease"
			if stateName == "step_unclaimed" {
				want = "agentflow resume blocks step_unclaimed under an enforced lease because claiming would enter non-atomic gate recovery"
			}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("resume error = %v, want %q", err, want)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("mutations before lease refusal = %v, want %v", af.seq, want)
			}
		})
	}
}

func TestResumeReentryRefusesRecordedGateInsteadOfDuplicatingIt(t *testing.T) {
	for _, status := range []string{"satisfied", "failed"} {
		t.Run(status, func(t *testing.T) {
			initialFixture := newResumeFixture(t)
			initialFixture.useAdvisoryLease()
			initial := initialFixture.noAttemptState(t, "step_unclaimed", true)

			inFlightFixture := newResumeFixture(t)
			inFlightFixture.useAdvisoryLease()
			inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
				{Kind: "command", Label: "go test ./...", Status: status},
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

			want := fmt.Sprintf(`agentflow resumed step "P1" already has %s command gate "go test ./..."; refusing duplicate execution`, status)
			if _, err := d.resume(context.Background(), initialFixture.root, initialFixture.planJSON, nil); err == nil || err.Error() != want {
				t.Fatalf("resume error = %v, want %q", err, want)
			}
			wantSeq := []string{"probe", "next-action", "next-step", "claim:P1", "model:P1", "next-action"}
			if !reflect.DeepEqual(af.seq, wantSeq) || len(af.gateArgv) != 0 {
				t.Fatalf("calls before duplicate refusal = %v argv=%v, want %v and no gate argv", af.seq, af.gateArgv, wantSeq)
			}
		})
	}
}

// #688: resume runs #651's step-work check before finish-run, as a fresh run
// does. next-step --explain answers steps_complete here (#731), as when another
// writer claims P2 after the loop's last call, and next-action reports P2's open
// attempt: resume refuses and never calls finish-run, so no proof is built over
// the unfinished work.
func TestResumeRefusesFinishRunWhileStepWorkRemains(t *testing.T) {
	fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	fixture.useAdvisoryLease()
	initial := fixture.noAttemptState(t, "step_unclaimed", true)
	inFlightFixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"
	stepID2 := "P2"
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{
		initial, inFlight, {State: "validation_missing", StepID: &stepID2},
	}}
	d := &driver{af: af, plan: recoveryPlanTwoSteps()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	want := `agentflow still reports step work before finish-run: state "validation_missing" step "P2"; no proof was built; run -agentflow-status to see whether -agentflow-resume can recover it`
	if err == nil || err.Error() != want || final.State != "" {
		t.Fatalf("state = %q, err = %v\nwant error %s", final.State, err, want)
	}
	wantSeq := []string{
		"probe", "next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "next-action",
	}
	if !reflect.DeepEqual(af.seq, wantSeq) {
		t.Fatalf("seq = %v\nwant %v", af.seq, wantSeq)
	}
}

// #731 A3: when next-step --explain reports another writer's open attempt
// after settlement and the serial step, resume refuses there, before the
// next-action check and finish-run.
func TestResumeStopsWhenStepWorkRemainsAfterSettlement(t *testing.T) {
	fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	fixture.useAdvisoryLease()
	initial := fixture.noAttemptState(t, "step_unclaimed", true)
	inFlightFixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"
	af := &fakeAF{
		explanations: []agentflow.NextStepExplanation{
			{StepID: "P1", ReasonCode: agentflow.NextStepEligible},
			{ReasonCode: agentflow.NextStepOpenAttempts, Diagnostics: []string{"step P2 has open attempt A9 (owner other)"}},
		},
		nextActions: []agentflow.NextActionState{initial, inFlight},
	}
	d := &driver{af: af, plan: recoveryPlanTwoSteps()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	want := `agentflow reports no eligible step while step work remains: reason_code "open_attempts" (step P2 has open attempt A9 (owner other)); no proof was built; run -agentflow-status to see whether -agentflow-resume can recover it`
	if err == nil || err.Error() != want || final.State != "" {
		t.Fatalf("state = %q, err = %v\nwant error %s", final.State, err, want)
	}
	wantSeq := []string{
		"probe", "next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step",
	}
	if !reflect.DeepEqual(af.seq, wantSeq) {
		t.Fatalf("seq = %v\nwant %v", af.seq, wantSeq)
	}
}

// nextActionErrAt fails the call-th NextAction with err and serves every
// other call from fakeAF, so a test can fail only resume's pre-finish-run
// check (#688). fakeAF.failAt would fail every scripted call.
type nextActionErrAt struct {
	*fakeAF
	call, calls int
	err         error
}

func (f *nextActionErrAt) NextAction(ctx context.Context) (agentflow.NextActionState, error) {
	f.calls++
	if f.calls == f.call {
		f.seq = append(f.seq, "next-action")
		return agentflow.NextActionState{}, f.err
	}
	return f.fakeAF.NextAction(ctx)
}

// #688: when the pre-finish-run next-action fails, resume stops before
// finish-run and keeps the error's identity, which reportAgentflowRecovery
// inspects with errors.As.
func TestResumeNextActionErrorBlocksFinishRun(t *testing.T) {
	fixture := newResumeFixture(t)
	initial := fixture.noAttemptState(t, "run_unverified", false)
	sentinel := errors.New("next-action unavailable")
	af := &nextActionErrAt{fakeAF: &fakeAF{nextActions: []agentflow.NextActionState{initial}}, call: 2, err: sentinel}
	d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
	_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if !errors.Is(err, sentinel) || err.Error() != "check step work before finish-run: next-action unavailable" {
		t.Fatalf("err = %v, want the wrapped next-action error", err)
	}
	if want := []string{"probe", "next-action", "next-step", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #688: a writer that opens an attempt after resume's pre-finish-run check
// still reaches finish-run (kstruzzieri/agentflow#58). Resume's final check is
// then the only refusal, so it must reject the non-complete state rather than
// report success over an open attempt.
func TestResumeFinalCheckRefusesAttemptOpenedAfterCheck(t *testing.T) {
	fixture := newResumeFixture(t)
	initial := fixture.noAttemptState(t, "run_unverified", false)
	openedFixture := newResumeFixture(t)
	openedFixture.useAdvisoryLease()
	opened := openedFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	af := &fakeAF{nextActions: []agentflow.NextActionState{initial, finishRunGuard, opened}}
	d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if want := `agentflow resume finished in state "validation_missing", want complete`; err == nil || err.Error() != want || final.State != "" {
		t.Fatalf("state = %q, err = %v\nwant error %s", final.State, err, want)
	}
	if want := []string{"probe", "next-action", "next-step", "next-action", "finish-run", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
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
		want := []string{"probe", "next-action", "finish-step:P1:A1", "next-action"}
		if !reflect.DeepEqual(af.seq, want) {
			t.Fatalf("seq = %v, want %v", af.seq, want)
		}
	})

	for _, stateName := range []string{"run_unverified", "proof_missing"} {
		t.Run(stateName+" skips progress reread", func(t *testing.T) {
			fixture := newResumeFixture(t)
			initial := fixture.noAttemptState(t, stateName, false)
			complete := fixture.noAttemptState(t, "complete", false)
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, finishRunGuard, complete}}
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil {
				t.Fatal(err)
			}
			want := []string{"probe", "next-action", "next-step", "next-action", "finish-run", "next-action"}
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
			want: []string{"probe", "next-action", "gate:P1:lint", "finish-step:P1:A1", "next-action", "next-step", "next-action", "finish-run", "next-action"},
		},
		{
			state: "step_unverified",
			want:  []string{"probe", "next-action", "finish-step:P1:A1", "next-action", "next-step", "next-action", "finish-run", "next-action"},
		},
		{
			state: "step_uncompleted",
			want:  []string{"probe", "next-action", "complete-step:P1:A1", "next-action", "next-step", "next-action", "finish-run", "next-action"},
		},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			initial := fixture.attemptState(t, test.state, test.gates)
			progressed := fixture.noAttemptState(t, "run_unverified", false)
			complete := fixture.noAttemptState(t, "complete", false)
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, progressed, finishRunGuard, complete}}
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
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want read-only %v", af.seq, want)
			}
		})
	}
}

// #689: settlement appends to the .agent ledgers an older proof hashes, so
// AgentFlow reports that proof proof_stale. Resume continues to the same tail
// as after the serial loop, and finish-run replaces it.
func TestResumeSettlementReplacesProofItsWorkMadeStale(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	stale := fixture.noAttemptState(t, "proof_stale", false)
	complete := fixture.noAttemptState(t, "complete", false)
	// The pre-finish-run check reads the same stale state again.
	af := &fakeAF{nextActions: []agentflow.NextActionState{initial, stale, stale, complete}}
	d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
		t.Error("resume ran the model")
		return nil
	}}
	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("resume state = %q, err = %v; want complete", final.State, err)
	}
	want := []string{"probe", "next-action", "complete-step:P1:A1", "next-action", "next-step", "next-action", "finish-run", "next-action"}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #689: Golem's own ledger writes make an older proof proof_stale, never
// proof_failing, so a failing proof exposed by settlement (malformed, newer
// schema) is not Golem's to replace. Nothing after the progress read runs.
func TestResumeSettlementPreservesFailingProof(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	failing := fixture.noAttemptState(t, "proof_failing", false)
	af := &fakeAF{nextActions: []agentflow.NextActionState{initial, failing}}
	d := &driver{af: af, plan: recoveryPlan()}
	_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if want := `agentflow resume blocked after settlement in state "proof_failing": blocked by failing proof`; err == nil || err.Error() != want {
		t.Fatalf("resume err = %v\nwant %s", err, want)
	}
	if want := []string{"probe", "next-action", "complete-step:P1:A1", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #689 D2: drift exposed by settlement still fails closed; nothing after the
// progress read runs.
func TestResumeSettlementStillRefusesDrift(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	drift := fixture.noAttemptState(t, "drift_failing", false)
	af := &fakeAF{nextActions: []agentflow.NextActionState{initial, drift}}
	d := &driver{af: af, plan: recoveryPlan()}
	_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if want := `agentflow resume blocked after settlement in state "drift_failing": blocked by drift findings`; err == nil || err.Error() != want {
		t.Fatalf("resume err = %v\nwant %s", err, want)
	}
	if want := []string{"probe", "next-action", "complete-step:P1:A1", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #689: when the replacement proof fails, resume reports finish-run's error
// and never reads a final state.
func TestResumeSettlementReplacementProofFailureStopsResume(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	stale := fixture.noAttemptState(t, "proof_stale", false)
	af := &fakeAF{
		nextActions: []agentflow.NextActionState{initial, stale, stale},
		proofError:  errors.New("finish-run stopped at verify-proof"),
	}
	d := &driver{af: af, plan: recoveryPlan()}
	if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), "finish-run stopped at verify-proof") {
		t.Fatalf("resume err = %v, want finish-run's error", err)
	}
	if want := []string{"probe", "next-action", "complete-step:P1:A1", "next-action", "next-step", "next-action", "finish-run"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #689: settlement completes P1 of two; the serial loop runs P2; the tail
// replaces the proof that work made stale.
func TestResumeSettlementThenSerialStepReplacesStaleProof(t *testing.T) {
	fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "step_uncompleted", nil)
	unclaimedP2 := fixture.noAttemptState(t, "step_unclaimed", true)
	stepID2 := "P2"
	unclaimedP2.StepID = &stepID2
	unclaimedP2.Resumability.Step.ID = "P2"
	stale := fixture.noAttemptState(t, "proof_stale", false)
	complete := fixture.noAttemptState(t, "complete", false)

	inFlightP2Fixture := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
	inFlightP2Fixture.useAdvisoryLease()
	inFlightP2 := inFlightP2Fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./second/...", Status: "missing"},
	})
	inFlightP2.StepID = &stepID2
	inFlightP2.Resumability.Step.ID = "P2"
	inFlightP2.Resumability.Attempt.ID = "A-P2"

	af := &fakeAF{
		nextSteps:   []string{"P2"},
		nextActions: []agentflow.NextActionState{initial, unclaimedP2, inFlightP2, stale, complete},
	}
	d := &driver{af: af, plan: recoveryPlanTwoSteps()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}
	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("resume state = %q, err = %v; want complete", final.State, err)
	}
	want := []string{
		"probe", "next-action", "complete-step:P1:A1", "next-action",
		"next-step", "claim:P2", "model:P2",
		"next-action", "gate:P2:unit-tests-2", "finish-step:P2:A-P2",
		"next-step", "next-action", "finish-run", "next-action",
	}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
}

// #689: a progressed run-phase state is validated before resume acts on it.
// A projected step, or a contract from another plan, is refused before
// next-step or finish-run.
func TestResumeRefusesMalformedProgressedProofState(t *testing.T) {
	for _, test := range []struct {
		name, want string
		progressed func(t *testing.T) agentflow.NextActionState
	}{
		{
			name: "projected step",
			want: `agentflow state "proof_stale" unexpectedly projects a step`,
			progressed: func(t *testing.T) agentflow.NextActionState {
				f := newResumeFixture(t)
				f.useAdvisoryLease()
				state := f.noAttemptState(t, "proof_stale", true)
				state.StepID = nil
				return state
			},
		},
		{
			name: "foreign plan contract",
			want: "task plan does not match Agentflow resumability contract",
			progressed: func(t *testing.T) agentflow.NextActionState {
				f := newResumeFixtureWithPlan(t, recoveryPlanTwoSteps())
				f.useAdvisoryLease()
				return f.noAttemptState(t, "proof_stale", false)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			initial := fixture.attemptState(t, "step_uncompleted", nil)
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, test.progressed(t)}}
			d := &driver{af: af, plan: recoveryPlan()}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || err.Error() != test.want {
				t.Fatalf("resume err = %v\nwant %s", err, test.want)
			}
			if want := []string{"probe", "next-action", "complete-step:P1:A1", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
}

// #689: entry dispositions are unchanged. With a valid run-phase projection
// (so only the disposition can refuse), each state is refused read-only with
// its own description.
func TestResumeEntryRunPhaseRefusalsAreExact(t *testing.T) {
	for state, want := range map[string]string{
		"proof_stale":   `agentflow resume blocked in state "proof_stale": blocked by stale proof`,
		"proof_failing": `agentflow resume blocked in state "proof_failing": blocked by failing proof`,
		"drift_failing": `agentflow resume blocked in state "drift_failing": blocked by drift findings`,
	} {
		t.Run(state, func(t *testing.T) {
			fixture := newResumeFixture(t)
			af := &fakeAF{nextActions: []agentflow.NextActionState{fixture.noAttemptState(t, state, false)}}
			d := &driver{af: af, plan: recoveryPlan()}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || err.Error() != want {
				t.Fatalf("resume err = %v\nwant %s", err, want)
			}
			if seq := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, seq) {
				t.Fatalf("seq = %v, want read-only %v", af.seq, seq)
			}
		})
	}
}

// #652 A6: resume blocks an open attempt whose command gate failed, runs no
// gate on it (not even the missing one before the failed one), then claims a
// new attempt and runs the model.
func TestResumeBlocksFailedGateAttemptThenRerunsStep(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := fixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "failed"},
	})
	complete := fixture.noAttemptState(t, "complete", false)
	unclaimedFixture := newResumeFixture(t)
	unclaimedFixture.useAdvisoryLease()
	unclaimed := unclaimedFixture.noAttemptState(t, "step_unclaimed", true)
	unclaimed.Resumability.Step.State = "blocked"
	inFlightFixture := newResumeFixture(t)
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{initial, unclaimed, inFlight, finishRunGuard, complete}}
	d := &driver{af: af, plan: recoveryPlan()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}

	final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("state = %q, err = %v", final.State, err)
	}
	want := []string{
		"probe", "next-action",
		"block-step:P1:A1:golem: resume found failed command gate",
		"next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "next-action", "finish-run", "next-action",
	}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v\nwant %v", af.seq, want)
	}
	assertFreshBlockContext(t, af.blocks)
}

// failedGateRefusals are #652 A6's exceptions: each refuses before any
// mutation. status is the text human status shows after "resume: blocked";
// recoveryDisplayText prints a quote as \".
var failedGateRefusals = []struct {
	name, resume, status string
	advisory             bool
	mutate               func(f *resumeFixture)
}{
	{name: "finite enforced live lease",
		resume: "agentflow resume blocks validation_missing under a finite enforced lease because Agentflow cannot guarantee duplicate-free, no-renew recovery",
		status: "by unsafe or malformed projection: agentflow resume blocks validation_missing under a finite enforced lease"},
	{name: "enforced no-deadline lease", mutate: func(f *resumeFixture) {
		lease := f.projection()["lease"].(map[string]any)
		lease["state"], lease["expires_at"] = "no_deadline", nil
	},
		resume: "agentflow resume blocks validation_missing with a failed command gate under an enforced lease because blocking the attempt would leave step_unclaimed, which an enforced lease refuses",
		status: "by unsafe or malformed projection: agentflow resume blocks validation_missing with a failed command gate under an enforced lease"},
	{name: "foreign owner", advisory: true, mutate: func(f *resumeFixture) {
		f.projection()["attempt"].(map[string]any)["owner"] = "other"
	},
		resume: `agentflow resumability attempt "A1" is owned by "other", want "golem"`,
		status: `by unsafe or malformed projection: agentflow resumability attempt \"A1\" is owned by \"other\", want \"golem\"`},
	{name: "continue not allowed", advisory: true, mutate: func(f *resumeFixture) {
		f.projection()["recovery_actions"].([]any)[0].(map[string]any)["allowed"] = false
	},
		resume: "agentflow resumability does not allow automatic non-break-glass continue",
		status: "by unsafe or malformed projection: agentflow resumability does not allow automatic non-break-glass continue"},
	{name: "continue not automatic", advisory: true, mutate: func(f *resumeFixture) {
		f.projection()["recovery_actions"].([]any)[0].(map[string]any)["automatic"] = false
	},
		resume: "agentflow resumability does not allow automatic non-break-glass continue",
		status: "by unsafe or malformed projection: agentflow resumability does not allow automatic non-break-glass continue"},
	{name: "continue break-glass", advisory: true, mutate: func(f *resumeFixture) {
		f.projection()["recovery_actions"].([]any)[0].(map[string]any)["break_glass"] = true
	},
		resume: "agentflow resumability does not allow automatic non-break-glass continue",
		status: "by unsafe or malformed projection: agentflow resumability does not allow automatic non-break-glass continue"},
}

// failedGateState is resumeFixture's owned A1 with a missing gate before a
// failed one.
func failedGateState(t *testing.T, f *resumeFixture) agentflow.NextActionState {
	return f.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "failed"},
	})
}

func TestResumeRefusesFailedGateRecoveryBeforeMutation(t *testing.T) {
	for _, tt := range failedGateRefusals {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			if tt.advisory {
				fixture.useAdvisoryLease()
			}
			_ = failedGateState(t, &fixture)
			if tt.mutate != nil {
				tt.mutate(&fixture)
			}
			af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{fixture.state(t)}}
			models := 0
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
				models++
				return nil
			}}
			_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
			if err == nil || err.Error() != tt.resume {
				t.Fatalf("err = %v\nwant %s", err, tt.resume)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) || models != 0 || len(af.blocks) != 0 {
				t.Fatalf("seq = %v, model runs = %d, blocks = %d; want %v and none", af.seq, models, len(af.blocks), want)
			}
		})
	}
}

// #652 A6: a block that fails (AgentFlow 1.1 also refuses an attempt already
// closed, agentflow#61) names the failed gate and the block outcome, runs
// nothing more, and exits through the error (#611 D6).
func TestResumeFailedGateBlockFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		blockErr  error
		stepRuns  string
		want      string
		wantSeq   []string
		wantBlock int
	}{
		{name: "block rejected", blockErr: errors.New("block rejected"),
			want:      `step P1 attempt A1: gate "lint" already failed; record blocked attempt: block rejected`,
			wantSeq:   []string{"probe", "next-action", "block-step:P1:A1:golem: resume found failed command gate"},
			wantBlock: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			state := failedGateState(t, &fixture)
			if tt.stepRuns != "" {
				writeAgentFile(t, fixture.root, "step-runs.jsonl", tt.stepRuns)
			}
			af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{state}}
			if tt.blockErr != nil {
				af.failAt = map[string]error{"block-step": tt.blockErr}
			}
			models := 0
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
				models++
				return nil
			}}
			_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v\nwant %s", err, tt.want)
			}
			if tt.blockErr != nil && !errors.Is(err, tt.blockErr) {
				t.Fatalf("err = %v does not wrap the block error", err)
			}
			if !reflect.DeepEqual(af.seq, tt.wantSeq) || models != 0 || len(af.blocks) != tt.wantBlock {
				t.Fatalf("seq = %v, model runs = %d, blocks = %d; want %v, none, %d", af.seq, models, len(af.blocks), tt.wantSeq, tt.wantBlock)
			}
		})
	}
}

// #652 D1: an open review-amendment attempt sits on a step AgentFlow already
// counts completed, so recovery refuses it before the gate projection, failed
// gate or not. A6 never blocks it.
func TestFailedGateAmendmentAttemptStaysRefused(t *testing.T) {
	const reason = "agentflow resumability open attempt belongs to a completed step"
	amendment := func(t *testing.T) resumeFixture {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		_ = failedGateState(t, &fixture)
		projection := fixture.projection()
		projection["step"].(map[string]any)["state"] = "amendment_started"
		projection["step"].(map[string]any)["completed"] = true
		projection["attempt"].(map[string]any)["state"] = "amendment_started"
		return fixture
	}
	t.Run("status", func(t *testing.T) {
		fixture := amendment(t)
		payload, err := json.Marshal(fixture.payload)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
		var exit *agentflowStatusExit
		if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(out.String(), "\nresume: blocked by unsafe or malformed projection: "+reason+"\n") {
			t.Fatalf("err = %v, want exit 3 with the completed-step refusal\n%s", err, out.String())
		}
	})
	t.Run("resume", func(t *testing.T) {
		fixture := amendment(t)
		af := &fakeAF{nextActions: []agentflow.NextActionState{fixture.state(t)}}
		d := &driver{af: af, plan: recoveryPlan()}
		if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || err.Error() != reason {
			t.Fatalf("err = %v, want %q", err, reason)
		}
		if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
			t.Fatalf("seq = %v, want %v", af.seq, want)
		}
	})
}

// retainedTimeoutStates are the resumeSerial states recovery would otherwise
// enter, as valid advisory projections.
var retainedTimeoutStates = []struct {
	name  string
	state func(t *testing.T, f *resumeFixture) agentflow.NextActionState
}{
	{"step_unclaimed", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.noAttemptState(t, "step_unclaimed", true)
	}},
	{"validation_missing", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
		})
	}},
	{"run_unverified", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.noAttemptState(t, "run_unverified", false)
	}},
	{"step_unverified", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.attemptState(t, "step_unverified", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "satisfied"},
			{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
		})
	}},
	{"step_uncompleted", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.attemptState(t, "step_uncompleted", []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "satisfied"},
			{Kind: "command", Label: "go vet ./...", Status: "satisfied"},
		})
	}},
	{"proof_missing", func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.noAttemptState(t, "proof_missing", false)
	}},
	{"validation_missing with a failed gate", failedGateState},
}

// #642 A9: status turns every otherwise-resumable state into a blocked exit 3
// when root retains a timed-out receipt no attempt can supersede; an ordinary
// failed receipt, or a timeout whose attempt was blocked (agentflow#60), stays
// resumable. JSON mode keeps its contract: the raw payload, exit 3, no
// diagnostic.
func TestAgentflowStatus_RetainedTimeoutBlocksResumableStates(t *testing.T) {
	for _, st := range retainedTimeoutStates {
		for _, receipt := range []struct {
			name, body, stepRuns string
			timeout              bool
		}{
			{"timeout", timeoutReceiptCR1, "", true},
			{"recoverable timeout", timeoutReceiptCR1, blockedA1Rows, false},
			{"exit-1", exitOneReceiptCR1, "", false},
		} {
			for _, jsonOutput := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/json=%t", st.name, receipt.name, jsonOutput), func(t *testing.T) {
					fixture := newResumeFixture(t)
					fixture.useAdvisoryLease()
					state := st.state(t, &fixture)
					writeAgentFile(t, fixture.root, "command-receipts.jsonl", receipt.body+"\n")
					if receipt.stepRuns != "" {
						writeAgentFile(t, fixture.root, "step-runs.jsonl", receipt.stepRuns)
					}
					payload, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					var out bytes.Buffer
					err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, jsonOutput, &recoveryRunner{payload: payload})
					var exit *agentflowStatusExit
					if !errors.As(err, &exit) {
						t.Fatalf("err = %v, want a status exit", err)
					}
					wantCode := 2
					if receipt.timeout {
						wantCode = 3
					}
					if exit.ExitCode() != wantCode || exit.diagnostic != "" {
						t.Fatalf("exit = %d diagnostic = %q, want %d and none\n%s", exit.ExitCode(), exit.diagnostic, wantCode, out.String())
					}
					switch {
					case jsonOutput:
						if !bytes.Equal(out.Bytes(), payload) {
							t.Fatalf("JSON output = %q, want exact payload %q", out.Bytes(), payload)
						}
					case receipt.timeout:
						if want := "\nresume: blocked: " + wantTimeoutReport(fixture.root, "CR1") + "\n"; !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "blocking: true") {
							t.Fatalf("output missing %q:\n%s", want, out.String())
						}
					default:
						if strings.Contains(out.String(), "resume: blocked") {
							t.Fatalf("%s receipt blocked recovery:\n%s", receipt.name, out.String())
						}
					}
				})
			}
		}
	}
}

// #642 D9: the projection checks keep precedence over the timeout report, and
// complete keeps its verified-proof path. (#652 moved the failed-gate row to
// retainedTimeoutStates: with failed known, status reaches the A9 report as
// resume does.)
func TestAgentflowStatus_RetainedTimeoutKeepsEarlierRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		mutate func(f *resumeFixture)
		want   string
	}{
		{name: "foreign owner", state: retainedTimeoutStates[1].state, mutate: func(f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["owner"] = "other"
		}, want: "resume: blocked by unsafe or malformed projection: agentflow resumability attempt "},
		{name: "expired lease", state: retainedTimeoutStates[1].state, mutate: func(f *resumeFixture) {
			f.projection()["lease"].(map[string]any)["state"] = "expired"
		}, want: "resume: blocked by unsafe or malformed projection: agentflow resumability lease state "},
		{name: "unlocked contract", state: retainedTimeoutStates[0].state, mutate: func(f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["locked"] = false
		}, want: "\nresume: blocked by unsafe or malformed projection: agentflow resumability contract is not locked\n"},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", tt.name, jsonOutput), func(t *testing.T) {
				fixture := newResumeFixture(t)
				fixture.useAdvisoryLease()
				_ = tt.state(t, &fixture)
				if tt.mutate != nil {
					tt.mutate(&fixture)
				}
				writeAgentFile(t, fixture.root, "command-receipts.jsonl", timeoutReceiptCR1+"\n")
				payload, err := json.Marshal(fixture.payload)
				if err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, jsonOutput, &recoveryRunner{payload: payload})
				var exit *agentflowStatusExit
				if !errors.As(err, &exit) || exit.ExitCode() != 3 || exit.diagnostic != "" {
					t.Fatalf("err = %v, want status exit 3 without a diagnostic", err)
				}
				if jsonOutput {
					if !bytes.Equal(out.Bytes(), payload) {
						t.Fatalf("JSON output = %q, want exact payload %q", out.Bytes(), payload)
					}
					return
				}
				if !strings.Contains(out.String(), tt.want) || strings.Contains(out.String(), "retained timeout receipt") {
					t.Fatalf("output lacks the earlier refusal %q, or shows the timeout report:\n%s", tt.want, out.String())
				}
			})
		}
	}
	t.Run("complete", func(t *testing.T) {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		state := fixture.noAttemptState(t, "complete", false)
		writeAgentFile(t, fixture.root, "command-receipts.jsonl", timeoutReceiptCR1+"\n")
		writeAgentFile(t, fixture.root, "proof-pack.json", `{"checks":[{"status":"passed"}]}`)
		payload, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload}); err != nil {
			t.Fatalf("err = %v, want exit 0\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "proof: verified") || strings.Contains(out.String(), "retained timeout receipt") {
			t.Fatalf("complete left its verified-proof path:\n%s", out.String())
		}
	})
}

// #642 A9: resume refuses a root that retains a timed-out receipt no attempt
// can supersede after its read-only next-action and before any mutation or
// model call.
func TestResumeRefusesRetainedTimeoutBeforeMutation(t *testing.T) {
	type row struct {
		name     string
		state    func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		receipts string
		id       string
	}
	var rows []row
	for _, st := range retainedTimeoutStates {
		rows = append(rows, row{st.name, st.state, timeoutReceiptCR1, "CR1"})
	}
	rows = append(rows,
		row{"empty-id timeout", retainedTimeoutStates[0].state, `{"schema_version":"1.0.0","id":"","decision":"timeout","timed_out":true}`, ".agent/command-receipts.jsonl:1"},
	)
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			state := tt.state(t, &fixture)
			writeAgentFile(t, fixture.root, "command-receipts.jsonl", tt.receipts+"\n")
			af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{state}}
			modelCalls := 0
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
				modelCalls++
				return nil
			}}
			_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
			if want := wantTimeoutReport(fixture.root, tt.id); err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) || modelCalls != 0 {
				t.Fatalf("seq = %v, model calls = %d; want %v and none", af.seq, modelCalls, want)
			}
		})
	}
	// complete mutates nothing, so it reports without the refusal.
	t.Run("complete", func(t *testing.T) {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		state := fixture.noAttemptState(t, "complete", false)
		writeAgentFile(t, fixture.root, "command-receipts.jsonl", timeoutReceiptCR1+"\n")
		af := &fakeAF{nextActions: []agentflow.NextActionState{state}}
		d := &driver{af: af, plan: recoveryPlan()}
		final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
		if err != nil || final.State != "complete" {
			t.Fatalf("state = %q, err = %v; want complete and no error", final.State, err)
		}
		if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
			t.Fatalf("seq = %v, want %v", af.seq, want)
		}
	})
	for name, rows := range map[string][2]string{
		"exit-1 receipt":      {exitOneReceiptCR1, ""},
		"recoverable timeout": {timeoutReceiptCR1, blockedA1Rows},
	} {
		t.Run("control: "+name+" settles", func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			initial := retainedTimeoutStates[1].state(t, &fixture)
			progressed := fixture.noAttemptState(t, "run_unverified", false)
			complete := fixture.noAttemptState(t, "complete", false)
			writeAgentFile(t, fixture.root, "command-receipts.jsonl", rows[0]+"\n")
			if rows[1] != "" {
				writeAgentFile(t, fixture.root, "step-runs.jsonl", rows[1])
			}
			af := &fakeAF{nextActions: []agentflow.NextActionState{initial, progressed, finishRunGuard, complete}}
			d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil {
				t.Fatal(err)
			}
			want := []string{"probe", "next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A1", "next-action", "next-step", "next-action", "finish-run", "next-action"}
			if !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
}

// #642 D9: resume keeps its earlier refusals (a fail-closed state, the
// projection and binding checks, a finite enforced lease) ahead of the A9
// timeout refusal.
func TestResumeKeepsEarlierRefusalsOverRetainedTimeout(t *testing.T) {
	missing := []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	}
	validationMissing := func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		return f.attemptState(t, "validation_missing", missing)
	}
	for _, tt := range []struct {
		name     string
		advisory bool
		state    func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		mutate   func(f *resumeFixture)
		want     string
	}{
		{name: "fail-closed state", advisory: true, state: func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
			return f.noAttemptState(t, "drift_failing", false)
		}, want: `agentflow resume blocked in state "drift_failing": blocked by drift findings`},
		{name: "foreign owner", advisory: true, state: validationMissing, mutate: func(f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["owner"] = "other"
		}, want: `agentflow resumability attempt "A1" is owned by "other", want "golem"`},
		{name: "plan digest mismatch", advisory: true, state: validationMissing, mutate: func(f *resumeFixture) {
			f.projection()["contract"].(map[string]any)["plan_sha256"] = "other"
		}, want: "task plan does not match Agentflow resumability contract"},
		{name: "finite enforced live lease", state: validationMissing,
			want: "agentflow resume blocks validation_missing under a finite enforced lease because Agentflow cannot guarantee duplicate-free, no-renew recovery"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			if tt.advisory {
				fixture.useAdvisoryLease()
			}
			_ = tt.state(t, &fixture)
			if tt.mutate != nil {
				tt.mutate(&fixture)
			}
			writeAgentFile(t, fixture.root, "command-receipts.jsonl", timeoutReceiptCR1+"\n")
			af := &fakeAF{nextActions: []agentflow.NextActionState{fixture.state(t)}}
			d := &driver{af: af, plan: recoveryPlan()}
			_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v\nwant %s", err, tt.want)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
}

// #642 A9: status blocks an otherwise-resumable state whose command-receipt
// ledger cannot be read, rather than reading it as no timeout.
func TestAgentflowStatus_UnreadableReceiptsBlocksResume(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	state := fixture.noAttemptState(t, "step_unclaimed", true)
	writeAgentFile(t, fixture.root, "command-receipts.jsonl", "{\n")
	payload, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
	var exit *agentflowStatusExit
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || exit.diagnostic != "" {
		t.Fatalf("err = %v, want status exit 3 without a diagnostic\n%s", err, out.String())
	}
	if want := "\nresume: blocked: " + incompatibleLedger + "\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("output missing %q:\n%s", want, out.String())
	}
}

// timedOutState is resumeFixture's owned A1 as AgentFlow 1.1 projects an
// attempt holding a timed-out gate receipt: continue denied, block allowed
// (agentflow#70).
func timedOutState(t *testing.T, f *resumeFixture) agentflow.NextActionState {
	_ = f.attemptState(t, "attempt_timed_out", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "failed"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	f.projection()["recovery_actions"] = []any{
		map[string]any{"action": "continue", "allowed": false, "automatic": true, "break_glass": false, "reason": "timed out"},
		map[string]any{"action": "block", "allowed": true, "automatic": true, "break_glass": false, "reason": "timed out"},
	}
	return f.state(t)
}

// amendmentNeededState is AgentFlow 1.1's step_amendment_needed: no open
// attempt, a completed P1 whose latest attempt was blocked, amend allowed
// (agentflow#70).
func amendmentNeededState(t *testing.T, f *resumeFixture) agentflow.NextActionState {
	_ = f.noAttemptState(t, "step_amendment_needed", true)
	projection := f.projection()
	projection["step"] = map[string]any{"id": "P1", "state": "blocked", "completed": true}
	projection["recovery_actions"] = []any{
		map[string]any{"action": "claim", "allowed": false, "automatic": true, "break_glass": false, "reason": "use amend"},
		map[string]any{"action": "amend", "allowed": true, "automatic": true, "break_glass": false, "reason": "needs a replacement attempt"},
	}
	return f.state(t)
}

// asAmendment puts the fixture's open attempt on a step AgentFlow already
// counts completed, as amend-step opens it.
func asAmendment(f *resumeFixture, attempt string) {
	projection := f.projection()
	projection["step"].(map[string]any)["completed"] = true
	projection["step"].(map[string]any)["state"] = "amendment_started"
	projection["attempt"].(map[string]any)["state"] = "amendment_started"
	projection["attempt"].(map[string]any)["id"] = attempt
}

func TestValidateRecoveryProjectionAgentflow11States(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		mutate func(f *resumeFixture)
		want   string // "" means valid
	}{
		{name: "timed-out attempt", state: timedOutState},
		{name: "timed-out amendment", state: timedOutState, mutate: func(f *resumeFixture) { asAmendment(f, "A1") }},
		{name: "timed-out attempt without block", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["recovery_actions"].([]any)[1].(map[string]any)["allowed"] = false
		}, want: "agentflow resumability does not allow automatic non-break-glass block"},
		{name: "timed-out attempt with only continue", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["recovery_actions"] = []any{
				map[string]any{"action": "continue", "allowed": true, "automatic": true, "break_glass": false, "reason": "owner"},
			}
		}, want: "agentflow resumability does not allow automatic non-break-glass block"},
		// A verified attempt is still open, and a later command can time out.
		{name: "timed-out verified attempt", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["step"].(map[string]any)["state"] = "verified"
			f.projection()["attempt"].(map[string]any)["state"] = "verified"
		}},
		{name: "timed-out completed attempt", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["step"].(map[string]any)["state"] = "completed"
			f.projection()["attempt"].(map[string]any)["state"] = "completed"
		}, want: `agentflow attempt_timed_out attempt has state "completed"`},
		// AgentFlow lets a timed-out advisory attempt be blocked whatever its
		// lease; an expired enforced one is reclaim-only.
		{name: "timed-out attempt under an expired advisory lease", state: timedOutState, mutate: func(f *resumeFixture) {
			lease := f.projection()["lease"].(map[string]any)
			lease["state"], lease["expires_at"] = "expired", "2026-10-18T10:00:00+00:00"
		}},
		{name: "expired advisory lease without a deadline", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["lease"].(map[string]any)["state"] = "expired"
		}, want: `agentflow resumability lease state "expired" is not recoverable`},
		{name: "timed-out attempt under an expired enforced lease", state: timedOutState, mutate: func(f *resumeFixture) {
			lease := f.projection()["lease"].(map[string]any)
			lease["policy"], lease["exclusive"], lease["state"], lease["expires_at"] = "enforce", true, "expired", "2026-10-18T10:00:00+00:00"
		}, want: `agentflow resumability lease state "expired" is not recoverable`},
		{name: "control: expired advisory lease in validation_missing", state: failedGateState, mutate: func(f *resumeFixture) {
			lease := f.projection()["lease"].(map[string]any)
			lease["state"], lease["expires_at"] = "expired", "2026-10-18T10:00:00+00:00"
		}, want: `agentflow resumability lease state "expired" is not recoverable`},
		{name: "timed-out state without an attempt", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["attempt"] = nil
			lease := f.projection()["lease"].(map[string]any)
			lease["state"], lease["expires_at"] = "not_applicable", nil
		}, want: `agentflow state "attempt_timed_out" requires an open attempt`},
		{name: "timed-out attempt of another owner", state: timedOutState, mutate: func(f *resumeFixture) {
			f.projection()["attempt"].(map[string]any)["owner"] = "other"
		}, want: `agentflow resumability attempt "A1" is owned by "other", want "golem"`},
		{name: "amendment needed", state: amendmentNeededState},
		{name: "amendment needed on an incomplete step", state: amendmentNeededState, mutate: func(f *resumeFixture) {
			f.projection()["step"].(map[string]any)["completed"] = false
		}, want: "agentflow step_amendment_needed projection has no completed step"},
		{name: "amendment needed without a step", state: amendmentNeededState, mutate: func(f *resumeFixture) {
			f.projection()["step"] = nil
			delete(f.payload, "step_id")
		}, want: "agentflow step_amendment_needed projection has no completed step"},
		{name: "amendment needed without amend", state: amendmentNeededState, mutate: func(f *resumeFixture) {
			f.projection()["recovery_actions"].([]any)[1].(map[string]any)["allowed"] = false
		}, want: "agentflow resumability does not allow automatic non-break-glass amend"},
		{name: "amendment needed with an open attempt", state: amendmentNeededState, mutate: func(f *resumeFixture) {
			f.projection()["attempt"] = map[string]any{"id": "A2", "state": "claimed", "owner": "golem", "open": true}
			lease := f.projection()["lease"].(map[string]any)
			lease["state"] = "no_deadline"
		}, want: `agentflow state "step_amendment_needed" unexpectedly projects an open attempt`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			_ = tt.state(t, &fixture)
			if tt.mutate != nil {
				tt.mutate(&fixture)
			}
			state := fixture.state(t)
			err := validateRecoveryProjection(state)
			if err == nil {
				err = refuseCompletedStepAttempt(state)
			}
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want valid", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v\nwant %s", err, tt.want)
			}
		})
	}
}

// Under an enforced lease, every close leads to a claim or amend, which opens
// a finite enforced attempt that resume refuses (#652 A6 pattern), so both
// AgentFlow 1.1 states are refused before any mutation; status says so.
func TestAgentflowStatus_Agentflow11States(t *testing.T) {
	for _, tt := range []struct {
		name     string
		state    func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		advisory bool
		code     int
		want     string
	}{
		{name: "timed-out attempt", state: timedOutState, advisory: true, code: 2,
			want: "\nresume: block the timed-out attempt, then re-run the step serially\n"},
		{name: "amendment needed", state: amendmentNeededState, advisory: true, code: 2,
			want: "\nresume: amend the completed step, then continue serially\n"},
		{name: "enforced timed-out attempt", state: timedOutState, code: 3,
			want: "\nresume: blocked by unsafe or malformed projection: agentflow resume blocks attempt_timed_out under an enforced lease because blocking the attempt would leave a step to claim or amend, which an enforced lease refuses\n"},
		{name: "enforced amendment needed", state: amendmentNeededState, code: 3,
			want: "\nresume: blocked by unsafe or malformed projection: agentflow resume blocks step_amendment_needed under an enforced lease because amending would enter non-atomic gate recovery\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			if tt.advisory {
				fixture.useAdvisoryLease()
			}
			payload, err := json.Marshal(tt.state(t, &fixture))
			if err != nil {
				t.Fatal(err)
			}
			runner := &recoveryRunner{payload: payload}
			var out bytes.Buffer
			err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, runner)
			var exit *agentflowStatusExit
			if !errors.As(err, &exit) || exit.ExitCode() != tt.code || !strings.Contains(out.String(), tt.want) {
				t.Fatalf("err = %v, want exit %d with %q\n%s", err, tt.code, tt.want, out.String())
			}
			for _, call := range runner.calls {
				if call[0] != "--version" && call[0] != "next-action" {
					t.Fatalf("status ran %v", call)
				}
			}
		})
	}
}

// agentflow#70: resume blocks an attempt AgentFlow reports attempt_timed_out
// (it can never complete), then the serial loop claims a new attempt and runs
// the model, as for a failed gate (#652 A6).
func TestResumeBlocksTimedOutAttemptThenRerunsStep(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	initial := timedOutState(t, &fixture)
	unclaimedFixture := newResumeFixture(t)
	unclaimedFixture.useAdvisoryLease()
	unclaimed := unclaimedFixture.noAttemptState(t, "step_unclaimed", true)
	unclaimed.Resumability.Step.State = "blocked"
	inFlightFixture := newResumeFixture(t)
	inFlightFixture.useAdvisoryLease()
	inFlight := inFlightFixture.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	inFlight.Resumability.Attempt.ID = "A-P1"
	complete := fixture.noAttemptState(t, "complete", false)
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{initial, unclaimed, inFlight, finishRunGuard, complete}}
	d := &driver{af: af, plan: recoveryPlan()}
	d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
		af.seq = append(af.seq, "model:"+step.ID)
		return nil
	}
	if final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil || final.State != "complete" {
		t.Fatalf("state = %q, err = %v", final.State, err)
	}
	want := []string{
		"probe", "next-action",
		"block-step:P1:A1:golem: resume found timed-out gate",
		"next-action", "next-step", "claim:P1", "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:A-P1",
		"next-step", "next-action", "finish-run", "next-action",
	}
	if !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("seq = %v\nwant %v", af.seq, want)
	}
	assertFreshBlockContext(t, af.blocks)
}

// inFlightAmendment is the validation_missing projection of the amendment
// attempt resume opened itself (fakeAF names it AM-P1).
func inFlightAmendment(t *testing.T) agentflow.NextActionState {
	f := newResumeFixture(t)
	f.useAdvisoryLease()
	_ = f.attemptState(t, "validation_missing", []agentflow.ResumabilityGate{
		{Kind: "command", Label: "go test ./...", Status: "missing"},
		{Kind: "command", Label: "go vet ./...", Status: "missing"},
	})
	asAmendment(&f, "AM-P1")
	return f.state(t)
}

// agentflow#70: a completed step that needs a replacement attempt is amended,
// and the amendment runs as a claimed attempt does: the step's goal, its gates
// (beforeGates accepts the completed step for the attempt resume opened), then
// finish-step. Entered directly, or after blocking a timed-out amendment.
func TestResumeAmendsCompletedStep(t *testing.T) {
	const amend = "amend-completed:P1:golem: resume replaces the step's attempt"
	tail := []string{
		amend, "model:P1",
		"next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:AM-P1",
		"next-step", "next-action", "finish-run", "next-action",
	}
	for _, tt := range []struct {
		name  string
		entry func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		head  []string
	}{
		{name: "step_amendment_needed", entry: amendmentNeededState, head: []string{"probe", "next-action"}},
		{name: "timed-out amendment", entry: func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
			_ = timedOutState(t, f)
			asAmendment(f, "A2")
			return f.state(t)
		}, head: []string{"probe", "next-action", "block-step:P1:A2:golem: resume found timed-out gate", "next-action"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			states := []agentflow.NextActionState{tt.entry(t, &fixture)}
			if tt.name != "step_amendment_needed" {
				needed := newResumeFixture(t)
				needed.useAdvisoryLease()
				states = append(states, amendmentNeededState(t, &needed))
			}
			complete := fixture.noAttemptState(t, "complete", false)
			states = append(states, inFlightAmendment(t), finishRunGuard, complete)
			af := &fakeAF{nextActions: states}
			goals := 0
			d := &driver{af: af, plan: recoveryPlan()}
			d.runStep = func(_ context.Context, step agentflow.Step, attempt, goal string) error {
				af.seq = append(af.seq, "model:"+step.ID)
				if want, _ := stepGoal(recoveryPlan(), step); attempt != "AM-P1" || goal != want {
					t.Errorf("model ran attempt %q with goal %q, want AM-P1 with the step goal", attempt, goal)
				}
				goals++
				return nil
			}
			if final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err != nil || final.State != "complete" {
				t.Fatalf("state = %q, err = %v\nseq = %v", final.State, err, af.seq)
			}
			if want := append(slices.Clone(tt.head), tail...); !reflect.DeepEqual(af.seq, want) || goals != 1 {
				t.Fatalf("seq = %v, model runs = %d\nwant %v and one", af.seq, goals, want)
			}
		})
	}
}

// A timed-out attempt whose block AgentFlow refuses (for example, an operator
// closed it first: agentflow#61) stops resume with both causes and runs
// nothing more.
func TestResumeTimedOutBlockFailureStops(t *testing.T) {
	fixture := newResumeFixture(t)
	fixture.useAdvisoryLease()
	blockErr := &agentflow.CommandError{Cmd: "block-step", Exit: 2, Stderr: "attempt A1 is already failed"}
	af := &fakeAF{nextSteps: []string{"P1"}, nextActions: []agentflow.NextActionState{timedOutState(t, &fixture)}, failAt: map[string]error{"block-step": blockErr}}
	models := 0
	d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
		models++
		return nil
	}}
	_, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
	if want := "step P1 attempt A1: gate timed out; record blocked attempt: " + blockErr.Error(); err == nil || err.Error() != want || !errors.Is(err, blockErr) {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if want := []string{"probe", "next-action", "block-step:P1:A1:golem: resume found timed-out gate"}; !reflect.DeepEqual(af.seq, want) || models != 0 {
		t.Fatalf("seq = %v, model runs = %d; want %v and none", af.seq, models, want)
	}
}

// Resume refuses both AgentFlow 1.1 states under an enforced lease before any
// mutation.
func TestResumeRefusesAgentflow11StatesUnderEnforce(t *testing.T) {
	for name, state := range map[string]func(t *testing.T, f *resumeFixture) agentflow.NextActionState{
		"attempt_timed_out":     timedOutState,
		"step_amendment_needed": amendmentNeededState,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newResumeFixture(t)
			af := &fakeAF{nextActions: []agentflow.NextActionState{state(t, &fixture)}}
			d := &driver{af: af, plan: recoveryPlan()}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || !strings.Contains(err.Error(), "agentflow resume blocks "+name+" under an enforced lease") {
				t.Fatalf("err = %v", err)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
}

// reviewAmendmentRows are a completed P1 whose review amendment A2 (as Golem's
// AmendStep writes it) timed out and was blocked.
const reviewAmendmentRows = `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A1","event":"claimed","agent_id":"golem"}` + "\n" +
	`{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A1","event":"completed"}` + "\n" +
	`{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A2","event":"amendment_started","agent_id":"golem","reason":"address review findings RR-20261010T120000Z-643a643b#F1","reason_code":"review_feedback","finding_refs":[{"review_run_id":"RR-20261010T120000Z-643a643b","finding_id":"F1"}]}` + "\n" +
	`{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A2","event":"blocked","reason":"golem: gate execution failed"}` + "\n"

func TestUnfinishedReviewAmendment(t *testing.T) {
	row := func(attempt, event, extra string) string {
		return `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"` + attempt + `","event":"` + event + `"` + extra + `}` + "\n"
	}
	const completedA1 = `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A1","event":"claimed"}` + "\n" +
		`{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A1","event":"completed"}` + "\n"
	for _, tt := range []struct {
		name, ledger string
		want         bool
	}{
		{name: "no ledger"},
		{name: "blocked review amendment", ledger: reviewAmendmentRows, want: true},
		{name: "open review amendment", ledger: completedA1 + row("A2", "amendment_started", `,"reason_code":"review_feedback","finding_refs":[{"review_run_id":"RR-1","finding_id":"F1"}]`), want: true},
		{name: "finding refs alone", ledger: completedA1 + row("A2", "amendment_started", `,"finding_refs":[{"review_run_id":"RR-1","finding_id":"F1"}]`), want: true},
		{name: "review reason code alone", ledger: completedA1 + row("A2", "amendment_started", `,"reason_code":"review_feedback"`), want: true},
		{name: "completed review amendment", ledger: reviewAmendmentRows + row("A3", "amendment_started", `,"reason_code":"review_feedback"`) + row("A3", "completed", "")},
		{name: "plain amendment", ledger: completedA1 + row("A2", "amendment_started", `,"reason":"golem: resume replaces the step's attempt"`) + row("A2", "blocked", "")},
		{name: "empty finding refs", ledger: completedA1 + row("A2", "amendment_started", `,"finding_refs":[]`) + row("A2", "blocked", "")},
		{name: "another step's review amendment", ledger: completedA1 + `{"schema_version":"1.0.0","step_id":"P2","attempt_id":"A2","event":"amendment_started","reason_code":"review_feedback"}` + "\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.ledger != "" {
				writeAgentFile(t, root, "step-runs.jsonl", tt.ledger)
			}
			if got, err := unfinishedReviewAmendment(root, "P1"); err != nil || got != tt.want {
				t.Fatalf("unfinishedReviewAmendment = %t, %v; want %t", got, err, tt.want)
			}
		})
	}
	root := t.TempDir()
	writeAgentFile(t, root, "step-runs.jsonl", "{\n")
	if _, err := unfinishedReviewAmendment(root, "P1"); err == nil {
		t.Fatal("unreadable step-run ledger was read as no review amendment")
	}
}

// Resume replaces attempts with the step's own goal, so neither status nor
// resume offers to replace an unfinished review amendment: both refuse before
// any change, and a plain amendment history is the control.
func TestReviewAmendmentReplacementIsRefused(t *testing.T) {
	const refusal = "step P1 has an unfinished review amendment; resume would replace it with the step's own goal and drop the review findings, so it changes nothing; block the attempt if it is still open, then address the findings in a new -review-manifest round over the same plan"
	timedOutReview := func(t *testing.T, f *resumeFixture) agentflow.NextActionState {
		_ = timedOutState(t, f)
		asAmendment(f, "A2")
		return f.state(t)
	}
	openReviewRows := strings.TrimSuffix(reviewAmendmentRows, `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A2","event":"blocked","reason":"golem: gate execution failed"}`+"\n")
	for _, tt := range []struct {
		name   string
		state  func(t *testing.T, f *resumeFixture) agentflow.NextActionState
		ledger string
	}{
		{"step_amendment_needed", amendmentNeededState, reviewAmendmentRows},
		{"attempt_timed_out", timedOutReview, openReviewRows},
	} {
		t.Run(tt.name+"/status", func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			payload, err := json.Marshal(tt.state(t, &fixture))
			if err != nil {
				t.Fatal(err)
			}
			writeAgentFile(t, fixture.root, "step-runs.jsonl", tt.ledger)
			var out bytes.Buffer
			err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
			var exit *agentflowStatusExit
			if !errors.As(err, &exit) || exit.ExitCode() != 3 || !strings.Contains(out.String(), "\nresume: blocked: "+refusal+"\n") {
				t.Fatalf("err = %v, want exit 3 with the review refusal\n%s", err, out.String())
			}
		})
		t.Run(tt.name+"/resume", func(t *testing.T) {
			fixture := newResumeFixture(t)
			fixture.useAdvisoryLease()
			af := &fakeAF{nextActions: []agentflow.NextActionState{tt.state(t, &fixture)}}
			writeAgentFile(t, fixture.root, "step-runs.jsonl", tt.ledger)
			d := &driver{af: af, plan: recoveryPlan()}
			if _, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil); err == nil || err.Error() != refusal {
				t.Fatalf("err = %v\nwant %s", err, refusal)
			}
			if want := []string{"probe", "next-action"}; !reflect.DeepEqual(af.seq, want) {
				t.Fatalf("seq = %v, want %v", af.seq, want)
			}
		})
	}
	// The guard at the amend itself covers a step_amendment_needed reached
	// after settlement, whatever the entry check saw.
	t.Run("amend guard", func(t *testing.T) {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		state := amendmentNeededState(t, &fixture)
		writeAgentFile(t, fixture.root, "step-runs.jsonl", reviewAmendmentRows)
		af := &fakeAF{}
		d := &driver{af: af, plan: recoveryPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
			t.Fatal("the model ran")
			return nil
		}}
		if err := d.amendCompletedStep(context.Background(), fixture.root, state); err == nil || err.Error() != refusal || len(af.seq) != 0 {
			t.Fatalf("err = %v, seq = %v; want the refusal and no AgentFlow call", err, af.seq)
		}
	})
	t.Run("control: plain amendment history", func(t *testing.T) {
		fixture := newResumeFixture(t)
		fixture.useAdvisoryLease()
		payload, err := json.Marshal(amendmentNeededState(t, &fixture))
		if err != nil {
			t.Fatal(err)
		}
		writeAgentFile(t, fixture.root, "step-runs.jsonl", strings.ReplaceAll(strings.ReplaceAll(reviewAmendmentRows, `,"reason_code":"review_feedback"`, ""), `,"finding_refs":[{"review_run_id":"RR-20261010T120000Z-643a643b","finding_id":"F1"}]`, ""))
		var out bytes.Buffer
		err = runAgentflowStatusWithRunner(context.Background(), &out, fixture.root, false, &recoveryRunner{payload: payload})
		var exit *agentflowStatusExit
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("err = %v, want exit 2\n%s", err, out.String())
		}
	})
}

// One amendment per resume (agentflow#70): when two steps need a replacement
// attempt, the first resume amends P1, then stops at the step-work check
// before finish-run without building a proof; the next resume amends P2 and
// completes.
func TestResumeAmendsOneStepPerRun(t *testing.T) {
	plan := recoveryPlanTwoSteps()
	needed := func(t *testing.T, step string) agentflow.NextActionState {
		f := newResumeFixtureWithPlan(t, plan)
		f.useAdvisoryLease()
		state := amendmentNeededState(t, &f)
		state.Resumability.Step.ID = step
		state.StepID = &step
		return state
	}
	inFlight := func(t *testing.T, step string) agentflow.NextActionState {
		f := newResumeFixtureWithPlan(t, plan)
		f.useAdvisoryLease()
		gates := []agentflow.ResumabilityGate{
			{Kind: "command", Label: "go test ./...", Status: "missing"},
			{Kind: "command", Label: "go vet ./...", Status: "missing"},
		}
		if step == "P2" {
			gates = []agentflow.ResumabilityGate{{Kind: "command", Label: "go test ./second/...", Status: "missing"}}
		}
		_ = f.attemptState(t, "validation_missing", gates)
		asAmendment(&f, "AM-"+step)
		state := f.state(t)
		state.Resumability.Step.ID = step
		state.StepID = &step
		return state
	}
	fixture := newResumeFixtureWithPlan(t, plan)
	fixture.useAdvisoryLease()
	complete := fixture.noAttemptState(t, "complete", false)
	run := func(states []agentflow.NextActionState) (*fakeAF, agentflow.NextActionState, error) {
		af := &fakeAF{nextActions: states}
		d := &driver{af: af, plan: plan}
		d.runStep = func(_ context.Context, step agentflow.Step, _, _ string) error {
			af.seq = append(af.seq, "model:"+step.ID)
			return nil
		}
		final, err := d.resume(context.Background(), fixture.root, fixture.planJSON, nil)
		return af, final, err
	}
	const amend = ":golem: resume replaces the step's attempt"

	af, _, err := run([]agentflow.NextActionState{needed(t, "P1"), inFlight(t, "P1"), needed(t, "P2")})
	if want := `agentflow still reports step work before finish-run: state "step_amendment_needed" step "P2"; no proof was built; run -agentflow-status to see whether -agentflow-resume can recover it`; err == nil || err.Error() != want {
		t.Fatalf("resume 1 err = %v\nwant %s", err, want)
	}
	if want := []string{"probe", "next-action", "amend-completed:P1" + amend, "model:P1", "next-action", "gate:P1:unit-tests", "gate:P1:lint", "finish-step:P1:AM-P1", "next-step", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("resume 1 seq = %v\nwant %v", af.seq, want)
	}

	af, final, err := run([]agentflow.NextActionState{needed(t, "P2"), inFlight(t, "P2"), finishRunGuard, complete})
	if err != nil || final.State != "complete" {
		t.Fatalf("resume 2 state = %q, err = %v", final.State, err)
	}
	if want := []string{"probe", "next-action", "amend-completed:P2" + amend, "model:P2", "next-action", "gate:P2:unit-tests-2", "finish-step:P2:AM-P2", "next-step", "next-action", "finish-run", "next-action"}; !reflect.DeepEqual(af.seq, want) {
		t.Fatalf("resume 2 seq = %v\nwant %v", af.seq, want)
	}
}
