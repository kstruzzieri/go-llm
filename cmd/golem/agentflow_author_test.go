package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestSubmitPlanSchema_PinsIRStructs(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(submitPlanSchema), &schema); err != nil {
		t.Fatalf("submitPlanSchema is not valid JSON: %v", err)
	}

	// obj navigates a chain of object keys, failing the test if any hop is missing
	// or not an object.
	obj := func(m map[string]any, keys ...string) map[string]any {
		t.Helper()
		cur := m
		for _, k := range keys {
			next, ok := cur[k].(map[string]any)
			if !ok {
				t.Fatalf("schema path %v: %q is not an object", keys, k)
			}
			cur = next
		}
		return cur
	}
	planProps := obj(schema, "properties")
	stepItems := obj(schema, "properties", "steps", "items")
	stepProps := obj(stepItems, "properties")
	gateItems := obj(stepProps, "validations", "items")
	gateProps := obj(gateItems, "properties")

	// Every struct field's json tag (minus ,omitempty) must appear under the
	// matching properties object, so a tag rename or drop that would silently
	// mislead the model fails here rather than in production.
	checkFields := func(level string, rt reflect.Type, props map[string]any) {
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			if _, ok := props[name]; !ok {
				t.Errorf("%s: field %s (json %q) missing from schema properties", level, rt.Field(i).Name, name)
			}
		}
	}
	checkFields("PlanIR", reflect.TypeOf(agentflow.PlanIR{}), planProps)
	checkFields("StepIR", reflect.TypeOf(agentflow.StepIR{}), stepProps)
	checkFields("GateIR", reflect.TypeOf(agentflow.GateIR{}), gateProps)

	// Pin the required-key lists at all three levels. CheckPlan stays authoritative
	// for the richer semantic constraints; this only pins the model-facing contract.
	requires := func(m map[string]any, want ...string) {
		t.Helper()
		have := map[string]bool{}
		if req, ok := m["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					have[s] = true
				}
			}
		}
		for _, k := range want {
			if !have[k] {
				t.Errorf("required missing %q; got %v", k, m["required"])
			}
		}
	}
	requires(schema, "objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "steps")
	requires(stepItems, "id", "action", "files", "expected_diff", "validations")
	requires(gateItems, "argv")
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
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "add healthz", goalSet: true}, root, &stubLocker{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), filepath.Join(root, ".agent", "plan.lock.json")) ||
		!strings.Contains(out.String(), "review the locked plan") {
		t.Errorf("missing success output:\n%s", out.String())
	}
	// The execute-separately command must carry -root <absolute root> so #209 writes
	// proof state to the planning tree even when run from another directory.
	if !strings.Contains(out.String(), "-root "+root) {
		t.Errorf("execute-separately command missing -root %s:\n%s", root, out.String())
	}
}

func TestRunAgentflowAuthor_InterruptReturnsWithoutLock(t *testing.T) {
	root := t.TempDir()
	// A fired interrupt cancels the authoring loop before it can lock a plan. A
	// closed channel is deterministic: the watch goroutine cancels immediately, the
	// model never submits, and the flow falls to errPlannerNoSubmission.
	interrupts := make(chan struct{})
	close(interrupts)
	caller := &captureCaller{answer: "thinking, no submission"}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, interrupts, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{})
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("interrupted loop should return errPlannerNoSubmission, got %v", err)
	}
	if strings.Contains(out.String(), "locked plan") {
		t.Errorf("no plan may be locked after interrupt:\n%s", out.String())
	}
}

func TestRunAgentflowAuthor_RefusesLockedPlan(t *testing.T) {
	root := t.TempDir()
	writePlanLock(t, root, `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`)
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}); err == nil {
		t.Error("expected clobber-guard refusal for a locked plan")
	}
}

func TestRunAgentflowAuthor_ProbeFailsBeforeModel(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{probeErr: errors.New("missing")}
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client); err == nil {
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
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{})
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("err = %v, want no submission", err)
	}
	if !strings.Contains(caller.system, "Golem's planner") || !strings.Contains(caller.system, "repo rule") {
		t.Fatalf("planner system omitted prompt/context: %q", caller.system)
	}
}

func TestRunAgentflowAuthor_ExhaustionReportsDiagnostics(t *testing.T) {
	root := t.TempDir()
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "steps[0].files must be non-empty"}}}
	// Repairable on BOTH submissions: the flow uses its two-attempt budget, then
	// reports the last diagnostics and exits with errPlannerRejected.
	client := &stubLocker{lockErrs: []error{verr, verr}}
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t)), submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client)
	if !errors.Is(err, errPlannerRejected) {
		t.Fatalf("err = %v, want errPlannerRejected", err)
	}
	if client.locks != 2 {
		t.Fatalf("bounded repair should use both submissions; lock attempts = %d", client.locks)
	}
	if !strings.Contains(errb.String(), "validation_error") || !strings.Contains(errb.String(), "steps[0].files must be non-empty") {
		t.Fatalf("stderr missing rendered diagnostics:\n%s", errb.String())
	}
}

func TestRunAgentflowAuthor_TerminalLockErrorReturned(t *testing.T) {
	root := t.TempDir()
	term := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "invalid_arguments", Message: "boom"}}}
	// Terminal on the first submission: the flow must surface the terminal error,
	// not degrade to "no submission". Second error present only so a stray extra
	// lock (if the loop did not cancel) still cannot succeed.
	client := &stubLocker{lockErrs: []error{term, term}}
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t)), submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client)
	if errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("terminal lock error must not degrade to no-submission: %v", err)
	}
	var ce *agentflow.CommandError
	if !errors.As(err, &ce) || len(ce.Errors) == 0 || ce.Errors[0].Code != "invalid_arguments" {
		t.Fatalf("want terminal invalid_arguments CommandError, got %v", err)
	}
}
