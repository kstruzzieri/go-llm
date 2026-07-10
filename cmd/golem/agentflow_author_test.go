package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
)

// stubLocker satisfies afLocker; LockPlan returns a scripted error per call.
type stubLocker struct {
	probeErr error
	initErr  error
	lockErrs []error
	probes   int
	inits    int
	locks    int
	paths    []string
}

func (s *stubLocker) Probe(context.Context) error { s.probes++; return s.probeErr }
func (s *stubLocker) Init(context.Context) error  { s.inits++; return s.initErr }
func (s *stubLocker) LockPlan(_ context.Context, path string) error {
	s.paths = append(s.paths, path)
	i := s.locks
	s.locks++
	if i < len(s.lockErrs) {
		return s.lockErrs[i]
	}
	return nil
}

func validIRJSON(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(agentflow.PlanIR{
		Objective: "obj", Scope: []string{"src"}, Invariants: []string{"x"},
		RiskLevel: "low", RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Steps: []agentflow.StepIR{{ID: "S1", Action: "do", Files: []string{"src/a.go"}, ExpectedDiff: []string{"x"}, Validations: []agentflow.GateIR{{Argv: []string{"true"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSubmitPlanTool_Effect(t *testing.T) {
	as := &authorSession{root: "/root"}
	tool := newSubmitPlanTool(as)
	e := tool.Effect()
	if e.Class != agent.Write|agent.Exec {
		t.Errorf("effect class = %v, want Write|Exec", e.Class)
	}
	if e.Approval != agent.ApprovalNever {
		t.Errorf("approval = %v, want ApprovalNever", e.Approval)
	}
	if e.Scope.CWD != "/root" {
		t.Errorf("effect cwd = %q, want /root", e.Scope.CWD)
	}
}

func TestSubmitPlanTool_SuccessLocksAndCancels(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	canceled := false
	client := &stubLocker{}
	as := &authorSession{client: client, root: "/root",
		cancel: func() { canceled = true; cancel() }}
	tool := newSubmitPlanTool(as)
	res, err := tool.Invoke(context.Background(), validIRJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if as.lockedPath != filepath.Join("/root", ".agent", "plan.lock.json") || !canceled || res.IsError {
		t.Errorf("success path: locked=%q canceled=%v res=%+v", as.lockedPath, canceled, res)
	}
	if len(client.paths) != 1 {
		t.Fatalf("LockPlan calls = %d, want 1", len(client.paths))
	}
	if _, err := os.Stat(client.paths[0]); !os.IsNotExist(err) {
		t.Errorf("staged plan was not removed: %v", err)
	}
}

func TestSubmitPlanTool_FirstLocalFailureReturnsDiagnosticsNoCancel(t *testing.T) {
	canceled := false
	as := &authorSession{client: &stubLocker{}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	// Missing invariants etc: send an empty IR object.
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || canceled || len(as.lastDiags) == 0 {
		t.Errorf("attempt1 failure should return diagnostics without cancel; canceled=%v diags=%d", canceled, len(as.lastDiags))
	}
}

func TestSubmitPlanTool_SecondFailureCancels(t *testing.T) {
	canceled := 0
	as := &authorSession{client: &stubLocker{}, root: "/root", cancel: func() { canceled++ }}
	tool := newSubmitPlanTool(as)
	_, _ = tool.Invoke(context.Background(), json.RawMessage(`{}`))
	_, _ = tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if canceled == 0 {
		t.Errorf("second failed submission must cancel the loop")
	}
}

func TestSubmitPlanTool_TerminalLockErrorFailsFast(t *testing.T) {
	canceled := false
	term := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "invalid_arguments", Message: "boom"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{term}}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	res, _ := tool.Invoke(context.Background(), validIRJSON(t))
	if as.terminalErr == nil || !canceled || !res.IsError {
		t.Errorf("terminal lock error should fail fast; terminalErr=%v canceled=%v", as.terminalErr, canceled)
	}
}

func TestSubmitPlanTool_UnknownStructuredLockErrorFailsClosed(t *testing.T) {
	canceled := false
	term := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "future_error", Message: "unknown contract failure"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{term}}, root: "/root", cancel: func() { canceled = true }}
	_, _ = newSubmitPlanTool(as).Invoke(context.Background(), validIRJSON(t))
	if as.terminalErr == nil || !canceled {
		t.Fatal("unknown structured lock errors must fail closed")
	}
}

func TestSubmitPlanTool_ContentValidationErrorIsRepairable(t *testing.T) {
	canceled := false
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "steps[1].action must be a non-empty string"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{verr}}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	res, _ := tool.Invoke(context.Background(), validIRJSON(t))
	if canceled || !res.IsError || len(as.lastDiags) == 0 {
		t.Errorf("content validation_error should be repairable; canceled=%v", canceled)
	}
}

func TestSubmitPlanTool_CompilerOwnedValidationErrorIsTerminal(t *testing.T) {
	canceled := false
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "plan-lock schema_version 9.9.9 is incompatible"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{verr}}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	_, _ = tool.Invoke(context.Background(), validIRJSON(t))
	if as.terminalErr == nil || !canceled {
		t.Errorf("schema_version validation_error is compiler-owned -> terminal")
	}
}

func TestSubmitPlanSchema_ParsesAndPinsRequiredKeys(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(submitPlanSchema), &schema); err != nil {
		t.Fatalf("submitPlanSchema is not valid JSON: %v", err)
	}
	// Pin the model-facing required keys. CheckPlan remains authoritative for the
	// richer semantic constraints; this test does not pretend to be a JSON Schema
	// validator.
	req, _ := schema["required"].([]any)
	for _, k := range []string{"objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "steps"} {
		found := false
		for _, r := range req {
			if r == k {
				found = true
			}
		}
		if !found {
			t.Errorf("schema required missing %q", k)
		}
	}
}
