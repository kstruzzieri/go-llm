package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeAF records the ordered sequence of driver->agentflow calls.
type fakeAF struct {
	seq                []string
	nextSteps          []string // ids to hand out, then "" (done)
	i                  int
	review             agentflow.ReviewRun
	failAt             map[string]error
	proofError         error
	recommendation     agentflow.WorkflowRecommendation
	briefs             []agentflow.TaskBrief
	selectedProfiles   []string
	reasons            []string
	materializedRoutes []string
	gateArgv           [][]string
	nextActions        []agentflow.NextActionState
	nextActionIndex    int
	blocks             []blockCall
}

// blockCall snapshots a block-step context at call time: blockAttempt cancels
// that context once the call returns (#642 A3).
type blockCall struct {
	remaining   time.Duration
	hasDeadline bool
	err         error
}

func newBlockCall(ctx context.Context) blockCall {
	c := blockCall{err: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		c.remaining, c.hasDeadline = time.Until(deadline), true
	}
	return c
}

// assertFreshBlockContext checks that the one block-step ran on a live context
// bounded by the 30-second budget, not on the caller's context (#642 A3).
func assertFreshBlockContext(t *testing.T, blocks []blockCall) {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("block-step calls = %d, want 1", len(blocks))
	}
	// remaining is measured microseconds after the deadline is set, so the
	// 20-second floor leaves 10 seconds of slack and still rejects a short budget.
	if b := blocks[0]; !b.hasDeadline || b.err != nil || b.remaining <= 20*time.Second || b.remaining > 30*time.Second {
		t.Fatalf("block-step context = %+v, want a live context with a deadline in (20s, 30s]", b)
	}
}

func (f *fakeAF) failure(name string) error {
	if f.failAt == nil {
		return nil
	}
	return f.failAt[name]
}

func (f *fakeAF) Probe(context.Context) error {
	f.seq = append(f.seq, "probe")
	return f.failure("probe")
}
func (f *fakeAF) ProbeParallel(context.Context) error {
	f.seq = append(f.seq, "probe-parallel")
	return f.failure("probe-parallel")
}
func (f *fakeAF) ProbeWorkflow(context.Context) error {
	f.seq = append(f.seq, "probe-workflow")
	return f.failure("probe-workflow")
}
func (f *fakeAF) RecommendWorkflow(_ context.Context, brief agentflow.TaskBrief, selectedProfile, reason string) (agentflow.WorkflowRecommendation, error) {
	f.seq = append(f.seq, "recommend")
	f.briefs = append(f.briefs, brief)
	f.selectedProfiles = append(f.selectedProfiles, selectedProfile)
	f.reasons = append(f.reasons, reason)
	if err := f.failure("recommend"); err != nil {
		return agentflow.WorkflowRecommendation{}, err
	}
	if f.recommendation.SchemaVersion != "" {
		return f.recommendation, nil
	}
	return defaultWorkflowRecommendation(), nil
}
func (f *fakeAF) ProbeReview(context.Context) error {
	f.seq = append(f.seq, "probe-review")
	return f.failure("probe-review")
}
func (f *fakeAF) Init(context.Context) error { f.seq = append(f.seq, "init"); return nil }
func (f *fakeAF) LockPlan(_ context.Context, p string) error {
	f.seq = append(f.seq, "lock:"+p)
	return nil
}
func (f *fakeAF) MaterializeWorkflowContract(_ context.Context, recommendation agentflow.WorkflowRecommendation) error {
	f.seq = append(f.seq, "materialize")
	f.materializedRoutes = append(f.materializedRoutes, recommendation.Selected.Profile)
	return f.failure("materialize")
}
func (f *fakeAF) InitExecution(context.Context) error { f.seq = append(f.seq, "init-exec"); return nil }
func (f *fakeAF) Doctor(context.Context) error        { f.seq = append(f.seq, "doctor"); return nil }
func (f *fakeAF) NextStep(context.Context) (string, error) {
	f.seq = append(f.seq, "next-step")
	if f.i < len(f.nextSteps) {
		s := f.nextSteps[f.i]
		f.i++
		return s, nil
	}
	return "", nil
}
func (f *fakeAF) ClaimStep(_ context.Context, id string) (string, error) {
	f.seq = append(f.seq, "claim:"+id)
	return "A-" + id, nil
}
func (f *fakeAF) RecordReview(_ context.Context, path string) (agentflow.ReviewRun, error) {
	f.seq = append(f.seq, "record-review:"+path)
	return f.review, f.failure("record-review")
}
func (f *fakeAF) AmendStep(_ context.Context, id string, refs []string) (string, error) {
	f.seq = append(f.seq, "amend:"+id+":"+strings.Join(refs, ","))
	return "AM-" + id, f.failure("amend-step")
}
func (f *fakeAF) RunGate(_ context.Context, step, attempt, gate string, argv []string) error {
	f.seq = append(f.seq, "gate:"+step+":"+gate)
	f.gateArgv = append(f.gateArgv, append([]string(nil), argv...))
	return f.failure("gate")
}
func (f *fakeAF) FinishStep(_ context.Context, id, attempt string) error {
	f.seq = append(f.seq, "finish-step:"+id+":"+attempt)
	return f.failure("finish-step")
}
func (f *fakeAF) CompleteStep(_ context.Context, id, attempt string) error {
	f.seq = append(f.seq, "complete-step:"+id+":"+attempt)
	return f.failure("complete-step")
}
func (f *fakeAF) BlockStep(ctx context.Context, id, attempt, reason string) error {
	f.seq = append(f.seq, "block-step:"+id+":"+attempt+":"+reason)
	f.blocks = append(f.blocks, newBlockCall(ctx))
	return f.failure("block-step")
}
func (f *fakeAF) FinishRun(context.Context) (string, error) {
	f.seq = append(f.seq, "finish-run")
	if f.proofError != nil {
		return "", f.proofError
	}
	return "proof-pack.md", nil
}
func (f *fakeAF) RecordFileChange(_ context.Context, step, attempt, path string) error {
	f.seq = append(f.seq, "record-file-change:"+step+":"+attempt+":"+path)
	return f.failure("record-file-change")
}
func (f *fakeAF) RecordEvidence(_ context.Context, e agentflow.EvidenceEntry) error {
	f.seq = append(f.seq, "evidence:"+e.ID)
	return nil
}
func (f *fakeAF) NextAction(context.Context) (agentflow.NextActionState, error) {
	f.seq = append(f.seq, "next-action")
	if f.nextActionIndex < len(f.nextActions) {
		state := f.nextActions[f.nextActionIndex]
		f.nextActionIndex++
		return state, f.failure("next-action")
	}
	// Unscripted calls report the run phase, so d.run's pre-finish-run guard
	// (#651) passes in tests that do not exercise it.
	return agentflow.NextActionState{State: "run_unverified"}, nil
}

func marshalPlanJSON(t *testing.T, plan agentflow.Plan) []byte {
	t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDriver_HappyPathOrdering(t *testing.T) {
	af := &fakeAF{nextSteps: []string{"P1"}}
	plan := &agentflow.Plan{Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"src/a.go"},
		Validation: []string{"go test"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test"}}},
	}}}
	plan.AllowedFiles = []string{"src/*"}

	// runStep is scripted to "succeed" without a real model.
	runStep := func(ctx context.Context, step agentflow.Step, attempt, goal string) error {
		return nil
	}

	d := &driver{
		af: af, plan: plan, planPath: "plan.json", runStep: runStep,
		evidence: []agentflow.EvidenceEntry{{ID: "E1", Claim: "fixture", Source: "evidence.json"}},
	}
	proof, err := d.run(context.Background())
	if err != nil || proof != "proof-pack.md" {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
	want := []string{
		"probe", "probe-workflow", "recommend", "init", "evidence:E1", "lock:plan.json", "materialize", "init-exec", "doctor",
		"next-step", "claim:P1", "gate:P1:go test", "finish-step:P1:A-P1",
		"next-step", "next-action", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
	}
}

func TestDriver_RunOneStepValidatesGoalBeforeClaim(t *testing.T) {
	af := &fakeAF{}
	d := &driver{
		af: af,
		plan: &agentflow.Plan{
			Requirements: []agentflow.Requirement{{ID: "REQ-1"}},
			Steps:        []agentflow.Step{{ID: "P1", Gates: []agentflow.Gate{{Kind: "command"}}}},
		},
	}
	if err := d.runOneStep(context.Background(), "P1"); err == nil {
		t.Fatal("runOneStep() unexpectedly succeeded")
	}
	if len(af.seq) != 0 {
		t.Fatalf("claimed before pure goal validation: %v", af.seq)
	}
}

func TestTaskStepRunnerUsesItsRootAndAgentflowJournal(t *testing.T) {
	root := t.TempDir()
	plan := &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"out.txt"},
	}}}
	write := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "write-1", Type: "function",
		Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"worker\n"}`)},
	}}}
	orch := agent.New(&scriptCaller{responses: []agent.ModelResult{
		{Response: write},
		{Response: provider.ChatResponse{Content: "done"}},
	}}, agent.ContextManager{})
	af := &fakeAF{}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, af, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: af, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(got) != "worker\n" {
		t.Fatalf("root output = %q, %v", got, err)
	}
	want := []string{
		"claim:P1",
		"record-file-change:P1:A-P1:out.txt",
		"finish-step:P1:A-P1",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
	}
}

func TestDriver_ParallelCohortRunsBeforeSerialWork(t *testing.T) {
	af := &fakeAF{nextSteps: []string{"P1"}}
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
		parallelCohort: func(context.Context) error {
			af.seq = append(af.seq, "parallel-cohort")
			return nil
		},
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	if _, err := d.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"probe", "probe-parallel", "probe-workflow", "recommend", "init", "lock:plan.json", "materialize", "init-exec", "doctor", "parallel-cohort",
		"next-step", "claim:P1", "gate:P1:test one", "finish-step:P1:A-P1", "next-step", "next-action", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
	}
}

func TestDriver_ParallelCohortFailuresAbortBeforeSerialWork(t *testing.T) {
	for _, tt := range []struct {
		name    string
		af      *fakeAF
		cohort  func(context.Context) error
		wantSeq []string
	}{
		{
			name:    "parallel probe",
			af:      &fakeAF{failAt: map[string]error{"probe-parallel": errors.New("unavailable")}},
			cohort:  func(context.Context) error { return nil },
			wantSeq: []string{"probe", "probe-parallel"},
		},
		{
			name: "cohort",
			af:   &fakeAF{},
			cohort: func(context.Context) error {
				return errors.New("cohort failed")
			},
			wantSeq: []string{"probe", "probe-parallel", "probe-workflow", "recommend", "probe-review", "init", "lock:plan.json", "materialize", "init-exec", "doctor"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &driver{
				af: tt.af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
				parallelCohort: tt.cohort,
				runStep:        func(context.Context, agentflow.Step, string, string) error { return nil },
			}
			if _, err := d.run(context.Background()); err == nil {
				t.Fatal("expected error")
			}
			if !equalSeq(tt.af.seq, tt.wantSeq) {
				t.Fatalf("seq = %v, want %v", tt.af.seq, tt.wantSeq)
			}
		})
	}
}

// #651: next-step also returns nothing when the remaining step holds an open
// attempt, so the serial loop ending is not proof the work is done.
func TestDriver_RefusesFinishRunWhileStepWorkRemains(t *testing.T) {
	stepID := "P1"
	af := &fakeAF{nextActions: []agentflow.NextActionState{{State: "validation_missing", StepID: &stepID}}}
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	proof, err := d.run(context.Background())
	if err == nil || proof != "" {
		t.Fatalf("proof=%q err=%v, want a refusal", proof, err)
	}
	for _, want := range []string{`"validation_missing"`, `"P1"`, "run -agentflow-status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
	wantTail := []string{"doctor", "next-step", "next-action"}
	if len(af.seq) < len(wantTail) || !equalSeq(af.seq[len(af.seq)-len(wantTail):], wantTail) {
		t.Fatalf("seq = %v, want it to end %v with no finish-run", af.seq, wantTail)
	}
}

func TestDriver_FinishRunGateByNextActionState(t *testing.T) {
	// Resume's verdict needs its owned projection, so the refusal never
	// predicts it: every refusal sends the user to status, which runs resume's
	// state and projection checks and reports whether resume can recover.
	const hint = "; run -agentflow-status to see whether -agentflow-resume can recover it"
	for _, tt := range []struct {
		state   string
		proceed bool
	}{
		{"run_unverified", true},
		{"drift_failing", true},
		{"proof_missing", true},
		{"proof_stale", true},
		{"complete", true},
		{"step_unclaimed", false},
		{"file_receipts_missing", false},
		{"validation_missing", false},
		{"step_unverified", false},
		{"step_uncompleted", false},
		{"state_invalid", false},
		{"uninitialized", false},
		{"plan_unlocked", false},
		{"execution_uninitialized", false},
		{"", false},
		{"some_future_state", false},
	} {
		t.Run("state="+tt.state, func(t *testing.T) {
			af := &fakeAF{nextActions: []agentflow.NextActionState{{State: tt.state}}}
			d := &driver{
				af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
				runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
			}
			proof, err := d.run(context.Background())
			ranFinish := len(af.seq) > 0 && af.seq[len(af.seq)-1] == "finish-run"
			if tt.proceed {
				if err != nil || proof != "proof-pack.md" || !ranFinish {
					t.Fatalf("proof=%q err=%v seq=%v, want finish-run to run", proof, err, af.seq)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "still reports step work") || !strings.HasSuffix(err.Error(), hint) ||
				proof != "" || af.seq[len(af.seq)-1] != "next-action" || slices.Contains(af.seq, "finish-run") {
				t.Fatalf("proof=%q err=%v seq=%v, want the guard's refusal after next-action ending %q", proof, err, af.seq, hint)
			}
		})
	}
}

// #689: Golem's own ledger writes leave an older proof proof_stale, so a
// failing proof at the pre-finish-run check is one Golem cannot explain
// (malformed, newer schema). finish-run would overwrite it; refuse instead.
func TestDriver_FinishRunPreservesFailingProof(t *testing.T) {
	af := &fakeAF{nextActions: []agentflow.NextActionState{{State: "proof_failing"}}}
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	proof, err := d.run(context.Background())
	const want = "agentflow reports a failing proof before finish-run; finish-run would replace it, so no proof was built; inspect .agent/proof-pack.json, move it out of the workspace, then run -agentflow-status to see whether -agentflow-resume can build a new proof"
	if err == nil || err.Error() != want || proof != "" {
		t.Fatalf("proof=%q err=%v\nwant %s", proof, err, want)
	}
	if slices.Contains(af.seq, "finish-run") || af.seq[len(af.seq)-1] != "next-action" {
		t.Fatalf("seq = %v, want it to stop at the pre-finish-run next-action", af.seq)
	}
}

func TestDriver_NextActionErrorBlocksFinishRun(t *testing.T) {
	sentinel := errors.New("next-action unavailable")
	// The fake returns failAt's error only with a scripted state, so script a
	// state the guard would otherwise accept: the error must still win.
	af := &fakeAF{
		nextActions: []agentflow.NextActionState{{State: "run_unverified"}},
		failAt:      map[string]error{"next-action": sentinel},
	}
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	proof, err := d.run(context.Background())
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "before finish-run") || proof != "" || slices.Contains(af.seq, "finish-run") {
		t.Fatalf("proof=%q err=%v seq=%v, want the wrapped next-action error and no finish-run", proof, err, af.seq)
	}
}

func TestDriver_PreapprovedWorkflowIsReusedWithoutRecommendationOrMaterialization(t *testing.T) {
	af := &fakeAF{}
	recommendation := defaultWorkflowRecommendation()
	d := &driver{
		af: af, plan: &agentflow.Plan{}, planPath: "plan.json", out: io.Discard,
		approvedRecommendation: &recommendation,
		runStep:                func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	if _, err := d.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"probe", "probe-workflow", "init", "lock:plan.json", "init-exec", "doctor", "next-step", "next-action", "finish-run"}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq = %v, want %v", af.seq, want)
	}
	if len(af.briefs) != 0 || len(af.materializedRoutes) != 0 {
		t.Fatalf("preapproved route was recomputed/materialized: briefs=%v routes=%v", af.briefs, af.materializedRoutes)
	}
}

func TestReadExternalTaskBrief_ConservativeFallbackUsesOnlyExactPlanFacts(t *testing.T) {
	plan := agentflow.Plan{
		RiskLevel:       "low",
		ValidationGates: []string{"unit", "integration"},
		Steps: []agentflow.Step{
			{Files: []string{"src/a.go", "src/shared.go"}},
			{Files: []string{"src/shared.go", "src/b.go"}},
		},
	}
	brief, err := readExternalTaskBrief("", plan)
	if err != nil {
		t.Fatal(err)
	}
	if brief.SchemaVersion != agentflow.TaskBriefSchemaVersion || brief.TaskType != "feature" || brief.DeclaredRisk != "low" {
		t.Fatalf("fallback identity = %+v", brief)
	}
	if brief.CandidateFiles == nil || strings.Join(*brief.CandidateFiles, ",") != "src/a.go,src/shared.go,src/b.go" {
		t.Fatalf("fallback candidate files = %v", brief.CandidateFiles)
	}
	if brief.ValidationNeeds == nil || strings.Join(*brief.ValidationNeeds, ",") != "unit,integration" {
		t.Fatalf("fallback validation needs = %v", brief.ValidationNeeds)
	}
	if brief.SecuritySensitive != nil || brief.BlastRadius != nil || brief.DeclaredSize != nil {
		t.Fatalf("fallback guessed optional signals: %+v", brief)
	}
}

func TestReadExternalTaskBrief_StrictExplicitInputAndExactFactFill(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	plan := agentflow.Plan{
		RiskLevel: "high", ValidationGates: []string{"unit"},
		Steps: []agentflow.Step{{Files: []string{"src/a.go"}}},
	}
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(cwd, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}

	path := write("brief.json", `{"schema_version":"0.1.0","task_type":"refactor","declared_risk":"high","security_sensitive":false,"candidate_files":["external/context.go"],"blast_radius":"local","validation_needs":["lint"],"declared_size":"s"}`)
	brief, err := readExternalTaskBrief(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	if brief.TaskType != "refactor" || brief.SecuritySensitive == nil || *brief.SecuritySensitive ||
		brief.BlastRadius == nil || *brief.BlastRadius != "local" || brief.DeclaredSize == nil || *brief.DeclaredSize != "s" {
		t.Fatalf("explicit fields not preserved: %+v", brief)
	}
	if brief.CandidateFiles == nil || strings.Join(*brief.CandidateFiles, ",") != "src/a.go,external/context.go" ||
		brief.ValidationNeeds == nil || strings.Join(*brief.ValidationNeeds, ",") != "unit,lint" {
		t.Fatalf("exact plan facts were not conservatively unioned: %+v", brief)
	}

	emptyPath := write("explicit-empty.json", `{"schema_version":"0.1.0","task_type":"feature","declared_risk":"high","candidate_files":[],"validation_needs":[]}`)
	empty, err := readExternalTaskBrief(emptyPath, plan)
	if err != nil {
		t.Fatal(err)
	}
	if empty.CandidateFiles == nil || strings.Join(*empty.CandidateFiles, ",") != "src/a.go" ||
		empty.ValidationNeeds == nil || strings.Join(*empty.ValidationNeeds, ",") != "unit" {
		t.Fatalf("explicit empty fields concealed exact plan facts: %+v", empty)
	}

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"unknown field", `{"schema_version":"0.1.0","task_type":"feature","declared_risk":"high","future":true}`, "unknown field"},
		{"trailing json", `{"schema_version":"0.1.0","task_type":"feature","declared_risk":"high"}{}`, "trailing"},
		{"wrong version", `{"schema_version":"9.0.0","task_type":"feature","declared_risk":"high"}`, "schema_version"},
		{"missing task type", `{"schema_version":"0.1.0","declared_risk":"high"}`, "task_type"},
		{"risk mismatch", `{"schema_version":"0.1.0","task_type":"feature","declared_risk":"low"}`, "does not match plan risk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readExternalTaskBrief(write(strings.ReplaceAll(tc.name, " ", "-")+".json", tc.body), plan); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadExternalTaskBrief_CannotHideExactPlanScope(t *testing.T) {
	files := []string{
		"src/f01.go", "src/f02.go", "src/f03.go", "src/f04.go", "src/f05.go",
		"src/f06.go", "src/f07.go", "src/f08.go", "src/f09.go", "src/f10.go",
		"src/f11.go", "src/f12.go", "src/f13.go", "src/f14.go", "src/f15.go",
		"src/f16.go", "src/f17.go", "src/f18.go", "src/f19.go", "src/f20.go",
	}
	plan := agentflow.Plan{
		RiskLevel:       "low",
		ValidationGates: []string{"unit-tests"},
		Steps:           []agentflow.Step{{Files: files}},
	}
	claimedFiles := []string{files[0]}
	claimedGates := []string{}
	blast := "local"
	size := "xs"
	brief := agentflow.TaskBrief{
		SchemaVersion: agentflow.TaskBriefSchemaVersion,
		TaskType:      "bugfix", DeclaredRisk: "low",
		CandidateFiles: &claimedFiles, ValidationNeeds: &claimedGates,
		BlastRadius: &blast, DeclaredSize: &size,
	}
	b, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "brief.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readExternalTaskBrief(path, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got.CandidateFiles == nil || !reflect.DeepEqual(*got.CandidateFiles, files) {
		t.Fatalf("explicit brief hid exact plan files: %v", got.CandidateFiles)
	}
	if got.ValidationNeeds == nil || !reflect.DeepEqual(*got.ValidationNeeds, plan.ValidationGates) {
		t.Fatalf("explicit brief hid exact plan gates: %v", got.ValidationNeeds)
	}
}

func TestReadApprovedWorkflowHandoff_VerifiesExistingAgentflowContract(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := agentflow.Plan{Objective: "approved objective", RiskLevel: "low", ValidationGates: []string{"unit"}, Steps: []agentflow.Step{{Files: []string{"src/a.go"}}}}
	brief := agentflow.TaskBriefFromPlan(plan, "feature")
	recommendation := defaultWorkflowRecommendation()
	handoffPath, err := saveApprovedWorkflowHandoff(root, plan, brief, recommendation)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := json.Marshal(recommendation.Contract)
	if err != nil {
		t.Fatal(err)
	}
	contractPath := filepath.Join(root, ".agent", "workflow.contract.json")
	if err := os.WriteFile(contractPath, contract, 0o600); err != nil {
		t.Fatal(err)
	}

	planJSON := marshalPlanJSON(t, plan)
	var lockedPlan map[string]any
	if err := json.Unmarshal(planJSON, &lockedPlan); err != nil {
		t.Fatal(err)
	}
	lockedPlan["locked"] = true
	lockedPlan["locked_at"] = "2026-07-15T00:00:00+00:00"
	planJSON, err = json.Marshal(lockedPlan)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readApprovedWorkflowHandoff(handoffPath, root, planJSON, brief)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Selected != recommendation.Selected {
		t.Fatalf("approved recommendation = %+v", got)
	}
	tampered := bytes.Replace(contract, []byte(`"review_depth":"standard"`), []byte(`"review_depth":"deep"`), 1)
	if err := os.WriteFile(contractPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readApprovedWorkflowHandoff(handoffPath, root, planJSON, brief); err == nil || !strings.Contains(err.Error(), "approved workflow handoff") {
		t.Fatalf("tampered contract error = %v", err)
	}
}

func TestReadApprovedWorkflowHandoff_RejectsDifferentPlanOrBrief(t *testing.T) {
	root := t.TempDir()
	approvedPlan := agentflow.Plan{Objective: "approved objective", RiskLevel: "low"}
	brief := agentflow.TaskBriefFromPlan(approvedPlan, "feature")
	recommendation := defaultWorkflowRecommendation()
	handoffPath, err := saveApprovedWorkflowHandoff(root, approvedPlan, brief, recommendation)
	if err != nil {
		t.Fatal(err)
	}
	stalePairing := approvedPlan
	stalePairing.Objective = "different broad plan"
	if _, err := readApprovedWorkflowHandoff(handoffPath, root, marshalPlanJSON(t, stalePairing), brief); err == nil || !strings.Contains(err.Error(), "plan does not match") {
		t.Fatalf("stale plan pairing error = %v", err)
	}
	staleBrief := brief
	staleBrief.TaskType = "docs"
	if _, err := readApprovedWorkflowHandoff(handoffPath, root, marshalPlanJSON(t, approvedPlan), staleBrief); err == nil || !strings.Contains(err.Error(), "task brief does not match") {
		t.Fatalf("stale task brief pairing error = %v", err)
	}
}

func TestReadApprovedWorkflowHandoff_RejectsUnapprovedAgentflowPlanFields(t *testing.T) {
	root := t.TempDir()
	approvedPlan := agentflow.Plan{Objective: "approved objective", RiskLevel: "low"}
	brief := agentflow.TaskBriefFromPlan(approvedPlan, "feature")
	handoffPath, err := saveApprovedWorkflowHandoff(root, approvedPlan, brief, defaultWorkflowRecommendation())
	if err != nil {
		t.Fatal(err)
	}
	planJSON, err := json.Marshal(approvedPlan)
	if err != nil {
		t.Fatal(err)
	}
	var changed map[string]any
	if err := json.Unmarshal(planJSON, &changed); err != nil {
		t.Fatal(err)
	}
	changed["context_budget"] = map[string]any{"max_total_bytes": 1}
	planJSON, err = json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := readApprovedWorkflowHandoff(handoffPath, root, planJSON, brief); err == nil || !strings.Contains(err.Error(), "plan does not match") {
		t.Fatalf("unapproved Agentflow plan field error = %v", err)
	}
}

func TestRunAgentflowTask_RejectsStaleWorkflowHandoffBeforeClientUse(t *testing.T) {
	root := t.TempDir()
	ir := validTraceableIR()
	approvedPlan := agentflow.Compile(ir)
	brief := taskBriefFromIR(ir, approvedPlan)
	handoffPath, err := saveApprovedWorkflowHandoff(root, approvedPlan, brief, defaultWorkflowRecommendation())
	if err != nil {
		t.Fatal(err)
	}
	briefPath, err := saveApprovedTaskBrief(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	stalePairing := approvedPlan
	stalePairing.Objective = "a different plan must not inherit the approved route"
	planBytes, err := json.Marshal(stalePairing)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "different-plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	err = runAgentflowTask(context.Background(), &stdout, &stderr, nil, &replSession{}, flags{
		planPath: planPath, taskBriefPath: briefPath, workflowHandoffPath: handoffPath,
		approveEdits: true, approveGates: true,
		agentflowSrc: filepath.Join(t.TempDir(), "must-not-run"),
	}, root)
	if err == nil || !strings.Contains(err.Error(), "plan does not match approved workflow handoff") {
		t.Fatalf("stale handoff error = %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stale handoff reached task output/recovery: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunAgentflowTask_ParallelRequiresOrchestratorFactoryBeforeAgentflow(t *testing.T) {
	root := t.TempDir()
	plan := agentflow.Compile(validTraceableIR())
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &replSession{orch: agent.New(&scriptCaller{}, agent.ContextManager{})}
	var stdout, stderr bytes.Buffer
	err = runAgentflowTask(context.Background(), &stdout, &stderr, nil, sess, flags{
		planPath: planPath, planWorkers: 2, approveEdits: true, approveGates: true,
		agentflowSrc: filepath.Join(t.TempDir(), "must-not-run"),
	}, root)
	if err == nil || !strings.Contains(err.Error(), "parallel orchestrator factory is nil") {
		t.Fatalf("factory preflight error = %v", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("factory preflight reached Agentflow output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDriver_RouteIsReadOnlyBeforeMutationAndShownBeforeReview(t *testing.T) {
	rec := defaultWorkflowRecommendation()
	rec.Selected.Profile = "high-risk"
	rec.Contract.WorkflowProfile = "high-risk"
	rec.Contract.ReviewDepth = "deep"
	rec.Contract.ProofPolicy.RequireReviewRun = true
	af := &fakeAF{recommendation: rec, review: reviewRun(false)}
	var out strings.Builder
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: &out,
		taskBrief:       agentflow.TaskBrief{SchemaVersion: "0.1.0", TaskType: "feature", DeclaredRisk: "high"},
		workflowProfile: "high-risk", workflowReason: "operator requires deep review",
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	if _, err := d.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"probe", "probe-workflow", "recommend", "probe-review", "init", "lock:plan.json", "materialize"}
	if len(af.seq) < len(wantPrefix) || !equalSeq(af.seq[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("route/mutation prefix = %v, want %v", af.seq, wantPrefix)
	}
	if len(af.materializedRoutes) != 1 || af.materializedRoutes[0] != "high-risk" {
		t.Fatalf("materialized routes = %v", af.materializedRoutes)
	}
	if len(af.briefs) != 1 || af.briefs[0].DeclaredRisk != "high" || af.selectedProfiles[0] != "high-risk" || af.reasons[0] != "operator requires deep review" {
		t.Fatalf("route inputs = briefs %+v profile %v reason %v", af.briefs, af.selectedProfiles, af.reasons)
	}
	if strings.Count(out.String(), `selected: "agentflow-default" / "high-risk"`) != 2 ||
		!strings.Contains(out.String(), `review_depth: "deep"`) || !strings.Contains(out.String(), "Workflow route before review") {
		t.Fatalf("startup/review route output:\n%s", out.String())
	}
}

func TestDriver_WorkflowFailuresStopAtTheirMutationBoundary(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failAt   string
		wantSeq  []string
		wantText string
	}{
		{name: "workflow probe", failAt: "probe-workflow", wantSeq: []string{"probe", "probe-workflow"}, wantText: "workflow routing unavailable"},
		{name: "recommendation", failAt: "recommend", wantSeq: []string{"probe", "probe-workflow", "recommend"}, wantText: "recommend Agentflow workflow"},
		{name: "materialization", failAt: "materialize", wantSeq: []string{"probe", "probe-workflow", "recommend", "init", "lock:plan.json", "materialize"}, wantText: "materialize Agentflow workflow contract"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			af := &fakeAF{failAt: map[string]error{tt.failAt: errors.New("scripted failure")}}
			d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", out: io.Discard,
				runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
			_, err := d.run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want %q", err, tt.wantText)
			}
			if !equalSeq(af.seq, tt.wantSeq) {
				t.Fatalf("failure sequence = %v, want %v", af.seq, tt.wantSeq)
			}
			// #612 R10: only the probe failure skips the recovery report.
			var u *agentflowUnavailableError
			if got, want := errors.As(err, &u), tt.failAt == "probe-workflow"; got != want {
				t.Fatalf("%s: marked unavailable = %t, want %t", tt.name, got, want)
			}
		})
	}
}

func reviewRun(ready bool, findings ...agentflow.ReviewFinding) agentflow.ReviewRun {
	return agentflow.ReviewRun{
		ReviewRunID: "RR-20260714T120000Z-1234abcd", AmendmentReady: ready,
		GateStatus: "fail", ActiveBlocking: []string{"RF-1"},
		Findings: agentflow.ReviewFindings{Index: findings},
	}
}

func reviewPlan() *agentflow.Plan {
	return &agentflow.Plan{
		Objective: "repair the reviewed implementation", AllowedFiles: []string{"src/*"},
		Steps: []agentflow.Step{
			{ID: "P1", Action: "repair one", Files: []string{"src/a.go"}, Validation: []string{"test one"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./src/a"}}}},
			{ID: "P2", Action: "repair two", Files: []string{"src/b.go"}, Validation: []string{"test two"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./src/b"}}}},
		},
	}
}

func activeFinding(id, owner string) agentflow.ReviewFinding {
	return agentflow.ReviewFinding{
		FindingID: id, Severity: "high", Status: "accepted", OwningStep: owner,
		Claim: "the implementation omits " + id, SuggestedFix: "repair " + id,
	}
}

func TestDriver_AmendmentReadyFindingUsesExistingAttemptLifecycle(t *testing.T) {
	finding := activeFinding("RF-1", "P1")
	finding.Location = &agentflow.ReviewLocation{Path: "src/a.go", Line: 7, LineEnd: 9}
	af := &fakeAF{review: reviewRun(true, finding)}
	var gotAttempt string
	var gotGoal string
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
		runStep: func(_ context.Context, _ agentflow.Step, attempt, goal string) error {
			gotAttempt, gotGoal = attempt, goal
			return nil
		},
	}
	proof, err := d.run(context.Background())
	if err != nil || proof != "proof-pack.md" {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
	ref := "RR-20260714T120000Z-1234abcd#RF-1"
	want := []string{
		"probe", "probe-workflow", "recommend", "probe-review", "init", "lock:plan.json", "materialize", "init-exec", "doctor", "next-step",
		"record-review:review.json", "amend:P1:" + ref, "gate:P1:test one", "finish-step:P1:AM-P1", "next-action", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq=%v want=%v", af.seq, want)
	}
	if gotAttempt != "AM-P1" || !strings.Contains(gotGoal, ref) || !strings.Contains(gotGoal, "src/a.go:7-9") {
		t.Fatalf("attempt=%q goal=%q", gotAttempt, gotGoal)
	}
}

func TestAmendmentGoal_ContainsOnlyLockedSliceAndAuthoritativeRepairContext(t *testing.T) {
	plan := reviewPlan()
	finding := activeFinding("RF-1", "P1")
	finding.Location = &agentflow.ReviewLocation{Path: "src/a.go", Line: 7, LineEnd: 9}
	goal, err := amendmentGoal(plan, plan.Steps[0], "RR-20260714T120000Z-1234abcd", []agentflow.ReviewFinding{finding})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"repair one", "src/a.go", "RR-20260714T120000Z-1234abcd#RF-1",
		"the implementation omits RF-1", "src/a.go:7-9", "repair RF-1",
	} {
		if !strings.Contains(goal, want) {
			t.Fatalf("goal missing %q:\n%s", want, goal)
		}
	}
	for _, unwanted := range []string{"repair two", "src/b.go", "Severity:", "Status:"} {
		if strings.Contains(goal, unwanted) {
			t.Fatalf("goal leaked %q:\n%s", unwanted, goal)
		}
	}
}

func TestDriver_LegacyAndInactiveFindingsRemainVisibleWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		run  agentflow.ReviewRun
	}{
		{"legacy active", reviewRun(false, activeFinding("RF-1", ""))},
		{"inactive", reviewRun(true, agentflow.ReviewFinding{FindingID: "RF-2", Severity: "low", Status: "fixed"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			af := &fakeAF{review: tt.run}
			var out bytes.Buffer
			edits := 0
			d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: &out,
				runStep: func(context.Context, agentflow.Step, string, string) error { edits++; return nil }}
			if _, err := d.run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if edits != 0 || strings.Contains(strings.Join(af.seq, "\n"), "amend:") {
				t.Fatalf("display-only finding mutated: edits=%d seq=%v", edits, af.seq)
			}
			finding := tt.run.Findings.Index[0]
			want := tt.run.ReviewRunID + "#" + finding.FindingID + " status=" + finding.Status + " amendment=display-only"
			if !strings.Contains(out.String(), want) {
				t.Fatalf("output %q missing %q", out.String(), want)
			}
		})
	}
}

func TestDriver_RejectsMalformedAmendmentProjectionBeforeEdit(t *testing.T) {
	tests := []struct {
		name    string
		finding agentflow.ReviewFinding
		second  *agentflow.ReviewFinding
	}{
		{"unknown owner", activeFinding("RF-1", "PX"), nil},
		{"missing claim", func() agentflow.ReviewFinding { f := activeFinding("RF-1", "P1"); f.Claim = ""; return f }(), nil},
		{"bad location", func() agentflow.ReviewFinding {
			f := activeFinding("RF-1", "P1")
			f.Location = &agentflow.ReviewLocation{Line: 2}
			return f
		}(), nil},
		{"duplicate id", activeFinding("RF-1", "P1"), func() *agentflow.ReviewFinding { f := activeFinding("RF-1", "P2"); return &f }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := []agentflow.ReviewFinding{tt.finding}
			if tt.second != nil {
				findings = append(findings, *tt.second)
			}
			af := &fakeAF{review: reviewRun(true, findings...)}
			edits := 0
			d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
				runStep: func(context.Context, agentflow.Step, string, string) error { edits++; return nil }}
			if _, err := d.run(context.Background()); err == nil {
				t.Fatal("expected malformed projection error")
			}
			if edits != 0 || strings.Contains(strings.Join(af.seq, "\n"), "amend:") || strings.Contains(strings.Join(af.seq, "\n"), "finish-run") {
				t.Fatalf("malformed projection advanced execution: edits=%d seq=%v", edits, af.seq)
			}
		})
	}
}

func TestDriver_GroupsMultipleFindingsByLockedPlanOrder(t *testing.T) {
	af := &fakeAF{review: reviewRun(true,
		activeFinding("RF-2", "P2"), activeFinding("RF-1", "P1"), activeFinding("RF-3", "P2"),
	)}
	var calls []string
	var goals []string
	d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
		runStep: func(_ context.Context, step agentflow.Step, _ string, goal string) error {
			var ids []string
			for _, id := range []string{"RF-1", "RF-2", "RF-3"} {
				if strings.Contains(goal, "#"+id) {
					ids = append(ids, id)
				}
			}
			calls = append(calls, step.ID+":"+strings.Join(ids, ","))
			goals = append(goals, goal)
			return nil
		}}
	if _, err := d.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"P1:RF-1", "P2:RF-2,RF-3"}; !equalSeq(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
	if len(goals) != 2 || strings.Index(goals[1], "#RF-2") > strings.Index(goals[1], "#RF-3") {
		t.Fatalf("P2 finding order changed:\n%s", goals[1])
	}
}

func TestDriver_AmendmentFailuresStayIncomplete(t *testing.T) {
	tests := []struct {
		name   string
		failAt string
		runErr error
		block  string // the one block-step call, or "" for none
	}{
		{"record review command", "record-review", nil, ""},
		{"amend command", "amend-step", nil, ""},
		{"model", "", errors.New("model failed"), "block-step:P1:AM-P1:golem: agent run failed"},
		{"receipt", "", &unreceiptedEditError{err: errors.New("record rejected")}, "block-step:P1:AM-P1:golem: unreceipted edit aborted the run"},
		{"gate", "gate", nil, "block-step:P1:AM-P1:golem: gate execution failed"},
		{"finish step", "finish-step", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			af := &fakeAF{review: reviewRun(true, activeFinding("RF-1", "P1"))}
			if tt.failAt != "" {
				af.failAt = map[string]error{tt.failAt: errors.New(tt.failAt + " failed")}
			}
			d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard, root: t.TempDir(),
				runStep: func(context.Context, agentflow.Step, string, string) error { return tt.runErr }}
			if _, err := d.run(context.Background()); err == nil {
				t.Fatal("expected failure")
			}
			seq := strings.Join(af.seq, "\n")
			if strings.Contains(seq, "finish-run") {
				t.Fatalf("failed amendment reached finish-run: %v", af.seq)
			}
			if tt.failAt != "record-review" && !strings.Contains(seq, "record-review:") {
				t.Fatalf("review evidence was not recorded first: %v", af.seq)
			}
			if (tt.runErr != nil || tt.failAt == "gate" || tt.failAt == "finish-step") && !strings.Contains(seq, "amend:P1:") {
				t.Fatalf("failure did not leave an opened amendment: %v", af.seq)
			}
			// #642 A1, A2: a failed amendment run or gate blocks exactly its own
			// attempt; a record-review, amend-step or FinishStep error (A4)
			// blocks nothing.
			var blocks, want []string
			for _, call := range af.seq {
				if strings.HasPrefix(call, "block-step:") {
					blocks = append(blocks, call)
				}
			}
			if tt.block != "" {
				want = []string{tt.block}
			}
			if !slices.Equal(blocks, want) {
				t.Fatalf("block-step calls = %q, want %q (all calls: %v)", blocks, want, af.seq)
			}
		})
	}
}

func TestDriver_BlockingReviewRequiresAuthoritativeRereview(t *testing.T) {
	af := &fakeAF{
		review:     reviewRun(true, activeFinding("RF-1", "P1")),
		proofError: &agentflow.FinishRunError{StoppedAt: "verify-proof", Diagnostics: []string{"review gate still blocked"}},
	}
	d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil }}
	proof, err := d.run(context.Background())
	if err == nil || proof != "" || !strings.Contains(err.Error(), "review gate still blocked") {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
	if !strings.Contains(strings.Join(af.seq, "\n"), "finish-step:P1:AM-P1") {
		t.Fatalf("amendment did not complete before authoritative re-review block: %v", af.seq)
	}
}

func TestValidateFreshWorkerProjection(t *testing.T) {
	decode := func(t *testing.T, payload string) agentflow.NextActionState {
		t.Helper()
		var state agentflow.NextActionState
		if err := json.Unmarshal([]byte(payload), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	freshJSON := `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`
	fresh := decode(t, freshJSON)
	if err := validateFreshWorkerProjection(fresh, "golem-w1"); err != nil {
		t.Fatalf("fresh projection = %v", err)
	}
	for name, payload := range map[string]string{
		"missing projection":       `{}`,
		"missing contract":         `{"resumability":{"agent_id":"golem-w1","attempt":null,"diagnostics":[]}}`,
		"unlocked":                 `{"resumability":{"contract":{"plan_sha256":"plan","execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":null,"diagnostics":[]}}`,
		"missing plan digest":      `{"resumability":{"contract":{"locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":null,"diagnostics":[]}}`,
		"missing execution digest": `{"resumability":{"contract":{"locked":true,"plan_sha256":"plan"},"agent_id":"golem-w1","attempt":null,"diagnostics":[]}}`,
		"other agent":              `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w2","attempt":null,"diagnostics":[]}}`,
		"present attempt":          `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":{"id":"A1","open":false},"diagnostics":[]}}`,
		"missing step":             `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":null,"diagnostics":[]}}`,
		"failed step":              `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"failed","completed":false},"attempt":null,"diagnostics":[]}}`,
		"blocked step":             `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"blocked","completed":false},"attempt":null,"diagnostics":[]}}`,
		"missing step completed":   `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"pending"},"attempt":null,"diagnostics":[]}}`,
		"null step completed":      `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"pending","completed":null},"attempt":null,"diagnostics":[]}}`,
		"completed step":           `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"pending","completed":true},"attempt":null,"diagnostics":[]}}`,
		"diagnostic":               `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":null,"diagnostics":[{"code":"state_invalid","message":"bad ledger","artifact":".agent/step-runs.jsonl"}]}}`,
		"missing attempt":          `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","diagnostics":[]}}`,
		"missing diagnostics":      `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","attempt":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFreshWorkerProjection(decode(t, payload), "golem-w1"); err == nil {
				t.Fatal("expected non-fresh projection rejection")
			}
		})
	}
}

func TestValidateTraceability_RejectsInvalidReferences(t *testing.T) {
	tests := []struct {
		name     string
		step     agentflow.Step
		wantCode string
	}{
		{
			name:     "dangling step criterion",
			step:     agentflow.Step{ID: "P1", CriterionIDs: []string{"AC-MISSING"}},
			wantCode: "dangling_step_criterion",
		},
		{
			name: "gate criterion outside parent step",
			step: agentflow.Step{
				ID: "P1", CriterionIDs: []string{"AC-1"},
				Gates: []agentflow.Gate{{Kind: "command", Run: []string{"true"}, CriterionIDs: []string{"AC-2"}}},
			},
			wantCode: "gate_criterion_not_in_step",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &agentflow.Plan{
				Requirements: []agentflow.Requirement{{
					ID: "REQ-1", Text: "behavior",
					AcceptanceCriteria: []agentflow.Criterion{
						{ID: "AC-1", Text: "implemented", Review: &agentflow.CriterionReview{MinimumDepth: "deep"}},
						{ID: "AC-2", Text: "verified", Review: &agentflow.CriterionReview{MinimumDepth: "deep"}},
					},
				}},
				Steps: []agentflow.Step{tt.step},
			}
			err := validateTraceability(*plan)
			if err == nil || !strings.Contains(err.Error(), tt.wantCode) {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}
}

// fakeAgentflowOnPath puts an `agentflow` script first on PATH. It appends each
// invocation's arguments to a log and answers --version with version; any
// other call prints {}. The returned log path exists only once the script ran.
func fakeAgentflowOnPath(t *testing.T, version string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" +
		"if [ \"$1\" = --version ]; then printf 'agentflow %s\\n' " + shellQuote(version) + "; exit 0; fi\n" +
		"printf '{}\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "agentflow"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestRunAgentflowTask_RejectsInvalidTraceabilityBeforeClientUse(t *testing.T) {
	planJSON := `{
		"schema_version":"1.0.0",
		"requirements":[{"id":"REQ-1","text":"behavior","acceptance_criteria":[{"id":"AC-1","text":"verified","review":{"minimum_depth":"deep"}}]}],
		"steps":[{"id":"P1","files":["a.go"],"criterion_ids":["AC-MISSING"],"validation":["true"],"gates":[{"kind":"command","run":["true"]}]}]
	}`
	planPath := filepath.Join(t.TempDir(), "invalid-traceability.json")
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runAgentflowTask(context.Background(), &stdout, &stderr, nil, &replSession{}, flags{
		planPath: planPath, approveEdits: true, approveGates: true,
		agentflowSrc: filepath.Join(t.TempDir(), "must-not-run"),
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "dangling_step_criterion") {
		t.Fatalf("err = %v, want dangling_step_criterion", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("preflight rejection used task output or recovery: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// #612 R4: a 0.x plan file is refused before any AgentFlow call.
func TestRunAgentflowTask_RejectsZeroXPlanBeforeClientUse(t *testing.T) {
	calls := fakeAgentflowOnPath(t, "1.0.0")
	plan := agentflow.Compile(validTraceableIR())
	plan.SchemaVersion = "0.3.0"
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &replSession{orch: agent.New(&scriptCaller{}, agent.ContextManager{})}
	var stdout, stderr bytes.Buffer
	err = runAgentflowTask(context.Background(), &stdout, &stderr, nil, sess, flags{
		planPath: planPath, approveEdits: true, approveGates: true,
	}, t.TempDir())
	want := `plan schema_version "0.3.0" is not an AgentFlow 1.x plan; migrate it to schema_version 1.0.0 and review it again, or re-plan with -goal after moving any existing .agent/ aside`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("schema rejection used task output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got, readErr := os.ReadFile(calls); readErr == nil {
		t.Fatalf("AgentFlow ran before the schema check: %q", got)
	}
}

// #612 U7: with a rejected AgentFlow, fresh runs and resume make no call but
// --version: resume probes first (R6), and the failure report does not ask the
// refused binary for next-action or status (R10).
func TestRunAgentflowTask_RejectedVersionMakesNoOtherCall(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			root := t.TempDir()
			calls := fakeAgentflowOnPath(t, "0.4.0")
			planBytes, err := json.Marshal(agentflow.Compile(validTraceableIR()))
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			sess := &replSession{orch: agent.New(&scriptCaller{}, agent.ContextManager{}), maxSteps: 4, clock: time.Now}
			var stdout, stderr bytes.Buffer
			err = runAgentflowTask(context.Background(), &stdout, &stderr, nil, sess, flags{
				planPath: planPath, approveEdits: true, approveGates: true, agentflowResume: resume,
			}, root)
			if !errors.Is(err, errAgentflowTaskFailed) {
				t.Fatalf("err = %v, want errAgentflowTaskFailed", err)
			}
			if !strings.Contains(stderr.String(), "agentflow 0.4 is too old; need >= 1.0") {
				t.Fatalf("stderr = %q, want the version rejection", stderr.String())
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "--version\n" {
				t.Fatalf("AgentFlow calls = %q, want only --version", got)
			}
		})
	}
}

// The driver marks every probe failure, so runAgentflowTask can skip recovery.
// Each row fails one probe; parallel and review probes run only when their
// feature is configured.
func TestDriverRun_MarksProbeFailuresUnavailable(t *testing.T) {
	for _, tt := range []struct {
		probe    string
		wantErr  string
		parallel bool
		review   string
	}{
		{probe: "probe", wantErr: "agentflow unavailable: too old"},
		{probe: "probe-parallel", wantErr: "agentflow parallel runtime unavailable: too old", parallel: true},
		{probe: "probe-workflow", wantErr: "agentflow workflow routing unavailable: too old"},
		{probe: "probe-review", wantErr: "agentflow review unavailable: too old", review: "review.json"},
	} {
		t.Run(tt.probe, func(t *testing.T) {
			af := &fakeAF{failAt: map[string]error{tt.probe: errors.New("too old")}}
			d := &driver{
				af: af, plan: stopTestPlan(), reviewManifest: tt.review,
				runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
			}
			if tt.parallel {
				d.parallelCohort = func(context.Context) error { return nil }
			}
			_, err := d.run(context.Background())
			var unavailable *agentflowUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("failure = %v, want *agentflowUnavailableError", err)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("error = %q, want %q", err.Error(), tt.wantErr)
			}
			if got := af.seq[len(af.seq)-1]; got != tt.probe {
				t.Fatalf("last call = %q (seq %v), want the failing probe", got, af.seq)
			}
		})
	}
}

func TestRunAgentflowTask_RejectsInvalidDesignTraceabilityBeforeClientUse(t *testing.T) {
	planJSON := `{
		"schema_version":"1.0.0",
		"design_decisions":[],
		"steps":[{"id":"P1","files":["a.go"],"design_decision_ids":["DD-MISSING"],"validation":["true"],"gates":[{"kind":"command","run":["true"]}]}]
	}`
	planPath := filepath.Join(t.TempDir(), "invalid-design-traceability.json")
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runAgentflowTask(context.Background(), &stdout, &stderr, nil, &replSession{}, flags{
		planPath: planPath, approveEdits: true, approveGates: true,
		agentflowSrc: filepath.Join(t.TempDir(), "must-not-run"),
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "missing_design_decisions") {
		t.Fatalf("err = %v, want missing_design_decisions", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("preflight rejection used task output or recovery: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDecodeAgentflowPlanJSON_RejectsMalformedDesignLists(t *testing.T) {
	for name, input := range map[string]string{
		"null declarations": `{"design_decisions":null}`,
		"null references":   `{"design_decisions":[{"id":"DD-1","text":"x","references":null}]}`,
		"null selections":   `{"steps":[{"design_decision_ids":null}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var plan agentflow.Plan
			if err := decodeAgentflowPlanJSON([]byte(input), &plan); err == nil {
				t.Fatal("expected malformed design list rejection")
			}
		})
	}
}

func TestStepGoal_ProjectsOnlyTheLockedStepSpecificationSlice(t *testing.T) {
	plan := &agentflow.Plan{
		Objective:  "ship the requested behavior",
		Invariants: []string{"no new dependencies", "proof state stays opaque"},
		NonGoals:   []string{"redesign authoring"},
		Requirements: []agentflow.Requirement{
			{ID: "REQ-1", Text: "support the primary workflow", AcceptanceCriteria: []agentflow.Criterion{
				{ID: "AC-1", Text: "focused behavior works"},
				{ID: "AC-2", Text: "focused validation passes", Review: &agentflow.CriterionReview{MinimumDepth: "deep"}},
			}},
			{ID: "REQ-2", Text: "support an unrelated workflow", AcceptanceCriteria: []agentflow.Criterion{
				{ID: "AC-3", Text: "unrelated behavior works"},
			}},
		},
	}
	step := agentflow.Step{
		ID:            "P1",
		Preconditions: []string{"baseline tests pass"},
		Action:        "implement the focused behavior",
		Files:         []string{"src/a.go", "src/a_test.go"},
		ExpectedDiff:  []string{"add focused implementation", "add regression coverage"},
		Validation:    []string{"focused tests"},
		CriterionIDs:  []string{"AC-2", "AC-1"},
		Gates: []agentflow.Gate{{
			Kind: "command", Run: []string{"go", "test", "./src", "-run", "TestFocused"},
			CriterionIDs: []string{"AC-1", "AC-2"},
		}},
	}

	want := `Locked specification slice

Objective:
ship the requested behavior

Invariants:
- no new dependencies
- proof state stays opaque

Non-goals:
- redesign authoring

Step P1
Preconditions:
- baseline tests pass
Action:
implement the focused behavior
Target files:
- src/a.go
- src/a_test.go
Expected diff:
- add focused implementation
- add regression coverage
Validation intent:
- focused tests
Structured gates:
- focused tests: ["go", "test", "./src", "-run", "TestFocused"] (criteria: AC-1, AC-2)
Requirements and acceptance criteria:
- REQ-1: support the primary workflow
  - AC-1: focused behavior works
  - AC-2: focused validation passes (review minimum depth: deep)`
	got, err := stepGoal(plan, step)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("step goal mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if retry, err := stepGoal(plan, step); err != nil || retry != got {
		t.Fatalf("retry rendering changed\nfirst: %q\nretry: %q", got, retry)
	}

	other := agentflow.Step{ID: "P2", Action: "implement the unrelated behavior", CriterionIDs: []string{"AC-3"}}
	otherGoal, err := stepGoal(plan, other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(otherGoal, "- REQ-2: support an unrelated workflow\n  - AC-3: unrelated behavior works") ||
		strings.Contains(otherGoal, "REQ-1") || strings.Contains(otherGoal, "AC-1") || strings.Contains(otherGoal, "AC-2") {
		t.Fatalf("second step received the wrong requirement slice:\n%s", otherGoal)
	}
}

func TestStepGoal_TraceableStepWithoutCriteriaHasExplicitEmptySlice(t *testing.T) {
	plan := &agentflow.Plan{
		Objective: "prepare shared scaffolding",
		Requirements: []agentflow.Requirement{{
			ID: "REQ-1", Text: "later step behavior",
			AcceptanceCriteria: []agentflow.Criterion{{ID: "AC-1", Text: "later behavior works"}},
		}},
	}
	step := agentflow.Step{ID: "P0", Action: "prepare scaffolding"}

	got, err := stepGoal(plan, step)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "Requirements and acceptance criteria:\n(none)") {
		t.Fatalf("criteria-less traced step omitted explicit empty slice:\n%s", got)
	}
}

func TestStepGoal_ProjectsSelectedDesignDecisionsInDeclarationOrder(t *testing.T) {
	references := []string{"ADR-2", "ADR-1"}
	decisions := []agentflow.DesignDecision{
		{ID: "DD-2", Text: "second decision", References: &references},
		{ID: "DD-1", Text: "first decision"},
		{ID: "DD-3", Text: "unselected decision"},
	}
	selected := []string{"DD-1", "DD-2"}
	plan := &agentflow.Plan{
		Objective: "ship the requested behavior", Invariants: []string{"stay compatible"},
		DesignDecisions: &decisions,
	}
	step := agentflow.Step{
		ID: "P1", Action: "implement the focused behavior", Files: []string{"src/a.go"},
		ExpectedDiff: []string{"add focused implementation"}, DesignDecisionIDs: &selected,
		Validation: []string{"focused tests"},
		Gates:      []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./src"}}},
	}

	want := `Locked specification slice

Objective:
ship the requested behavior

Invariants:
- stay compatible

Non-goals:
(none)

Step P1
Preconditions:
(none)
Action:
implement the focused behavior
Target files:
- src/a.go
Expected diff:
- add focused implementation
Validation intent:
- focused tests
Structured gates:
- focused tests: ["go", "test", "./src"]
Requirements and acceptance criteria:
(none)
Design decisions:
- DD-2: second decision
  - reference: ADR-2
  - reference: ADR-1
- DD-1: first decision`
	got, err := stepGoal(plan, step)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("design instruction mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if retry, err := stepGoal(plan, step); err != nil || retry != got {
		t.Fatalf("retry rendering changed\nfirst: %q\nretry: %q", got, retry)
	}
}

func TestStepGoal_DesignFamilyWithoutStepSelectionHasExplicitEmptySlice(t *testing.T) {
	decisions := []agentflow.DesignDecision{{ID: "DD-1", Text: "used by a later step"}}
	plan := &agentflow.Plan{DesignDecisions: &decisions}
	got, err := stepGoal(plan, agentflow.Step{ID: "P0", Action: "prepare scaffolding"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "Design decisions:\n(none)") || !strings.Contains(got, "Locked specification slice") {
		t.Fatalf("design-only step omitted expanded explicit empty slice:\n%s", got)
	}
}

func TestStepGoal_UsesExecutionGateLabelFallback(t *testing.T) {
	plan := &agentflow.Plan{Requirements: []agentflow.Requirement{{
		ID: "REQ-1", Text: "behavior",
		AcceptanceCriteria: []agentflow.Criterion{{ID: "AC-1", Text: "verified"}},
	}}}
	step := agentflow.Step{
		ID: "P1", Action: "implement behavior", CriterionIDs: []string{"AC-1"},
		Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./..."}, CriterionIDs: []string{"AC-1"}}},
	}
	got, err := stepGoal(plan, step)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Validation intent:\n- go test ./...") ||
		!strings.Contains(got, "Structured gates:\n- go test ./...: [\"go\", \"test\", \"./...\"] (criteria: AC-1)") {
		t.Fatalf("prompt omitted execution-derived gate label:\n%s", got)
	}
}

func TestStepGoal_AttributesCriteriaToCommandGateUnderKindFilter(t *testing.T) {
	// P0 preflight rejects non-command gates before rendering, but the renderer
	// must not depend on that: gate criteria have to ride the extracted gate,
	// not an index back into the unfiltered step.gates slice.
	plan := &agentflow.Plan{Requirements: []agentflow.Requirement{{
		ID: "REQ-1", Text: "behavior",
		AcceptanceCriteria: []agentflow.Criterion{{ID: "AC-1", Text: "verified"}},
	}}}
	step := agentflow.Step{
		ID: "P1", Action: "implement behavior", CriterionIDs: []string{"AC-1"},
		Gates: []agentflow.Gate{
			{Kind: "inspection", CriterionIDs: []string{"AC-WRONG"}},
			{Kind: "command", Run: []string{"go", "test", "./..."}, CriterionIDs: []string{"AC-1"}},
		},
	}
	got, err := stepGoal(plan, step)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Structured gates:\n- go test ./...: [\"go\", \"test\", \"./...\"] (criteria: AC-1)") ||
		strings.Contains(got, "AC-WRONG") {
		t.Fatalf("gate criteria misattributed across the command-kind filter:\n%s", got)
	}
}

func TestStepGoal_PropagatesInvalidCommandGate(t *testing.T) {
	plan := &agentflow.Plan{Requirements: []agentflow.Requirement{{ID: "REQ-1"}}}
	step := agentflow.Step{ID: "P1", Gates: []agentflow.Gate{{Kind: "command"}}}
	if _, err := stepGoal(plan, step); err == nil || !strings.Contains(err.Error(), "empty run") {
		t.Fatalf("err = %v, want empty run", err)
	}
}

func TestStepGoal_LegacyPlanPreservesExactInstructionBytes(t *testing.T) {
	plan := &agentflow.Plan{}
	step := agentflow.Step{
		Action:       "change the requested behavior",
		Files:        []string{"a.go", "a_test.go"},
		ExpectedDiff: []string{"implementation changes", "tests change"},
	}
	want := "Make exactly the change described. Action: change the requested behavior\n" +
		"Target files: a.go, a_test.go\nExpected diff:\nimplementation changes\ntests change"
	if got, err := stepGoal(plan, step); err != nil || got != want {
		t.Fatalf("legacy instruction bytes changed\n got: %q\nwant: %q", got, want)
	}
}

func TestTaskApprover_ApprovesWhenEnabled(t *testing.T) {
	if ok, err := taskApprover(true).Approve(context.Background(), provider.ToolCall{}, ""); !ok || err != nil {
		t.Fatalf("enabled: ok=%v err=%v", ok, err)
	}
	if ok, err := taskApprover(false).Approve(context.Background(), provider.ToolCall{}, ""); ok || err != nil {
		t.Fatalf("disabled: ok=%v err=%v", ok, err)
	}
}

func TestReadEvidenceSidecar(t *testing.T) {
	tests := []struct {
		name        string
		write       bool // false => empty-path case (no file)
		content     string
		wantLen     int
		wantFirstID string
		wantErr     bool
	}{
		{name: "empty path", write: false, wantLen: 0},
		{name: "single object", write: true, content: `{"id":"E1","claim":"c","source":"s"}`, wantLen: 1, wantFirstID: "E1"},
		{name: "array of two", write: true, content: `[{"id":"E1","claim":"c1","source":"s1"},{"id":"E2","claim":"c2","source":"s2"}]`, wantLen: 2, wantFirstID: "E1"},
		{name: "missing source", write: true, content: `{"id":"E1","claim":"c","source":""}`, wantErr: true},
		{name: "malformed json", write: true, content: `{not json`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.write {
				path = filepath.Join(t.TempDir(), "evidence.json")
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readEvidenceSidecar(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got entries=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len=%d want %d (%v)", len(got), tt.wantLen, got)
			}
			if tt.wantFirstID != "" && got[0].ID != tt.wantFirstID {
				t.Fatalf("first id=%q want %q", got[0].ID, tt.wantFirstID)
			}
		})
	}
}

// TestRunAgentflowTask_RequiresApprovalFlags pins the fail-before-exec invariant:
// a headless run missing either approval class errors before building the runner
// or touching agentflow (no binary is on PATH here).
func TestRunAgentflowTask_RequiresApprovalFlags(t *testing.T) {
	planJSON := `{"schema_version":"1.0.0","steps":[{"id":"P1","files":["a.go"],"validation":["go test"],"gates":[{"kind":"command","run":["go","test"]}]}]}`
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		approveEdits bool
		approveGates bool
		wantContains string
	}{
		{"missing edits", false, true, "approve-plan-edits"},
		{"missing gates", true, false, "approve-plan-gates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			f := flags{planPath: planPath, approveEdits: tc.approveEdits, approveGates: tc.approveGates}
			err := runAgentflowTask(context.Background(), &stdout, &stderr, nil, &replSession{}, f, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("err=%v, want contains %q", err, tc.wantContains)
			}
		})
	}
}

func TestResolveTaskPlanPath(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	got, err := resolveTaskPlanPath(filepath.Join("plans", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "plans", "plan.json"); got != want {
		t.Fatalf("relative path = %q, want %q", got, want)
	}

	abs := filepath.Join(t.TempDir(), "plan.json")
	got, err = resolveTaskPlanPath(abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("absolute path = %q, want unchanged %q", got, abs)
	}
}

func TestResolveTaskAgentflowSourcePinsRelativeCheckoutToCanonicalRoot(t *testing.T) {
	root := t.TempDir()
	relative := filepath.Join("tools", "agentflow")
	got, err := resolveTaskAgentflowSource(root, relative)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, relative)
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("relative source = %q, want absolute %q", got, want)
	}
	workerRoot := t.TempDir()
	if got == filepath.Join(workerRoot, relative) {
		t.Fatalf("source checkout moved with worker root: %q", got)
	}

	absolute := filepath.Join(t.TempDir(), "agentflow")
	if got, err := resolveTaskAgentflowSource(root, absolute); err != nil || got != absolute {
		t.Fatalf("absolute source = %q, %v; want %q", got, err, absolute)
	}
	if got, err := resolveTaskAgentflowSource(root, ""); err != nil || got != "" {
		t.Fatalf("binary mode source = %q, %v", got, err)
	}
}

func TestResolveReviewManifest(t *testing.T) {
	if got, err := resolveReviewManifest(""); err != nil || got != "" {
		t.Fatalf(`empty manifest: got %q err %v (want "", nil)`, got, err)
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "review.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	// A relative manifest resolves against the caller's cwd, not agentflow's -root.
	got, err := resolveReviewManifest("review.json")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "review.json"); got != want {
		t.Fatalf("relative manifest = %q, want %q", got, want)
	}

	// A missing manifest fails fast, before any agentflow call.
	if _, err := resolveReviewManifest(filepath.Join(cwd, "missing.json")); err == nil ||
		!strings.Contains(err.Error(), "review manifest") {
		t.Fatalf("missing manifest err = %v, want review manifest preflight failure", err)
	}
}

func TestReviewAmendments_ValidatesActiveButToleratesDisplayOnly(t *testing.T) {
	plan := reviewPlan()

	reject := []struct {
		name string
		run  agentflow.ReviewRun
		want string
	}{
		{"bad review_run_id", func() agentflow.ReviewRun {
			r := reviewRun(true, activeFinding("RF-1", "P1"))
			r.ReviewRunID = "not-a-run-id"
			return r
		}(), "review_run_id"},
		{"empty finding_id", reviewRun(true, agentflow.ReviewFinding{
			FindingID: "  ", Severity: "high", Status: "accepted",
			OwningStep: "P1", Claim: "c", SuggestedFix: "f",
		}), "finding_id"},
		{"invalid severity on active", reviewRun(true, func() agentflow.ReviewFinding {
			f := activeFinding("RF-1", "P1")
			f.Severity = "info"
			return f
		}()), "invalid severity"},
	}
	for _, tt := range reject {
		t.Run("reject/"+tt.name, func(t *testing.T) {
			if _, err := reviewAmendments(plan, tt.run); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// Display-only findings are never turned into work, so unrecognized
	// severity/status vocabulary and a location they never consume must not
	// abort a completed run.
	tolerate := []struct {
		name    string
		finding agentflow.ReviewFinding
	}{
		{"unknown severity and status", agentflow.ReviewFinding{FindingID: "RF-9", Severity: "info", Status: "acknowledged"}},
		{"inactive with pathless location", agentflow.ReviewFinding{
			FindingID: "RF-8", Severity: "low", Status: "fixed", Location: &agentflow.ReviewLocation{Line: 5},
		}},
	}
	for _, tt := range tolerate {
		t.Run("tolerate/"+tt.name, func(t *testing.T) {
			amendments, err := reviewAmendments(plan, reviewRun(true, tt.finding))
			if err != nil {
				t.Fatalf("display-only finding rejected: %v", err)
			}
			if len(amendments) != 0 {
				t.Fatalf("display-only finding produced amendments: %v", amendments)
			}
		})
	}
}

func TestDriver_MixedFindingsReportQueuedAndDisplayOnly(t *testing.T) {
	active := activeFinding("RF-1", "P1")
	inactive := agentflow.ReviewFinding{FindingID: "RF-2", Severity: "low", Status: "superseded"}
	af := &fakeAF{review: reviewRun(true, active, inactive)}
	var out bytes.Buffer
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: &out,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	if _, err := d.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"#RF-1 status=accepted amendment=queued",
		"#RF-2 status=superseded amendment=display-only",
		"gate=fail amendment_ready=true active_blocking=RF-1",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}

func TestFormatReviewLocation_Variants(t *testing.T) {
	cases := []struct {
		loc  agentflow.ReviewLocation
		want string
	}{
		{agentflow.ReviewLocation{Path: "src/a.go"}, "src/a.go"},
		{agentflow.ReviewLocation{Path: "src/a.go", Line: 7}, "src/a.go:7"},
		{agentflow.ReviewLocation{Path: "src/a.go", Line: 7, LineEnd: 9}, "src/a.go:7-9"},
	}
	for _, tt := range cases {
		if got := formatReviewLocation(tt.loc); got != tt.want {
			t.Fatalf("formatReviewLocation(%+v) = %q, want %q", tt.loc, got, tt.want)
		}
	}
}

func TestDriver_ReviewProbeFailureAbortsBeforeInit(t *testing.T) {
	af := &fakeAF{
		review: reviewRun(true, activeFinding("RF-1", "P1")),
		failAt: map[string]error{"probe-review": errors.New("amend-step unavailable")},
	}
	d := &driver{
		af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
		runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
	}
	if _, err := d.run(context.Background()); err == nil || !strings.Contains(err.Error(), "review unavailable") {
		t.Fatalf("err = %v, want review unavailable", err)
	}
	if want := []string{"probe", "probe-workflow", "recommend", "probe-review"}; !equalSeq(af.seq, want) {
		t.Fatalf("review-probe failure must abort before Init: seq = %v", af.seq)
	}
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The Agentflow task driver sends the session's composed prompt, so the
// current Git block reaches direct task requests; captured on the wire rather
// than inferred from sess.baseSystem.
func TestAgentflowDriverUsesCurrentGitFragment(t *testing.T) {
	root := t.TempDir()
	plan := &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"out.txt"}}}}
	caller := &captureCaller{answer: "done"}
	orch := agent.New(caller, agent.ContextManager{})
	in := systemInputs{projectContext: "<<<PROJECT_CONTEXT (advisory)\nrepo rule\n>>>PROJECT_CONTEXT",
		gitContext: gitContextOpen + "\nbranch: main\nrecent commits (newest first): (none)\nworking tree: clean\n" + gitContextClose}
	sess := &replSession{maxSteps: 4, sysInputs: in, baseSystem: composeSystem(in)}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, &fakeAF{}, orch, sess, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: &fakeAF{}, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	if caller.system != sess.baseSystem || strings.Count(caller.system, gitContextOpen) != 1 || strings.Index(caller.system, "repo rule") > strings.Index(caller.system, gitContextOpen) {
		t.Fatalf("driver request System differs from the composed prompt or misorders the fragments:\n got=%q\nwant=%q", caller.system, sess.baseSystem)
	}
}

// A direct Agentflow task request after /git-context refresh carries the
// refreshed block, captured on the wire.
func TestAgentflowDriverSeesRefreshedGitFragment(t *testing.T) {
	caller := &captureCaller{answer: "done"}
	sess, root := newRefreshSession(t, caller)
	gitContextTestRun(t, root, "checkout", "-q", "-b", "feature")
	if got := refresh(t, sess); !strings.HasPrefix(got, "git context refreshed: feature,") {
		t.Fatalf("refresh: %q", got)
	}
	plan := &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"out.txt"}}}}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, &fakeAF{}, sess.orch, sess, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: &fakeAF{}, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	if caller.system != sess.baseSystem || !strings.Contains(caller.system, "branch: feature\n") || strings.Count(caller.system, gitContextOpen) != 1 {
		t.Fatalf("driver request after refresh:\n%s", caller.system)
	}
}

// The driver builds the canonical runner and every worker runner through
// agentflowRunnerForRoot, so a non-canonical root here stands in for a worker.
func TestAgentflowRunnerForRootForwardsApprovedNamesToWorkerRoots(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$PWD\" \"${GOLEM_577_APPROVED-unset}\" > \"$GOLEM_577_OUT\"\n"
	if err := os.WriteFile(filepath.Join(bin, "agentflow"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("GOLEM_577_OUT", out)
	t.Setenv("GOLEM_577_APPROVED", "worker-visible")
	workerRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := agentflowRunnerForRoot("", []string{"GOLEM_577_OUT", "GOLEM_577_APPROVED"})(workerRoot)
	if _, stderr, exit, err := r.Run(t.Context(), []string{"status"}, nil); err != nil || exit != 0 {
		t.Fatalf("fake agentflow: exit=%d err=%v stderr=%q", exit, err, stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := workerRoot + "\nworker-visible\n"; string(got) != want {
		t.Fatalf("worker child saw %q, want cwd and approved value %q", got, want)
	}
}

func TestMustAgentflowRunnerPanicsWithoutEchoOnUnvalidatedNames(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil || strings.Contains(fmt.Sprint(r), "SECRET") {
			t.Fatalf("recover = %v, want a value-free panic", r)
		}
	}()
	mustAgentflowRunner(t.TempDir(), "", []string{"NAME=sk-SECRET-577"})
}

// stopTestPlan has one step with one command gate, so a skipped gate shows in
// fakeAF's call sequence.
func stopTestPlan() *agentflow.Plan {
	return &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"out.txt"},
		Validation: []string{"go test"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test"}}},
	}}}
}

// #611: a step run that stops at a loop cap (nil error, non-Completed
// StopReason) fails its attempt before any gate and records it as blocked.
func TestTaskStepRunnerStopReasonsBlockBeforeGates(t *testing.T) {
	read := func(id, path string) agent.ModelResult {
		return toolStep(id, "read_file", `{"path":"`+path+`"}`)
	}
	write := toolStep("w1", "write_file", `{"path":"out.txt","content":"partial\n"}`)
	blocked := func(token string) string { return "block-step:P1:A-P1:golem: agent run stopped: " + token }
	failure := func(token string) string {
		return "step P1 attempt A-P1: agent run stopped: " + token + "; attempt recorded as blocked"
	}
	tests := []struct {
		name      string
		maxSteps  int
		budget    agent.Budget
		responses []agent.ModelResult
		wantSeq   []string
		wantErr   string // empty: the step completes
		zeroCalls bool
	}{
		{
			name: "completed", maxSteps: 4, responses: []agent.ModelResult{write, answerStep("done")},
			wantSeq: []string{"claim:P1", "record-file-change:P1:A-P1:out.txt", "before-gates", "gate:P1:go test", "finish-step:P1:A-P1"},
		},
		{
			name: "step cap", maxSteps: 1, responses: []agent.ModelResult{write},
			wantSeq: []string{"claim:P1", "record-file-change:P1:A-P1:out.txt", blocked("step_cap_reached")},
			wantErr: failure("step_cap_reached"),
		},
		{
			name: "tool errors", maxSteps: 4,
			responses: []agent.ModelResult{read("r1", "missing-a.txt"), read("r2", "missing-b.txt"), read("r3", "missing-c.txt")},
			wantSeq:   []string{"claim:P1", blocked("tool_error_cap_reached")},
			wantErr:   failure("tool_error_cap_reached"),
		},
		{
			name: "repeats", maxSteps: 4,
			responses: []agent.ModelResult{read("r1", "seed.txt"), read("r2", "seed.txt"), read("r3", "seed.txt")},
			wantSeq:   []string{"claim:P1", blocked("repeat_limit_reached")},
			wantErr:   failure("repeat_limit_reached"),
		},
		{
			name: "budget", maxSteps: 4, budget: agent.Budget{InputCeiling: 8192, OutputReserve: 64, TotalTokens: 1},
			responses: []agent.ModelResult{answerStep("never sent")},
			wantSeq:   []string{"claim:P1", blocked("budget_reached")},
			wantErr:   failure("budget_reached"), zeroCalls: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			plan := stopTestPlan()
			caller := &scriptCaller{responses: tt.responses}
			af := &fakeAF{}
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sess := &replSession{maxSteps: tt.maxSteps, startupBudget: tt.budget}
			runStep, err := newTaskStepRunner(root, plan, af, agent.New(caller, agent.ContextManager{}), sess, true, io.Discard, nil, cancel)
			if err != nil {
				t.Fatal(err)
			}
			// The recorder proves a stopped run never reaches the pre-gate hook.
			d := &driver{af: af, plan: plan, runStep: runStep, beforeGates: func(context.Context, agentflow.Step, string) error {
				af.seq = append(af.seq, "before-gates")
				return nil
			}}
			err = d.runOneStep(runCtx, "P1")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("runOneStep: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q (calls %v)", err, tt.wantErr, af.seq)
			}
			if !equalSeq(af.seq, tt.wantSeq) {
				t.Fatalf("agentflow calls = %v, want %v", af.seq, tt.wantSeq)
			}
			if tt.zeroCalls && caller.i != 0 {
				t.Fatalf("model calls = %d, want 0", caller.i)
			}
		})
	}
}

// D6: a failed block names both the stop and the block failure, and still runs
// no gate.
func TestRunAttemptBlockFailureNamesBothCauses(t *testing.T) {
	af := &fakeAF{failAt: map[string]error{"block-step": errors.New("block rejected: A-P1 lease expired")}}
	d := &driver{af: af, plan: stopTestPlan(), runStep: func(context.Context, agentflow.Step, string, string) error {
		return &runStoppedError{reason: agent.ToolErrorCapReached}
	}}
	err := d.runOneStep(context.Background(), "P1")
	want := "step P1 attempt A-P1: agent run stopped: tool_error_cap_reached; record blocked attempt: block rejected: A-P1 lease expired"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if wantSeq := []string{"claim:P1", "block-step:P1:A-P1:golem: agent run stopped: tool_error_cap_reached"}; !equalSeq(af.seq, wantSeq) {
		t.Fatalf("agentflow calls = %v, want %v", af.seq, wantSeq)
	}
}

// #642 A3: pins the contractual block budget constant. Whether blockAttempt
// uses it is checked by assertFreshBlockContext's deadline bounds.
func TestBlockStepTimeoutBudget(t *testing.T) {
	if blockStepTimeout != 30*time.Second {
		t.Fatalf("blockStepTimeout = %v, want 30s", blockStepTimeout)
	}
}

// callerContexts are the states a caller's context can be in when an attempt
// fails: the block must run on a fresh bounded context in each (#642 A3).
var callerContexts = []struct {
	name    string
	ctx     func(t *testing.T) context.Context
	wantErr error
}{
	{"live", func(*testing.T) context.Context { return context.Background() }, nil},
	{"cancelled", func(*testing.T) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}, context.Canceled},
	{"deadline exceeded", func(t *testing.T) context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}, context.DeadlineExceeded},
}

// #642 A1, A3 (T1): every step-run error blocks the attempt with a fixed reason
// for its class, checked in order, on a fresh bounded context whatever state
// the caller's context is in. Raw error text stays in the returned error only.
func TestRunAttemptRunErrorsBlockWithClassReason(t *testing.T) {
	errRecord := errors.New("record rejected")
	tests := []struct {
		name   string
		err    error
		reason string
		as     any // errors.As target for a typed error, or nil
	}{
		{"unreceipted edit", fmt.Errorf("run: %w", &unreceiptedEditError{err: errRecord}), "golem: unreceipted edit aborted the run", new(*unreceiptedEditError)},
		{"unreceipted edit over cancel", &unreceiptedEditError{err: fmt.Errorf("record: %w", context.Canceled)}, "golem: unreceipted edit aborted the run", new(*unreceiptedEditError)},
		{"stopped", &runStoppedError{reason: agent.ToolErrorCapReached}, "golem: agent run stopped: tool_error_cap_reached", new(*runStoppedError)},
		{"cancelled", fmt.Errorf("chat: %w", context.Canceled), "golem: agent run cancelled", nil},
		{"cancel over exhaustion", errors.Join(context.Canceled, agent.ErrContextExhausted), "golem: agent run cancelled", nil},
		{"deadline is not cancel", fmt.Errorf("chat: %w", context.DeadlineExceeded), "golem: agent run failed", nil},
		{"exhausted", fmt.Errorf("compact: %w", agent.ErrContextExhausted), "golem: agent run exhausted its context", nil},
		{"other", errors.New("provider down: token=sk-test-123"), "golem: agent run failed", nil},
	}
	for _, c := range callerContexts {
		for _, tt := range tests {
			t.Run(c.name+"/"+tt.name, func(t *testing.T) {
				ctx := c.ctx(t)
				if ctx.Err() != c.wantErr {
					t.Fatalf("fixture context err = %v, want %v", ctx.Err(), c.wantErr)
				}
				plan := stopTestPlan()
				af := &fakeAF{}
				d := &driver{af: af, plan: plan, root: t.TempDir(), runStep: func(context.Context, agentflow.Step, string, string) error {
					return tt.err
				}}
				err := d.runAttempt(ctx, plan.Steps[0], "A-P1", "goal")
				if want := []string{"block-step:P1:A-P1:" + tt.reason}; !equalSeq(af.seq, want) {
					t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
				}
				if want := "step P1 attempt A-P1: " + tt.err.Error() + "; attempt recorded as blocked"; err == nil || err.Error() != want {
					t.Fatalf("err = %v, want %q", err, want)
				}
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v does not wrap the step error", err)
				}
				if tt.as != nil && !errors.As(err, tt.as) {
					t.Fatalf("err = %v lost its typed cause %T", err, tt.as)
				}
				assertFreshBlockContext(t, af.blocks)
			})
		}
	}
}

// #642 A2, A3 (T1 gate rows): every RunGate error blocks the attempt with the
// one fixed reason, on a fresh bounded context, and runs no later gate. Neither
// the gate outcome nor its label reaches the ledger; the returned error keeps
// both.
func TestRunAttemptGateErrorsBlock(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
		as      any // errors.As target for a typed error, or nil
	}{
		{
			"command error", &agentflow.CommandError{Cmd: "run", Exit: 1, Stderr: "grep: no match"},
			`step P1 attempt A-P1: gate "go test" failed: agentflow run: exit 1: grep: no match; attempt recorded as blocked`,
			new(*agentflow.CommandError),
		},
		{
			"exec error", errors.New("exec: \"agentflow\": executable file not found in $PATH"),
			`step P1 attempt A-P1: gate "go test" failed: exec: "agentflow": executable file not found in $PATH; attempt recorded as blocked`,
			nil,
		},
	}
	for _, c := range callerContexts {
		for _, tt := range tests {
			t.Run(c.name+"/"+tt.name, func(t *testing.T) {
				ctx := c.ctx(t)
				if ctx.Err() != c.wantErr {
					t.Fatalf("fixture context err = %v, want %v", ctx.Err(), c.wantErr)
				}
				// A second gate pins stopping at the first failure: failAt
				// fails every gate, so a later gate run shows in the sequence.
				plan := stopTestPlan()
				plan.Steps[0].Validation = append(plan.Steps[0].Validation, "lint")
				plan.Steps[0].Gates = append(plan.Steps[0].Gates, agentflow.Gate{Kind: "command", Run: []string{"go", "vet"}})
				af := &fakeAF{failAt: map[string]error{"gate": tt.err}}
				d := &driver{af: af, plan: plan, root: t.TempDir(), runStep: func(context.Context, agentflow.Step, string, string) error {
					return nil
				}}
				err := d.runAttempt(ctx, plan.Steps[0], "A-P1", "goal")
				if want := []string{"gate:P1:go test", "block-step:P1:A-P1:golem: gate execution failed"}; !equalSeq(af.seq, want) {
					t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
				}
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v does not wrap the gate error", err)
				}
				if tt.as != nil && !errors.As(err, tt.as) {
					t.Fatalf("err = %v lost its typed cause %T", err, tt.as)
				}
				assertFreshBlockContext(t, af.blocks)
			})
		}
	}
}

// #642 A4, A5 (T2): beforeGates and FinishStep errors keep today's exits, and a
// failed block on any step-run error names both errors.
func TestRunAttemptUnchangedExits(t *testing.T) {
	runOK := func(context.Context, agentflow.Step, string, string) error { return nil }
	t.Run("before gates", func(t *testing.T) {
		errGuard := errors.New("receipt-safety refusal")
		plan := stopTestPlan()
		af := &fakeAF{}
		d := &driver{af: af, plan: plan, runStep: runOK, beforeGates: func(context.Context, agentflow.Step, string) error {
			return errGuard
		}}
		if err := d.runAttempt(context.Background(), plan.Steps[0], "A-P1", "goal"); err != errGuard {
			t.Fatalf("err = %v, want the guard error itself", err)
		}
		if len(af.seq) != 0 {
			t.Fatalf("agentflow calls = %v, want none", af.seq)
		}
	})
	t.Run("finish step", func(t *testing.T) {
		errFinish := errors.New("finish-step rejected")
		plan := stopTestPlan()
		af := &fakeAF{failAt: map[string]error{"finish-step": errFinish}}
		d := &driver{af: af, plan: plan, runStep: runOK}
		if err := d.runAttempt(context.Background(), plan.Steps[0], "A-P1", "goal"); err != errFinish {
			t.Fatalf("err = %v, want the finish-step error itself", err)
		}
		if want := []string{"gate:P1:go test", "finish-step:P1:A-P1"}; !equalSeq(af.seq, want) {
			t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
		}
	})
	t.Run("block failure", func(t *testing.T) {
		providerDown := errors.New("provider down")
		errBlock := errors.New("block rejected")
		plan := stopTestPlan()
		af := &fakeAF{failAt: map[string]error{"block-step": errBlock}}
		d := &driver{af: af, plan: plan, runStep: func(context.Context, agentflow.Step, string, string) error {
			return providerDown
		}}
		err := d.runAttempt(context.Background(), plan.Steps[0], "A-P1", "goal")
		if want := "step P1 attempt A-P1: provider down; record blocked attempt: block rejected"; err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if !errors.Is(err, providerDown) || !errors.Is(err, errBlock) {
			t.Fatalf("err = %v does not wrap both the step and block errors", err)
		}
		if want := []string{"block-step:P1:A-P1:golem: agent run failed"}; !equalSeq(af.seq, want) {
			t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
		}
	})
}

// #642 A1, A7 (T3) through the production runner: a provider failure after an
// edit, and an edit AgentFlow refused to record, both block the attempt. The
// journal cancels the run context on the refused receipt, so that block must
// not run on the run context.
func TestTaskStepRunnerRunErrorsBlockAttempt(t *testing.T) {
	write := toolStep("w1", "write_file", `{"path":"out.txt","content":"partial\n"}`)
	setup := func(t *testing.T, caller agent.ModelCaller, af *fakeAF) (*driver, context.Context) {
		t.Helper()
		plan := stopTestPlan()
		runCtx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		runStep, err := newTaskStepRunner(t.TempDir(), plan, af, agent.New(caller, agent.ContextManager{}), &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
		if err != nil {
			t.Fatal(err)
		}
		return &driver{af: af, plan: plan, runStep: runStep}, runCtx
	}
	t.Run("provider", func(t *testing.T) {
		providerDown := errors.New("provider down")
		af := &fakeAF{}
		caller := &errAfterCaller{inner: &scriptCaller{responses: []agent.ModelResult{write}}, failAt: 1, err: providerDown}
		d, runCtx := setup(t, caller, af)
		err := d.runOneStep(runCtx, "P1")
		if want := []string{"claim:P1", "record-file-change:P1:A-P1:out.txt", "block-step:P1:A-P1:golem: agent run failed"}; !equalSeq(af.seq, want) {
			t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
		}
		if !errors.Is(err, providerDown) || !strings.HasPrefix(err.Error(), "step P1 attempt A-P1: ") || !strings.HasSuffix(err.Error(), "; attempt recorded as blocked") {
			t.Fatalf("err = %v, want the provider error inside the blocked-attempt shape", err)
		}
		assertFreshBlockContext(t, af.blocks)
	})
	t.Run("fatal receipt", func(t *testing.T) {
		errRecord := errors.New("record rejected")
		af := &fakeAF{failAt: map[string]error{"record-file-change": errRecord}}
		d, runCtx := setup(t, &scriptCaller{responses: []agent.ModelResult{write, answerStep("done")}}, af)
		err := d.runOneStep(runCtx, "P1")
		if want := []string{"claim:P1", "record-file-change:P1:A-P1:out.txt", "block-step:P1:A-P1:golem: unreceipted edit aborted the run"}; !equalSeq(af.seq, want) {
			t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
		}
		if want := "step P1 attempt A-P1: unreceipted edit aborted the run: record rejected; attempt recorded as blocked"; err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if runCtx.Err() != context.Canceled {
			t.Fatalf("run context err = %v, want the journal's cancel before the block", runCtx.Err())
		}
		assertFreshBlockContext(t, af.blocks)
	})
}

// #642 A9 (T1 timeout rows): after blocking a failed gate, the error reports a
// timed-out receipt the driver's root retains, since verify-run will reject
// it whatever runs next; a read refusal is appended, never in place of the gate
// or block error, and the one block still runs.
func TestRunAttemptGateTimeoutReportsRetainedReceipt(t *testing.T) {
	const prefix = `step P1 attempt A-P1: gate "go test" failed: agentflow run: exit 124: command timed out after 1 seconds`
	const unreadable = "this workspace holds incompatible AgentFlow state (.agent/command-receipts.jsonl unreadable); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run"
	errBlock := errors.New("block rejected")
	for _, tt := range []struct {
		name       string
		receipts   string // command-receipts.jsonl body; "" means a directory there
		blockErr   error
		wantSuffix func(root string) string
	}{
		{"timeout", timeoutReceiptCR1 + "\n", nil, func(root string) string {
			return "; attempt recorded as blocked; " + wantTimeoutReport(root, "CR1")
		}},
		{"timeout, block fails", timeoutReceiptCR1 + "\n", errBlock, func(root string) string {
			return "; record blocked attempt: block rejected; " + wantTimeoutReport(root, "CR1")
		}},
		{"exit-1 receipt", exitOneReceiptCR1 + "\n", nil, func(string) string {
			return "; attempt recorded as blocked"
		}},
		{"unreadable receipts", "", nil, func(string) string {
			return "; attempt recorded as blocked; read command receipts for the timeout check: " + unreadable + "; run -agentflow-status"
		}},
		{"unreadable receipts, block fails", "", errBlock, func(string) string {
			return "; record blocked attempt: block rejected; read command receipts for the timeout check: " + unreadable + "; run -agentflow-status"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.receipts != "" {
				writeAgentFile(t, root, "command-receipts.jsonl", tt.receipts)
			} else if err := os.MkdirAll(filepath.Join(root, ".agent", "command-receipts.jsonl"), 0o700); err != nil {
				t.Fatal(err)
			}
			gateErr := &agentflow.CommandError{Cmd: "run", Exit: 124, Stderr: "command timed out after 1 seconds"}
			af := &fakeAF{failAt: map[string]error{"gate": gateErr, "block-step": tt.blockErr}}
			plan := stopTestPlan()
			d := &driver{af: af, plan: plan, root: root, runStep: func(context.Context, agentflow.Step, string, string) error {
				return nil
			}}
			err := d.runAttempt(context.Background(), plan.Steps[0], "A-P1", "goal")
			if want := prefix + tt.wantSuffix(root); err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
			var asCommand *agentflow.CommandError
			if !errors.Is(err, gateErr) || !errors.As(err, &asCommand) {
				t.Fatalf("err = %v lost the gate error", err)
			}
			if tt.blockErr != nil && !errors.Is(err, tt.blockErr) {
				t.Fatalf("err = %v lost the block error", err)
			}
			if want := []string{"gate:P1:go test", "block-step:P1:A-P1:golem: gate execution failed"}; !equalSeq(af.seq, want) {
				t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
			}
			assertFreshBlockContext(t, af.blocks)
		})
	}
}

// #642: AgentFlow 1.0's block-step only checks that the attempt was opened,
// and its ledger reader then rejects an event after a terminal one as
// state_invalid. So a failed run or gate whose attempt root's ledger already
// closed (an operator's break-glass fail-step, say) is reported and not
// blocked, nor is an attempt whose ledger cannot be read. A terminal event for
// another attempt or step, and every non-terminal event AgentFlow 1.0 reads
// (execution.py OPEN_EVENTS and LEASE_EVENTS), is no reason to skip the block.
func TestBlockAttemptSkipsClosedAttempt(t *testing.T) {
	const claimed = `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"claimed"}` + "\n"
	event := func(name string) string {
		return `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"` + name + `"}` + "\n"
	}
	rows := []struct {
		name, ledger string
		suffix       string // after the cause; "" means the block runs
	}{
		{"completed", claimed + `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"completed"}` + "\n", "; attempt already closed (completed); not blocked"},
		{"blocked", claimed + `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"blocked"}` + "\n", "; attempt already closed (blocked); not blocked"},
		{"failed", claimed + `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"failed"}` + "\n", "; attempt already closed (failed); not blocked"},
		{"abandoned", claimed + `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P1","event":"abandoned"}` + "\n", "; attempt already closed (abandoned); not blocked"},
		{"control: open attempt", claimed, ""},
		{"control: amendment_started", event("amendment_started"), ""},
		{"control: lease_renewed", claimed + event("lease_renewed"), ""},
		{"control: verified", claimed + event("verified"), ""},
		{"control: in_progress", claimed + event("in_progress"), ""},
		{"control: another attempt failed", claimed + `{"schema_version":"1.0.0","step_id":"P1","attempt_id":"A-P2","event":"failed"}` + "\n", ""},
		// Defence in depth, not a reachable state: attempt ids are global, so
		// AgentFlow rejects this ledger ("attempt ... belongs to multiple
		// steps", porcelain.py); Golem still matches on both ids.
		{"defence in depth: same attempt id failed under another step", claimed + `{"schema_version":"1.0.0","step_id":"P2","attempt_id":"A-P1","event":"failed"}` + "\n", ""},
		{"malformed row", claimed + "{\n", "; check attempt state before blocking: this workspace holds incompatible AgentFlow state (.agent/step-runs.jsonl:2 unreadable); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run; attempt not blocked; run -agentflow-status"},
		// A newer AgentFlow 1.x may add a terminal event; an event Golem does not
		// know fails closed (no block) rather than risk an event after a
		// terminal one.
		{"unknown event", claimed + event("cancelled"), `; check attempt state before blocking: attempt A-P1 has an event this Golem does not know ("cancelled"); attempt not blocked; run -agentflow-status`},
		{"unknown event is escaped", claimed + event(`x\u001b[2J`), `; check attempt state before blocking: attempt A-P1 has an event this Golem does not know ("x\x1b[2J"); attempt not blocked; run -agentflow-status`},
	}
	providerDown, gateDown := errors.New("provider down"), errors.New("gate down")
	paths := []struct {
		name   string
		runErr error
		failAt map[string]error
		cause  error
		prefix string
		before []string // calls before the block
		block  string
	}{
		{"run error", providerDown, nil, providerDown, "step P1 attempt A-P1: provider down", nil, "block-step:P1:A-P1:golem: agent run failed"},
		{"gate error", nil, map[string]error{"gate": gateDown}, gateDown, `step P1 attempt A-P1: gate "go test" failed: gate down`, []string{"gate:P1:go test"}, "block-step:P1:A-P1:golem: gate execution failed"},
	}
	for _, p := range paths {
		for _, tt := range rows {
			t.Run(p.name+"/"+tt.name, func(t *testing.T) {
				root := t.TempDir()
				writeAgentFile(t, root, "step-runs.jsonl", tt.ledger)
				af := &fakeAF{failAt: p.failAt}
				plan := stopTestPlan()
				d := &driver{af: af, plan: plan, root: root, runStep: func(context.Context, agentflow.Step, string, string) error {
					return p.runErr
				}}
				err := d.runAttempt(context.Background(), plan.Steps[0], "A-P1", "goal")
				want, wantSeq := p.prefix+tt.suffix, p.before
				if tt.suffix == "" {
					want, wantSeq = p.prefix+"; attempt recorded as blocked", append(slices.Clone(p.before), p.block)
				}
				if err == nil || err.Error() != want {
					t.Fatalf("err = %v\nwant %s", err, want)
				}
				if !errors.Is(err, p.cause) {
					t.Fatalf("err = %v does not wrap the cause", err)
				}
				if !equalSeq(af.seq, wantSeq) {
					t.Fatalf("agentflow calls = %v, want %v", af.seq, wantSeq)
				}
			})
		}
	}
	// A review amendment as AgentFlow 1.0 writes it (execution.py claim_step,
	// _close_step, amend_step): the step's earlier attempt completed, and the
	// amendment's own failed gate must still block the amendment attempt.
	t.Run("gate error/amendment attempt", func(t *testing.T) {
		root := t.TempDir()
		writeAgentFile(t, root, "step-runs.jsonl", `{"schema_version":"1.0.0","recorded_at":"2026-10-11T10:00:00Z","event":"claimed","step_id":"P1","attempt_id":"A1","agent_id":"golem","lease_expires_at":null}`+"\n"+
			`{"schema_version":"1.0.0","recorded_at":"2026-10-11T10:01:00Z","event":"completed","step_id":"P1","attempt_id":"A1"}`+"\n"+
			`{"schema_version":"1.0.0","recorded_at":"2026-10-11T10:02:00Z","event":"amendment_started","step_id":"P1","attempt_id":"A2","amends_attempt":"A1","amends_completed_at":"2026-10-11T10:01:00Z","agent_id":"golem","reason":"review RR-20261011T100200Z-0123abcd"}`+"\n")
		af := &fakeAF{failAt: map[string]error{"gate": gateDown}}
		plan := stopTestPlan()
		d := &driver{af: af, plan: plan, root: root, runStep: func(context.Context, agentflow.Step, string, string) error {
			return nil
		}}
		err := d.runAttempt(context.Background(), plan.Steps[0], "A2", "goal")
		if want := `step P1 attempt A2: gate "go test" failed: gate down; attempt recorded as blocked`; err == nil || err.Error() != want {
			t.Fatalf("err = %v\nwant %s", err, want)
		}
		if want := []string{"gate:P1:go test", "block-step:P1:A2:golem: gate execution failed"}; !equalSeq(af.seq, want) {
			t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
		}
	})
}
