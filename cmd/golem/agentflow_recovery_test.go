package main

import (
	"bytes"
	"context"
	"errors"
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
	payload := []byte(`{"state":"validation_missing","reason":"gate missing","blocking":true,"command":"sh","args":["-c","touch /tmp/owned"],"step_id":"P1","gate":"unit-tests","diagnostics":["missing gate"],"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem","step":{"id":"P1","state":"in_progress","completed":false},"attempt":{"id":"A1","state":"claimed","owner":"golem","open":true},"lease":{"policy":"enforce","state":"live","exclusive":true},"gates":[{"kind":"command","label":"go test ./...","status":"missing","receipt_id":null,"evidence_id":null}],"recovery_actions":[{"action":"continue","allowed":true,"automatic":true,"break_glass":false,"reason":"owner holds lease"}],"diagnostics":[]}}`)
	runner := &recoveryRunner{payload: payload}
	var out bytes.Buffer

	err := runAgentflowStatusWithRunner(context.Background(), &out, root, false, runner)
	var statusErr *agentflowStatusExit
	if !errors.As(err, &statusErr) || statusErr.ExitCode() != 2 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"state: validation_missing", "reason: gate missing", "step: P1", "attempt: A1 owner=golem",
		"lease: policy=enforce state=live", "gate: command go test ./... (missing)",
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
	payload := []byte("{\"state\":\"uninitialized\",\"unknown\":{\"kept\":true}}\n")
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

func TestAgentflowStatus_CompleteRequiresAndSummarizesProof(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	proof := `{"checks":[{"status":"passed"},{"status":"warning"},{"status":"passed"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recoveryRunner{payload: []byte(`{"state":"complete","reason":"run complete and proof verified","blocking":false}`)}
	var out bytes.Buffer
	if err := runAgentflowStatusWithRunner(context.Background(), &out, root, false, runner); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proof: verified", filepath.Join(root, ".agent", "proof-pack.json"), "checks: passed=2 warning=1 failed=0 total=3"} {
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

var _ agentflow.Runner = (*recoveryRunner)(nil)
