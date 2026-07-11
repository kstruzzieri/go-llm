package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// stubLocker satisfies afLocker; LockPlan returns a scripted error per call.
type stubLocker struct {
	probeErr     error
	initErr      error
	lockErrs     []error
	probes       int
	inits        int
	locks        int
	paths        []string
	probeStarted chan struct{}
	lockStarted  chan struct{}
}

func (s *stubLocker) Probe(ctx context.Context) error {
	s.probes++
	if s.probeStarted != nil {
		close(s.probeStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	return s.probeErr
}
func (s *stubLocker) Init(context.Context) error { s.inits++; return s.initErr }
func (s *stubLocker) LockPlan(ctx context.Context, path string) error {
	s.paths = append(s.paths, path)
	i := s.locks
	s.locks++
	if s.lockStarted != nil {
		close(s.lockStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	if i < len(s.lockErrs) {
		return s.lockErrs[i]
	}
	return nil
}

func validIRJSON(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(validTraceableIR())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validTraceableIR() agentflow.PlanIR {
	return agentflow.PlanIR{
		Objective: "obj", Scope: []string{"src"}, Invariants: []string{"x"},
		RiskLevel: "low", RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Requirements: []agentflow.RequirementIR{{
			ID: "REQ-1", Text: "add the requested behavior",
			AcceptanceCriteria: []agentflow.CriterionIR{{ID: "AC-1", Text: "the focused validation passes"}},
		}},
		Steps: []agentflow.StepIR{{
			ID: "S1", Action: "do", Files: []string{"src/a.go"}, ExpectedDiff: []string{"x"},
			CriterionIDs: []string{"AC-1"},
			Validations:  []agentflow.GateIR{{Argv: []string{"true"}, CriterionIDs: []string{"AC-1"}}},
		}},
	}
}

func TestRenderPlanPreview_DeterministicAndTraceable(t *testing.T) {
	ir := validTraceableIR()
	ir.NonGoals = []string{"no dependency changes"}
	want := "Plan preview\n\n" +
		"Objective\n  \"obj\"\n\n" +
		"Scope\n  - \"src\"\n\n" +
		"Risk\n  \"low\"\n\n" +
		"Non-goals\n  - \"no dependency changes\"\n\n" +
		"Invariants\n  - \"x\"\n\n" +
		"Allowed files\n  - \"src/*\"\n  - \".agent/\"\n\n" +
		"Blocked files\n  - none\n\n" +
		"Rollback\n  \"git checkout -- .\"\n\n" +
		"Requirements\n  \"REQ-1\": \"add the requested behavior\"\n    \"AC-1\": \"the focused validation passes\"\n\n" +
		"Steps\n  \"S1\": \"do\"\n    files: [\"src/a.go\"]\n    depends_on: none\n    criteria: [\"AC-1\"]\n    expected_diff: [\"x\"]\n    validation:\n      - \"true\"\n        argv: [\"true\"]\n        criteria: [\"AC-1\"]\n"

	first := renderPlanPreview(agentflow.Compile(ir))
	second := renderPlanPreview(agentflow.Compile(ir))
	if first != want || second != want {
		t.Fatalf("preview mismatch:\n--- got ---\n%s--- want ---\n%s", first, want)
	}
}

func TestRenderPlanPreview_DistinguishesLockedValues(t *testing.T) {
	a := validTraceableIR()
	a.Steps[0].ExpectedDiff = []string{"a, b", "c"}
	b := validTraceableIR()
	b.Steps[0].ExpectedDiff = []string{"a", "b", "c"}
	if renderPlanPreview(agentflow.Compile(a)) == renderPlanPreview(agentflow.Compile(b)) {
		t.Fatal("different expected_diff arrays produced the same approval preview")
	}

	a.Objective = "a\nb"
	b.Objective = `"a\nb"`
	if renderPlanPreview(agentflow.Compile(a)) == renderPlanPreview(agentflow.Compile(b)) {
		t.Fatal("newline and literal escape text produced the same approval preview")
	}
}

func TestRenderPlanPreview_EscapesControlCharacters(t *testing.T) {
	ir := validTraceableIR()
	ir.Objective = "visible\nRequirements\nforged\x1b[2J"
	ir.Steps[0].Action = "do\rspoof"
	got := renderPlanPreview(agentflow.Compile(ir))
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\r') {
		t.Fatalf("preview contains raw terminal controls: %q", got)
	}
	if strings.Count(got, "\nRequirements\n") != 1 || !strings.Contains(got, `\nRequirements\nforged\x1b`) {
		t.Fatalf("preview allowed section spoofing: %q", got)
	}
}

func TestSubmitPlanTool_Effect(t *testing.T) {
	as := &authorSession{root: "/root"}
	tool := newSubmitPlanTool(as)
	e := tool.Effect()
	if e.Class != agent.Write|agent.Exec {
		t.Errorf("effect class = %v, want Write|Exec", e.Class)
	}
	if e.Approval != agent.ApprovalAlways {
		t.Errorf("approval = %v, want ApprovalAlways", e.Approval)
	}
	if e.Scope.CWD != "/root" {
		t.Errorf("effect cwd = %q, want /root", e.Scope.CWD)
	}
}

func TestSubmitPlanTool_PreviewsWithoutMutatingProofState(t *testing.T) {
	client := &stubLocker{}
	tool := newSubmitPlanTool(&authorSession{client: client, root: "/root"})
	plan, err := tool.Plan(context.Background(), validIRJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effect.Approval != agent.ApprovalAlways || !strings.Contains(plan.Preview, "AC-1") || !strings.Contains(plan.Preview, `argv: ["true"]`) {
		t.Fatalf("tool plan = %+v", plan)
	}
	if client.inits != 0 || client.locks != 0 {
		t.Fatalf("preview mutated proof state: init=%d lock=%d", client.inits, client.locks)
	}
}

type fixedApprover bool

func (a fixedApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return bool(a), nil
}

type sequenceApprover struct {
	decisions []bool
}

func (a *sequenceApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	decision := a.decisions[0]
	a.decisions = a.decisions[1:]
	return decision, nil
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

func TestSubmitPlanTool_RejectsRequirementFreeAuthoring(t *testing.T) {
	ir := validTraceableIR()
	ir.Requirements = nil
	ir.Steps[0].CriterionIDs = nil
	ir.Steps[0].Validations[0].CriterionIDs = nil
	args, err := json.Marshal(ir)
	if err != nil {
		t.Fatal(err)
	}
	client := &stubLocker{}
	as := &authorSession{client: client, root: "/root", cancel: func() {}}
	res, err := newSubmitPlanTool(as).Invoke(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "missing_requirements") {
		t.Fatalf("requirement-free submission = %+v", res)
	}
	if client.inits != 0 || client.locks != 0 {
		t.Fatalf("requirement-free authoring mutated Agentflow: init=%d lock=%d", client.inits, client.locks)
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
	requirementItems := obj(schema, "properties", "requirements", "items")
	requirementProps := obj(requirementItems, "properties")
	criterionItems := obj(requirementProps, "acceptance_criteria", "items")
	criterionProps := obj(criterionItems, "properties")
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
	checkFields("RequirementIR", reflect.TypeOf(agentflow.RequirementIR{}), requirementProps)
	checkFields("CriterionIR", reflect.TypeOf(agentflow.CriterionIR{}), criterionProps)
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
	requires(schema, "objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "requirements", "steps")
	requires(requirementItems, "id", "text", "acceptance_criteria")
	requires(criterionItems, "id", "text")
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
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "absent"},
		{name: "pristine", body: `{"schema_version":"0.3.0","objective":"","steps":[],"locked":false}`},
		{name: "locked", body: `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`, wantErr: true},
		{name: "draft", body: `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":false}`, wantErr: true},
		{name: "malformed", body: `{not json`, wantErr: true},
		{name: "incomplete", body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.body != "" {
				writePlanLock(t, root, tc.body)
			}
			if err := guardExistingPlan(root); (err != nil) != tc.wantErr {
				t.Fatalf("guardExistingPlan() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
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

type repairFailureCaller struct {
	first agent.ModelResult
	err   error
	calls int
}

func (c *repairFailureCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls++
	if c.calls > 1 {
		return agent.ModelResult{}, c.err
	}
	if onToken != nil {
		_ = onToken(c.first.Response)
	}
	return c.first, nil
}

func TestRunAgentflowAuthor_HappyPathPrintsExecuteSeparately(t *testing.T) {
	root := filepath.Join(t.TempDir(), "space and 'quote")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}

	var out, errb bytes.Buffer
	// runAgentflowAuthorWithClient injects a stub afLocker so no real CLI runs.
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "add healthz", goalSet: true}, root, client, fixedApprover(true))
	if err != nil {
		t.Fatal(err)
	}
	if client.inits != 1 || client.locks != 1 {
		t.Fatalf("approved plan should initialize and lock once: init=%d lock=%d", client.inits, client.locks)
	}
	if !strings.Contains(out.String(), filepath.Join(root, ".agent", "plan.lock.json")) ||
		!strings.Contains(out.String(), "locked approved plan") {
		t.Errorf("missing success output:\n%s", out.String())
	}
	// The execute-separately command must carry -root <absolute root> so #209 writes
	// proof state to the planning tree even when run from another directory.
	if !strings.Contains(out.String(), "-plan "+shellQuote(filepath.Join(root, ".agent", "plan.lock.json"))+" -root "+shellQuote(root)) {
		t.Errorf("execute-separately command missing -root %s:\n%s", root, out.String())
	}
}

func TestRunAgentflowAuthor_RefusesLockWithoutExplicitApproval(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}

	err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, client, nil)
	if !errors.Is(err, errPlannerApprovalDenied) {
		t.Fatalf("err = %v, want approval denied", err)
	}
	if client.inits != 0 || client.locks != 0 {
		t.Fatalf("proof state mutated before approval: init=%d lock=%d", client.inits, client.locks)
	}
}

func TestRunAgentflowAuthor_DenialAfterRepairDoesNotClaimNoMutation(t *testing.T) {
	root := t.TempDir()
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "repair me"}}}
	client := &stubLocker{lockErrs: []error{verr}}
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t)), submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	approver := &sequenceApprover{decisions: []bool{true, false}}

	err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, client, approver)
	if !errors.Is(err, errPlannerApprovalDenied) || strings.Contains(err.Error(), "no Agentflow state was changed") {
		t.Fatalf("denial after initialized repair returned misleading error: %v", err)
	}
	if client.inits != 1 || client.locks != 1 {
		t.Fatalf("repair denial state = init %d lock %d, want 1/1", client.inits, client.locks)
	}
}

func TestAcquireAuthorLockSerializesRoot(t *testing.T) {
	root := t.TempDir()
	release, err := acquireAuthorLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireAuthorLock(waitCtx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err = %v, want context deadline", err)
	}
}

func TestRunAgentflowAuthor_InstallsProofStateReadGuard(t *testing.T) {
	// The planner's read tools must be scoped so the model cannot read .agent/
	// proof state. Drive a model that first reads .agent/plan.lock.json (denied
	// by the installed denyProofState guard, surfaced to stderr) then submits.
	root := t.TempDir()
	readAgent := agent.ModelResult{Response: provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "r1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":".agent/plan.lock.json"}`)},
		}},
	}}
	caller := &scriptCaller{responses: []agent.ModelResult{readAgent, submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)

	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, fixedApprover(true)); err != nil {
		t.Fatal(err)
	}
	// The .agent read must have been denied (guard wired), not served.
	if !strings.Contains(errb.String(), "proof state") {
		t.Errorf("planner did not deny the .agent read; guard not wired?\nstderr:\n%s", errb.String())
	}
	// The flow still locks after the denied read + valid submission.
	if !strings.Contains(out.String(), "locked approved plan") {
		t.Errorf("flow should still lock after a denied read:\n%s", out.String())
	}
}

func TestRunAgentflowAuthor_InterruptReturnsWithoutLock(t *testing.T) {
	root := t.TempDir()
	// A fired interrupt must cancel the in-flight model call. The caller blocks on
	// ctx (block channel never closed), so the only way Chat returns is the watch
	// goroutine cancelling loopCtx off the closed interrupts channel -> Chat returns
	// context.Canceled -> the flow reports errPlannerInterrupted (distinct from
	// errPlannerNoSubmission, which is the loop reaching its step cap without a submit).
	interrupts := make(chan struct{})
	close(interrupts)
	caller := &scriptCaller{block: make(chan struct{})}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, interrupts, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
	if !errors.Is(err, errPlannerInterrupted) {
		t.Fatalf("interrupted loop should return errPlannerInterrupted, got %v", err)
	}
	if strings.Contains(out.String(), "locked plan") {
		t.Errorf("no plan may be locked after interrupt:\n%s", out.String())
	}
}

func TestRunAgentflowAuthor_InterruptCancelsProbe(t *testing.T) {
	root := t.TempDir()
	interrupts := make(chan struct{}, 1)
	client := &stubLocker{probeStarted: make(chan struct{})}
	sess := newTestSession(t, &scriptCaller{}, root)
	done := make(chan error, 1)
	go func() {
		done <- runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, interrupts, sess, flags{goal: "x", goalSet: true}, root, client, fixedApprover(true))
	}()
	<-client.probeStarted
	interrupts <- struct{}{}
	if err := <-done; !errors.Is(err, errPlannerInterrupted) {
		t.Fatalf("probe interrupt err = %v, want errPlannerInterrupted", err)
	}
	if client.inits != 0 {
		t.Fatal("Init must not run after an interrupted Probe")
	}
}

func TestRunAgentflowAuthor_InterruptCancelsLockPlan(t *testing.T) {
	root := t.TempDir()
	interrupts := make(chan struct{}, 1)
	client := &stubLocker{lockStarted: make(chan struct{})}
	sess := newTestSession(t, &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}, root)
	done := make(chan error, 1)
	go func() {
		done <- runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, interrupts, sess, flags{goal: "x", goalSet: true}, root, client, fixedApprover(true))
	}()
	<-client.lockStarted
	interrupts <- struct{}{}
	if err := <-done; !errors.Is(err, errPlannerInterrupted) {
		t.Fatalf("lock interrupt err = %v, want errPlannerInterrupted", err)
	}
}

func TestRunAgentflowAuthor_RefusesLockedPlan(t *testing.T) {
	root := t.TempDir()
	writePlanLock(t, root, `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`)
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil); err == nil {
		t.Error("expected clobber-guard refusal for a locked plan")
	}
}

func TestRunAgentflowAuthor_ProbeFailsBeforeModel(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{probeErr: errors.New("missing")}
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client, nil); err == nil {
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
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("err = %v, want no submission", err)
	}
	if !strings.Contains(caller.system, "Golem's planner") || !strings.Contains(caller.system, "repo rule") ||
		!strings.Contains(caller.system, "acceptance criterion") || !strings.Contains(caller.system, "criterion_ids") {
		t.Fatalf("planner system omitted prompt/context: %q", caller.system)
	}
}

type optionsCaptureCaller struct{ options provider.ModelOptions }

func (c *optionsCaptureCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.options = req.Options
	return agent.ModelResult{Response: provider.ChatResponse{Content: "no submission"}}, nil
}

func TestRunAgentflowAuthor_UsesPlannerModelOptionsWithoutMutatingSession(t *testing.T) {
	on := true
	for _, tt := range []struct {
		name       string
		options    provider.ModelOptions
		budget     agent.Budget
		wantOutput int
	}{
		{name: "default budget", wantOutput: minPlannerOutput},
		{name: "lower budget and explicit thinking", options: provider.ModelOptions{NumPredict: 1024, Think: &on, ThinkEffort: "high"}, wantOutput: minPlannerOutput},
		{name: "larger caller budget", options: provider.ModelOptions{NumPredict: 8192, Think: &on, ThinkEffort: "high"}, wantOutput: 8192},
		// Budget.OutputReserve overrides Options.NumPredict inside the agent
		// layer, so the floor must survive it too (Codex review on PR #295).
		{name: "small output reserve floored", budget: agent.Budget{OutputReserve: 1024}, wantOutput: minPlannerOutput},
		{name: "large output reserve preserved", budget: agent.Budget{InputCeiling: 32768, OutputReserve: 8192}, wantOutput: 8192},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			caller := &optionsCaptureCaller{}
			sess := newTestSession(t, caller, root)
			sess.modelOptions = tt.options
			sess.budget = tt.budget
			before, err := json.Marshal(sess.modelOptions)
			if err != nil {
				t.Fatal(err)
			}

			err = runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
			if !errors.Is(err, errPlannerNoSubmission) {
				t.Fatalf("err = %v, want no submission", err)
			}
			if caller.options.NumPredict != tt.wantOutput || caller.options.Think == nil || *caller.options.Think || caller.options.ThinkEffort != "" {
				t.Errorf("planner options = %+v, want output %d with thinking disabled", caller.options, tt.wantOutput)
			}
			after, err := json.Marshal(sess.modelOptions)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Errorf("session options mutated: before=%s after=%s", before, after)
			}
			if sess.budget != tt.budget {
				t.Errorf("session budget mutated: before=%+v after=%+v", tt.budget, sess.budget)
			}
		})
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
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client, fixedApprover(true))
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

func TestRunAgentflowAuthor_RepairModelFailureOverridesStaleDiagnostics(t *testing.T) {
	root := t.TempDir()
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "repair me"}}}
	providerErr := errors.New("provider unavailable during repair")
	caller := &repairFailureCaller{first: submitPlanCall(validIRJSON(t)), err: providerErr}
	sess := newTestSession(t, caller, root)

	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{lockErrs: []error{verr}}, fixedApprover(true))
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want provider error", err)
	}
	if errors.Is(err, errPlannerRejected) {
		t.Fatalf("provider error must not be masked as exhausted submissions: %v", err)
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
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, client, fixedApprover(true))
	if errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("terminal lock error must not degrade to no-submission: %v", err)
	}
	var ce *agentflow.CommandError
	if !errors.As(err, &ce) || len(ce.Errors) == 0 || ce.Errors[0].Code != "invalid_arguments" {
		t.Fatalf("want terminal invalid_arguments CommandError, got %v", err)
	}
}
