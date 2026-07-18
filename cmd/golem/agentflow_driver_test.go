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
}

func (f *fakeAF) failure(name string) error {
	if f.failAt == nil {
		return nil
	}
	return f.failAt[name]
}

func (f *fakeAF) Probe(context.Context) error { f.seq = append(f.seq, "probe"); return nil }
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
	return f.failure("gate")
}
func (f *fakeAF) FinishStep(_ context.Context, id, attempt string) error {
	f.seq = append(f.seq, "finish-step:"+id+":"+attempt)
	return f.failure("finish-step")
}
func (f *fakeAF) FinishRun(context.Context) (string, error) {
	f.seq = append(f.seq, "finish-run")
	if f.proofError != nil {
		return "", f.proofError
	}
	return "proof-pack.md", nil
}
func (f *fakeAF) RecordFileChange(context.Context, string, string, string) error { return nil }
func (f *fakeAF) RecordEvidence(_ context.Context, e agentflow.EvidenceEntry) error {
	f.seq = append(f.seq, "evidence:"+e.ID)
	return nil
}
func (f *fakeAF) NextAction(context.Context) (agentflow.NextActionState, error) {
	return agentflow.NextActionState{}, nil
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
		"next-step", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
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
	want := []string{"probe", "probe-workflow", "init", "lock:plan.json", "init-exec", "doctor", "next-step", "finish-run"}
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
			if _, err := d.run(context.Background()); err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want %q", err, tt.wantText)
			}
			if !equalSeq(af.seq, tt.wantSeq) {
				t.Fatalf("failure sequence = %v, want %v", af.seq, tt.wantSeq)
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
		"record-review:review.json", "amend:P1:" + ref, "gate:P1:test one", "finish-step:P1:AM-P1", "finish-run",
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
	}{
		{"record review command", "record-review", nil},
		{"amend command", "amend-step", nil},
		{"model", "", errors.New("model failed")},
		{"receipt", "", errors.New("unreceipted edit aborted the run")},
		{"gate", "gate", nil},
		{"finish step", "finish-step", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			af := &fakeAF{review: reviewRun(true, activeFinding("RF-1", "P1"))}
			if tt.failAt != "" {
				af.failAt = map[string]error{tt.failAt: errors.New(tt.failAt + " failed")}
			}
			d := &driver{af: af, plan: reviewPlan(), planPath: "plan.json", reviewManifest: "review.json", out: io.Discard,
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
	fresh := agentflow.NextActionState{Resumability: &agentflow.ResumabilityProjection{
		Contract: &agentflow.ResumabilityContract{
			PlanSHA256:              "plan",
			ExecutionContractSHA256: "execution",
			Locked:                  true,
		},
		AgentID: "golem-w1",
	}}
	if err := validateFreshWorkerProjection(fresh, "golem-w1"); err != nil {
		t.Fatalf("fresh projection = %v", err)
	}
	for name, state := range map[string]agentflow.NextActionState{
		"missing projection":       {},
		"missing contract":         {Resumability: &agentflow.ResumabilityProjection{AgentID: "golem-w1"}},
		"unlocked":                 {Resumability: &agentflow.ResumabilityProjection{Contract: &agentflow.ResumabilityContract{PlanSHA256: "plan", ExecutionContractSHA256: "execution"}, AgentID: "golem-w1"}},
		"missing plan digest":      {Resumability: &agentflow.ResumabilityProjection{Contract: &agentflow.ResumabilityContract{Locked: true, ExecutionContractSHA256: "execution"}, AgentID: "golem-w1"}},
		"missing execution digest": {Resumability: &agentflow.ResumabilityProjection{Contract: &agentflow.ResumabilityContract{Locked: true, PlanSHA256: "plan"}, AgentID: "golem-w1"}},
		"other agent":              {Resumability: &agentflow.ResumabilityProjection{Contract: fresh.Resumability.Contract, AgentID: "golem-w2"}},
		"present attempt":          {Resumability: &agentflow.ResumabilityProjection{Contract: fresh.Resumability.Contract, AgentID: "golem-w1", Attempt: &agentflow.ResumabilityAttempt{ID: "A1", Open: false}}},
		"diagnostic":               {Resumability: &agentflow.ResumabilityProjection{Contract: fresh.Resumability.Contract, AgentID: "golem-w1", Diagnostics: []agentflow.ResumabilityDiagnostic{{Code: "state_invalid"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFreshWorkerProjection(state, "golem-w1"); err == nil {
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

func TestRunAgentflowTask_RejectsInvalidTraceabilityBeforeClientUse(t *testing.T) {
	planJSON := `{
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
	planJSON := `{"steps":[{"id":"P1","files":["a.go"],"validation":["go test"],"gates":[{"kind":"command","run":["go","test"]}]}]}`
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
