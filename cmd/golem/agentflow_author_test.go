package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
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

func writePlanLock(t *testing.T, root string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".agent", "plan.lock.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGuardExistingPlan(t *testing.T) {
	// Absent lock: allowed.
	if err := guardExistingPlan(t.TempDir()); err != nil {
		t.Errorf("absent lock should be allowed: %v", err)
	}
	// Pristine unlocked scaffold (blank objective, no steps): allowed.
	r1 := t.TempDir()
	writePlanLock(t, r1, `{"schema_version":"0.3.0","objective":"","steps":[],"locked":false}`)
	if err := guardExistingPlan(r1); err != nil {
		t.Errorf("pristine scaffold should be allowed: %v", err)
	}
	// Locked plan: refused.
	r2 := t.TempDir()
	writePlanLock(t, r2, `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`)
	if err := guardExistingPlan(r2); err == nil {
		t.Error("locked plan must be refused")
	}
	// Non-empty unlocked draft: refused.
	r3 := t.TempDir()
	writePlanLock(t, r3, `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":false}`)
	if err := guardExistingPlan(r3); err == nil {
		t.Error("non-empty unlocked draft must be refused")
	}
	// Malformed: refused.
	r4 := t.TempDir()
	writePlanLock(t, r4, `{not json`)
	if err := guardExistingPlan(r4); err == nil {
		t.Error("malformed plan must be refused")
	}
	// Valid JSON is not enough: an incomplete object is not the known scaffold.
	r5 := t.TempDir()
	writePlanLock(t, r5, `{}`)
	if err := guardExistingPlan(r5); err == nil {
		t.Error("incomplete plan object must be refused")
	}
}

func submitPlanCall(args json.RawMessage) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "submit_plan", Arguments: args},
		}},
	}}
}

func TestRunAgentflowAuthor_HappyPathPrintsExecuteSeparately(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)

	var out, errb bytes.Buffer
	// runAgentflowAuthorWithClient injects a stub afLocker so no real CLI runs.
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, sess, flags{goal: "add healthz", goalSet: true}, root, &stubLocker{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), filepath.Join(root, ".agent", "plan.lock.json")) ||
		!strings.Contains(out.String(), "review the locked plan") {
		t.Errorf("missing success output:\n%s", out.String())
	}
}

func TestRunAgentflowAuthor_RefusesLockedPlan(t *testing.T) {
	root := t.TempDir()
	writePlanLock(t, root, `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`)
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}); err == nil {
		t.Error("expected clobber-guard refusal for a locked plan")
	}
}

func TestRunAgentflowAuthor_ProbeFailsBeforeModel(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{probeErr: errors.New("missing")}
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, sess, flags{goal: "x", goalSet: true}, root, client); err == nil {
		t.Fatal("expected probe failure")
	}
	if caller.i != 0 || client.inits != 0 {
		t.Fatalf("probe failure reached model/init: model=%d init=%d", caller.i, client.inits)
	}
}

func TestRunAgentflowAuthor_UsesPlannerPromptAndProjectContext(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "no submission"}
	sess := newTestSession(t, caller, root)
	sess.projectContextBlock = "<<<PROJECT_CONTEXT>>>\nrepo rule\n<<<END_PROJECT_CONTEXT>>>"
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{})
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("err = %v, want no submission", err)
	}
	if !strings.Contains(caller.system, "Golem's planner") || !strings.Contains(caller.system, "repo rule") {
		t.Fatalf("planner system omitted prompt/context: %q", caller.system)
	}
}
