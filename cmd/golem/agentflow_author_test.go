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

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestCanonicalPlanJSONSHA256MatchesAgentflowPythonEncoding(t *testing.T) {
	for _, test := range []struct {
		name string
		plan string
		want string
	}{
		{
			name: "unicode html and float",
			plan: `{"objective":"café <tag> & 😀","locked":true,"locked_at":"ignored","n":1.0}`,
			want: "e314e84c943429c75d26fff4b97bb835d526e040d218af93d8fc484ec0abce8a",
		},
		{
			name: "delete snow exponent and negative zero integer",
			plan: `{"z":"\u007f雪","a":"</script>","integer":-0,"float":1e-7}`,
			want: "790c58daab38bbcf8a408877a64bf2b85a59130cda8969b75fa5622761575c5d",
		},
		{
			name: "python float overflow",
			plan: `{"future_numeric":1e400}`,
			want: "d2b8da6145542a53502f44c2bc63e3f19439a239bd9cbc948a640f4e15492e83",
		},
		{
			name: "python signed overflow and underflow",
			plan: `{"negative":-1e400,"underflow":1e-4000}`,
			want: "7b0d08326326bb0f3ee375ae981d580b8b64d044fc6b60573650c4a03cae6e48",
		},
		{
			name: "python preserves lone high surrogate",
			plan: `{"s":"\ud800"}`,
			want: "d06a70a1ca4d3ac4099cd5f35ecbb551be652247e0950c05790e8f0c58010851",
		},
		{
			name: "python preserves lone low surrogate",
			plan: `{"s":"\udc00"}`,
			want: "674779badd6e5aec127408f2a0672761c55e78095912ab422b570492277b592c",
		},
		{
			name: "python sorts a lone surrogate key by code point",
			plan: `{"\ud800":1,"x":2}`,
			want: "3cffeb91007ccbb7994a12c098d304a16a6b4ee319a08d42579c86b9f4d1b1f2",
		},
		{
			name: "python accepts nonfinite constants",
			plan: `{"infinity":Infinity,"negative":-Infinity,"not_a_number":NaN}`,
			want: "1f1b4b16529dd96825f8191bba4027096c80c12d6b41333b9bad1e0f8f2c1803",
		},
		{
			name: "escaped lock keys are excluded",
			plan: `{"\u006cocked":true,"locked_at":"ignored","s":"\ud800"}`,
			want: "d06a70a1ca4d3ac4099cd5f35ecbb551be652247e0950c05790e8f0c58010851",
		},
		{
			name: "decoded duplicate keys keep the last value",
			plan: `{"\ud83d\ude00":1,"😀":2}`,
			want: "da025370ab29ccb58a430a14f6a23fa2fc02abe81f1db81624adb73506fe238c",
		},
		{
			name: "keys sort by decoded Python code points",
			plan: `{"\ud800":1,"\ue000":2,"😀":3,"x":4}`,
			want: "d2f69b93836ae0544cf2bbfb7cf89e2b8299cd8b4c7f88d234585eff55ed9571",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalPlanJSONSHA256([]byte(test.plan))
			if err != nil {
				t.Fatal(err)
			}
			// Python: json.dumps(content, sort_keys=True, separators=(",", ":"))
			if got != test.want {
				t.Fatalf("digest = %s, want Agentflow digest %s", got, test.want)
			}
		})
	}
}

func TestCanonicalPlanJSONSHA256RejectsMalformedInput(t *testing.T) {
	for _, plan := range [][]byte{
		[]byte(`[]`),
		[]byte(`{"x":1} trailing`),
		[]byte(`{"x":"\q"}`),
		[]byte(`{"x":01}`),
		{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
	} {
		if digest, err := canonicalPlanJSONSHA256(plan); err == nil {
			t.Fatalf("canonicalPlanJSONSHA256(%q) = %s, want error", plan, digest)
		}
	}
}

func TestDecodeAgentflowPlanJSONRejectsUnexecutableLoneSurrogate(t *testing.T) {
	data := []byte(`{"steps":[{"id":"P1","gates":[{"kind":"command","run":["echo","\ud800"]}]}]}`)
	var plan agentflow.Plan
	if err := decodeAgentflowPlanJSON(data, &plan); err == nil || !strings.Contains(err.Error(), "lone UTF-16 surrogate") {
		t.Fatalf("decodeAgentflowPlanJSON() error = %v, want lone-surrogate refusal", err)
	}

	paired := []byte(`{"steps":[{"id":"P1","gates":[{"kind":"command","run":["echo","\ud83d\ude00"]}]}]}`)
	if err := decodeAgentflowPlanJSON(paired, &plan); err != nil {
		t.Fatal(err)
	}
	if got := plan.Steps[0].Gates[0].Run[1]; got != "😀" {
		t.Fatalf("paired surrogate decoded as %q, want emoji", got)
	}
}

// stubLocker satisfies afLocker; LockPlan returns a scripted error per call.
type stubLocker struct {
	probeErr         error
	workflowProbeErr error
	recommendErr     error
	initErr          error
	lockErrs         []error
	materializeErr   error
	recommendation   agentflow.WorkflowRecommendation
	probes           int
	workflowProbes   int
	recommendations  int
	inits            int
	locks            int
	materializations int
	paths            []string
	briefs           []agentflow.TaskBrief
	selectedProfiles []string
	reasons          []string
	sequence         []string
	probeStarted     chan struct{}
	lockStarted      chan struct{}
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
func (s *stubLocker) ProbeWorkflow(context.Context) error {
	s.workflowProbes++
	return s.workflowProbeErr
}
func (s *stubLocker) RecommendWorkflow(_ context.Context, brief agentflow.TaskBrief, selectedProfile, reason string) (agentflow.WorkflowRecommendation, error) {
	s.recommendations++
	s.sequence = append(s.sequence, "recommend")
	s.briefs = append(s.briefs, brief)
	s.selectedProfiles = append(s.selectedProfiles, selectedProfile)
	s.reasons = append(s.reasons, reason)
	if s.recommendErr != nil {
		return agentflow.WorkflowRecommendation{}, s.recommendErr
	}
	if s.recommendation.SchemaVersion != "" {
		return s.recommendation, nil
	}
	return defaultWorkflowRecommendation(), nil
}
func (s *stubLocker) Init(context.Context) error {
	s.inits++
	s.sequence = append(s.sequence, "init")
	return s.initErr
}
func (s *stubLocker) LockPlan(ctx context.Context, path string) error {
	s.paths = append(s.paths, path)
	i := s.locks
	s.locks++
	s.sequence = append(s.sequence, "lock")
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
func (s *stubLocker) MaterializeWorkflowContract(context.Context, agentflow.WorkflowRecommendation) error {
	s.materializations++
	s.sequence = append(s.sequence, "materialize")
	return s.materializeErr
}

func defaultWorkflowRecommendation() agentflow.WorkflowRecommendation {
	return agentflow.WorkflowRecommendation{
		SchemaVersion: "0.1.0",
		Recommended:   agentflow.WorkflowSelection{Pack: "agentflow-default", Profile: "medium-feature"},
		Selected:      agentflow.WorkflowSelection{Pack: "agentflow-default", Profile: "medium-feature"},
		Signals:       []string{"task_type=feature", "declared_risk=low"},
		Rationale:     "feature work uses the medium route",
		Alternatives: []agentflow.WorkflowAlternative{
			{Profile: "small-bugfix", Relation: "cheaper", Reason: "only if scope contracts"},
			{Profile: "large-feature", Relation: "safer", Reason: "if scope expands"},
		},
		Contract: agentflow.WorkflowContract{
			SchemaVersion: "0.1.0", WorkflowPack: "agentflow-default", WorkflowProfile: "medium-feature",
			SelectedBy: "recommend-workflow", SelectionReason: "feature work uses the medium route",
			RequiredCapabilities: []agentflow.WorkflowCapability{{ID: "implementation", Required: true}},
			ReviewDepth:          "standard",
			ValidationPolicy:     agentflow.WorkflowValidationPolicy{RequiredGates: []string{"unit"}},
			ProofPolicy:          agentflow.WorkflowProofPolicy{HunkAttribution: "observe", RequireReviewRun: false},
		},
	}
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
		TaskType:  "feature",
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
		"Schema\n  \"0.3.0\"\n\n" +
		"Drift budget\n  unrelated_edits: 0\n  new_dependencies: 0\n  formatting_drift: \"minimal\"\n  architecture_drift: \"requires_approval\"\n\n" +
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
	// The list-valued fields render through previewValues, a separate path from
	// previewText: cover DEL and the C1 range (U+009B is CSI on C1-honoring
	// terminals), which json.Marshal would have passed through raw.
	ir.Steps[0].Files = []string{"src/a\u009b.go"}
	ir.Steps[0].ExpectedDiff = []string{"diff\u009btext"}
	ir.Steps[0].Validations[0].Argv = []string{"echo", "x\u009by", "del\x7f"}
	got := renderPlanPreview(agentflow.Compile(ir))
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\r') {
		t.Fatalf("preview contains raw terminal controls: %q", got)
	}
	if strings.ContainsRune(got, '\u009b') || strings.ContainsRune(got, '\x7f') {
		t.Fatalf("preview contains raw C1/DEL controls: %q", got)
	}
	if !strings.Contains(got, `x\u009by`) || !strings.Contains(got, `del\x7f`) {
		t.Fatalf("argv controls were not visibly escaped: %q", got)
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
	if plan.Effect.Approval != agent.ApprovalAlways || !strings.Contains(plan.Preview, "AC-1") || !strings.Contains(plan.Preview, `argv: ["true"]`) ||
		!strings.Contains(plan.Preview, "Workflow recommendation") || !strings.Contains(plan.Preview, `review_depth: "standard"`) {
		t.Fatalf("tool plan = %+v", plan)
	}
	if client.recommendations != 1 || client.inits != 0 || client.locks != 0 || client.materializations != 0 {
		t.Fatalf("preview calls = recommend %d init %d lock %d materialize %d", client.recommendations, client.inits, client.locks, client.materializations)
	}
}

func TestSubmitPlanTool_PreviewsExactTaskSignalsAndOverrideControlSafely(t *testing.T) {
	truth := true
	ir := validTraceableIR()
	ir.SecuritySensitive = &truth
	ir.BlastRadius = "cross_cutting"
	ir.DeclaredSize = "l"
	args, err := json.Marshal(ir)
	if err != nil {
		t.Fatal(err)
	}
	rec := defaultWorkflowRecommendation()
	rec.Selected.Profile = "high-risk"
	rec.Contract.WorkflowProfile = "high-risk"
	rec.Signals = []string{"security_sensitive=true", "candidate_files=1\u009b"}
	rec.Rationale = "security-sensitive\nroute"
	rec.Contract.ReviewDepth = "deep"
	rec.Contract.ProofPolicy.RequireReviewRun = true
	rec.Contract.RequiredCapabilities = append(rec.Contract.RequiredCapabilities, agentflow.WorkflowCapability{ID: "security-review", Required: true})
	rec.Contract.ValidationPolicy.RequiredGates = []string{"unit", "security\x1b[2J"}
	rec.Override = &agentflow.WorkflowOverride{FromProfile: "medium-feature", ToProfile: "high-risk", Reason: "operator requires deep review\r"}
	rec.Contract.SelectedBy = "recommend-workflow --selected-profile"
	rec.Contract.SelectionReason = "Override: medium-feature -> high-risk. operator requires deep review\r"
	client := &stubLocker{recommendation: rec}
	tool := newSubmitPlanTool(&authorSession{client: client, root: "/root", workflowProfile: "high-risk", workflowReason: "operator requires deep review"})

	plan, err := tool.Plan(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`recommended: "agentflow-default" / "medium-feature"`, `selected: "agentflow-default" / "high-risk"`,
		`signals: ["security_sensitive=true", "candidate_files=1\u009b"]`, `rationale: "security-sensitive\nroute"`,
		`review_depth: "deep"`, `require_review_run: true`, `required_capabilities: ["implementation", "security-review"]`,
		`required_gates: ["unit", "security\x1b[2J"]`, `hunk_attribution: "observe"`,
		`selection_reason: "Override: medium-feature -> high-risk. operator requires deep review\r"`, `override_reason: "operator requires deep review\r"`,
		`profile: "small-bugfix"; relation: "cheaper"; reason: "only if scope contracts"`,
	} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("workflow preview missing %q:\n%s", want, plan.Preview)
		}
	}
	if strings.ContainsRune(plan.Preview, '\u009b') || strings.ContainsRune(plan.Preview, '\x1b') || strings.ContainsRune(plan.Preview, '\r') {
		t.Fatalf("workflow preview contains raw terminal controls: %q", plan.Preview)
	}
	if len(client.briefs) != 1 || client.briefs[0].SecuritySensitive == nil || !*client.briefs[0].SecuritySensitive ||
		client.briefs[0].BlastRadius == nil || *client.briefs[0].BlastRadius != "cross_cutting" ||
		client.briefs[0].DeclaredSize == nil || *client.briefs[0].DeclaredSize != "l" {
		t.Fatalf("recommendation did not receive exact optional signals: %+v", client.briefs)
	}
	if client.selectedProfiles[0] != "high-risk" || client.reasons[0] != "operator requires deep review" {
		t.Fatalf("override forwarding = %v / %v", client.selectedProfiles, client.reasons)
	}
}

func TestSubmitPlanTool_PreservesExplicitFalseSecuritySignal(t *testing.T) {
	falsity := false
	ir := validTraceableIR()
	ir.SecuritySensitive = &falsity
	args, _ := json.Marshal(ir)
	client := &stubLocker{}
	tool := newSubmitPlanTool(&authorSession{client: client, root: "/root"})
	if _, err := tool.Plan(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if len(client.briefs) != 1 || client.briefs[0].SecuritySensitive == nil || *client.briefs[0].SecuritySensitive {
		t.Fatalf("explicit false security signal was lost: %+v", client.briefs)
	}
}

func TestSubmitPlanTool_InvalidTaskSignalsFailBeforeRecommendation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*agentflow.PlanIR)
	}{
		{name: "missing task type", mutate: func(ir *agentflow.PlanIR) { ir.TaskType = "" }},
		{name: "unknown task type", mutate: func(ir *agentflow.PlanIR) { ir.TaskType = "chore" }},
		{name: "unknown blast radius", mutate: func(ir *agentflow.PlanIR) { ir.BlastRadius = "global" }},
		{name: "unknown size", mutate: func(ir *agentflow.PlanIR) { ir.DeclaredSize = "xxl" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ir := validTraceableIR()
			tt.mutate(&ir)
			args, _ := json.Marshal(ir)
			client := &stubLocker{}
			plan, err := newSubmitPlanTool(&authorSession{client: client, root: "/root"}).Plan(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Effect.Approval != agent.ApprovalNever || client.recommendations != 0 {
				t.Fatalf("invalid signals reached approval/recommendation: %+v, calls=%d", plan, client.recommendations)
			}
		})
	}
}

func TestSubmitPlanTool_RecommendationFailurePreventsApproval(t *testing.T) {
	client := &stubLocker{recommendErr: errors.New("routing unavailable")}
	plan, err := newSubmitPlanTool(&authorSession{client: client, root: "/root"}).Plan(context.Background(), validIRJSON(t))
	if err == nil || !strings.Contains(err.Error(), "routing unavailable") {
		t.Fatalf("recommendation failure = plan %+v, err %v", plan, err)
	}
	if plan.Effect.Approval != agent.ApprovalNever || client.inits != 0 || client.locks != 0 || client.materializations != 0 {
		t.Fatalf("recommendation failure reached approval/mutation: %+v, client=%+v", plan, client)
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
	root := t.TempDir()
	as := &authorSession{client: client, root: root,
		cancel: func() { canceled = true; cancel() }}
	tool := newSubmitPlanTool(as)
	security := true
	ir := validTraceableIR()
	ir.SecuritySensitive = &security
	ir.BlastRadius = "local"
	ir.DeclaredSize = "s"
	args, err := json.Marshal(ir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Plan(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if as.lockedPath != filepath.Join(root, ".agent", "plan.lock.json") || !canceled || res.IsError {
		t.Errorf("success path: locked=%q canceled=%v res=%+v", as.lockedPath, canceled, res)
	}
	if !strings.HasPrefix(as.taskBriefPath, filepath.Join(root, ".agent", "golem-task-brief-")) {
		t.Fatalf("approved task brief path = %q", as.taskBriefPath)
	}
	if !strings.HasPrefix(as.workflowHandoffPath, filepath.Join(root, ".agent", "golem-workflow-handoff-")) {
		t.Fatalf("approved workflow handoff path = %q", as.workflowHandoffPath)
	}
	b, err := os.ReadFile(as.taskBriefPath)
	if err != nil {
		t.Fatal(err)
	}
	var brief agentflow.TaskBrief
	if err := json.Unmarshal(b, &brief); err != nil || brief.TaskType != "feature" || brief.DeclaredRisk != "low" ||
		brief.SecuritySensitive == nil || !*brief.SecuritySensitive || brief.BlastRadius == nil || *brief.BlastRadius != "local" ||
		brief.DeclaredSize == nil || *brief.DeclaredSize != "s" || brief.CandidateFiles == nil || brief.ValidationNeeds == nil {
		t.Fatalf("saved approved task brief = %+v, err %v", brief, err)
	}
	info, err := os.Stat(as.taskBriefPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("approved task brief mode = %v", info.Mode().Perm())
	}
	handoff, err := os.ReadFile(as.workflowHandoffPath)
	if err != nil {
		t.Fatal(err)
	}
	var handoffEnvelope approvedWorkflowHandoff
	if err := json.Unmarshal(handoff, &handoffEnvelope); err != nil ||
		handoffEnvelope.SchemaVersion != approvedWorkflowHandoffSchemaVersion ||
		handoffEnvelope.PlanSHA256 == "" || handoffEnvelope.TaskBriefSHA256 == "" ||
		handoffEnvelope.Recommendation.Selected.Profile != "medium-feature" {
		t.Fatalf("saved approved workflow handoff = %+v, err %v", handoffEnvelope, err)
	}
	info, err = os.Stat(as.workflowHandoffPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("approved workflow handoff mode = %v", info.Mode().Perm())
	}
	if len(client.paths) != 1 {
		t.Fatalf("LockPlan calls = %d, want 1", len(client.paths))
	}
	if _, err := os.Stat(client.paths[0]); !os.IsNotExist(err) {
		t.Errorf("staged plan was not removed: %v", err)
	}
	if !reflect.DeepEqual(client.sequence, []string{"recommend", "init", "lock", "materialize"}) {
		t.Fatalf("workflow mutation order = %v", client.sequence)
	}
}

func TestSubmitPlanTool_RejectsUnpreviewedOrStaleInvokeWithoutMutation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		preview bool
		stale   bool
	}{
		{name: "unpreviewed"},
		{name: "stale", preview: true, stale: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &stubLocker{}
			as := &authorSession{client: client, root: "/root", cancel: func() {}}
			tool := newSubmitPlanTool(as)
			args := validIRJSON(t)
			if tt.preview {
				if _, err := tool.Plan(context.Background(), args); err != nil {
					t.Fatal(err)
				}
			}
			if tt.stale {
				ir := validTraceableIR()
				ir.Objective = "changed after preview"
				args, _ = json.Marshal(ir)
			}
			res, err := tool.Invoke(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || !strings.Contains(res.Content, "workflow_preview_required") {
				t.Fatalf("stale invoke = %+v", res)
			}
			if client.inits != 0 || client.locks != 0 || client.materializations != 0 {
				t.Fatalf("stale invoke mutated proof state: %+v", client)
			}
		})
	}
}

func TestSubmitPlanTool_MaterializationFailureIsTerminalAfterLock(t *testing.T) {
	canceled := false
	client := &stubLocker{materializeErr: errors.New("contract rejected")}
	as := &authorSession{client: client, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	if _, err := tool.Plan(context.Background(), validIRJSON(t)); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(context.Background(), validIRJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !canceled || as.terminalErr == nil || !strings.Contains(as.terminalErr.Error(), "materialize Agentflow workflow contract") {
		t.Fatalf("materialization failure = res %+v canceled=%v terminal=%v", res, canceled, as.terminalErr)
	}
	if !reflect.DeepEqual(client.sequence, []string{"recommend", "init", "lock", "materialize"}) {
		t.Fatalf("materialization failure sequence = %v", client.sequence)
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
	_, _ = tool.Plan(context.Background(), validIRJSON(t))
	res, _ := tool.Invoke(context.Background(), validIRJSON(t))
	if as.terminalErr == nil || !canceled || !res.IsError {
		t.Errorf("terminal lock error should fail fast; terminalErr=%v canceled=%v", as.terminalErr, canceled)
	}
}

func TestSubmitPlanTool_UnknownStructuredLockErrorFailsClosed(t *testing.T) {
	canceled := false
	term := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "future_error", Message: "unknown contract failure"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{term}}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	_, _ = tool.Plan(context.Background(), validIRJSON(t))
	_, _ = tool.Invoke(context.Background(), validIRJSON(t))
	if as.terminalErr == nil || !canceled {
		t.Fatal("unknown structured lock errors must fail closed")
	}
}

func TestSubmitPlanTool_ContentValidationErrorIsRepairable(t *testing.T) {
	canceled := false
	verr := &agentflow.CommandError{Cmd: "lock-plan", Exit: 1, Errors: []agentflow.StructuredError{{Code: "validation_error", Message: "steps[1].action must be a non-empty string"}}}
	as := &authorSession{client: &stubLocker{lockErrs: []error{verr}}, root: "/root", cancel: func() { canceled = true }}
	tool := newSubmitPlanTool(as)
	_, _ = tool.Plan(context.Background(), validIRJSON(t))
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
	_, _ = tool.Plan(context.Background(), validIRJSON(t))
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
	requires(schema, "task_type", "objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "requirements", "steps")
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
	agentflowSrc := filepath.Join(t.TempDir(), "agentflow source and 'quote")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}

	var out, errb bytes.Buffer
	// runAgentflowAuthorWithClient injects a stub afLocker so no real CLI runs.
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{
		goal: "add healthz", goalSet: true,
		workflowProfile: "high-risk", workflowReason: "operator requires deep review",
		agentflowSrc: agentflowSrc,
	}, root, client, fixedApprover(true))
	if err != nil {
		t.Fatal(err)
	}
	if client.probes != 1 || client.workflowProbes != 1 || client.recommendations != 1 || client.inits != 1 || client.locks != 1 || client.materializations != 1 {
		t.Fatalf("approved plan calls = probe %d workflow-probe %d recommend %d init %d lock %d materialize %d",
			client.probes, client.workflowProbes, client.recommendations, client.inits, client.locks, client.materializations)
	}
	if client.selectedProfiles[0] != "high-risk" || client.reasons[0] != "operator requires deep review" {
		t.Fatalf("author workflow override was not forwarded: %v / %v", client.selectedProfiles, client.reasons)
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
	if !strings.Contains(out.String(), " -task-brief ") || !strings.Contains(out.String(), " -workflow-handoff ") ||
		strings.Contains(out.String(), "-workflow-profile") || strings.Contains(out.String(), "-workflow-reason") {
		t.Errorf("execute-separately command did not bind the approved route handoff:\n%s", out.String())
	}
	if !strings.Contains(out.String(), " -agentflow-src "+shellQuote(agentflowSrc)) {
		t.Errorf("execute-separately command did not preserve source-checkout runtime %s:\n%s", agentflowSrc, out.String())
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
	if client.recommendations != 1 || client.materializations != 0 {
		t.Fatalf("denial route calls = recommend %d materialize %d", client.recommendations, client.materializations)
	}
	// The denied plan is saved so the authoring work is not discarded.
	const marker = "denied plan saved for reference: "
	msg := err.Error()
	idx := strings.LastIndex(msg, marker)
	if idx < 0 {
		t.Fatalf("denial error does not name the saved plan: %q", msg)
	}
	path := msg[idx+len(marker):]
	t.Cleanup(func() { _ = os.Remove(path) })
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var saved agentflow.Plan
	if jsonErr := json.Unmarshal(b, &saved); jsonErr != nil || len(saved.Requirements) == 0 || saved.Requirements[0].ID != "REQ-1" {
		t.Fatalf("saved denied plan is not the compiled plan (err=%v): %s", jsonErr, b)
	}
}

func TestSubmitPlanTool_ResubmissionPreviewShowsDelta(t *testing.T) {
	as := &authorSession{client: &stubLocker{}, root: "/root"}
	tool := newSubmitPlanTool(as)

	first, err := tool.Plan(context.Background(), validIRJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.Preview, "Resubmission") {
		t.Fatalf("first preview must not carry a delta section: %q", first.Preview)
	}

	changed := validTraceableIR()
	changed.Steps[0].Validations[0].Argv = []string{"false"}
	args, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.Plan(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Preview, "Resubmission changes since the previous preview") ||
		!strings.Contains(second.Preview, `  - `+`        argv: ["true"]`) ||
		!strings.Contains(second.Preview, `  + `+`        argv: ["false"]`) {
		t.Fatalf("resubmission preview missing delta:\n%s", second.Preview)
	}

	third, err := tool.Plan(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third.Preview, "identical to the previously previewed plan") {
		t.Fatalf("identical resubmission not flagged:\n%s", third.Preview)
	}
}

func TestAutoPlanApprover_ApprovesOnlySubmitPlan(t *testing.T) {
	var out strings.Builder
	a := &autoPlanApprover{out: &out}
	ok, err := a.Approve(context.Background(), provider.ToolCall{Function: provider.ToolCallFunction{Name: submitPlanToolName}}, "Plan preview\n")
	if err != nil || !ok {
		t.Fatalf("submit_plan auto-approval = %v, %v", ok, err)
	}
	if !strings.Contains(out.String(), "Plan preview") || !strings.Contains(out.String(), "auto-approved via -approve-plan-lock") {
		t.Fatalf("auto-approval must print the preview and provenance:\n%s", out.String())
	}
	ok, err = a.Approve(context.Background(), provider.ToolCall{Function: provider.ToolCallFunction{Name: "run_command"}}, "rm -rf /")
	if err != nil || ok {
		t.Fatalf("non-plan tool must stay denied: %v, %v", ok, err)
	}
}

func TestRunAgentflowAuthor_AutoApproverLocks(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}

	var out bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, client, &autoPlanApprover{out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if client.inits != 1 || client.locks != 1 || client.materializations != 1 {
		t.Fatalf("auto-approved plan should initialize, lock, and materialize once: init=%d lock=%d materialize=%d", client.inits, client.locks, client.materializations)
	}
	if !strings.Contains(out.String(), "auto-approved via -approve-plan-lock") || !strings.Contains(out.String(), "locked approved plan") {
		t.Fatalf("missing auto-approval output:\n%s", out.String())
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
	// The .agent read must have been denied (guard wired), not served. The
	// workspace collapses guard vetoes to the stable scope-denial message so
	// host policy text never reaches model-visible output.
	if !strings.Contains(errb.String(), "path denied by workspace policy") {
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
	if caller.i != 0 || client.workflowProbes != 0 || client.inits != 0 {
		t.Fatalf("probe failure reached workflow probe/model/init: workflow-probe=%d model=%d init=%d", client.workflowProbes, caller.i, client.inits)
	}
}

func TestRunAgentflowAuthor_WorkflowProbeFailsBeforeModel(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{workflowProbeErr: errors.New("recommend-workflow too old")}
	if err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, client, nil); err == nil || !strings.Contains(err.Error(), "workflow routing unavailable") {
		t.Fatalf("workflow probe error = %v", err)
	}
	if client.probes != 1 || client.workflowProbes != 1 || caller.i != 0 || client.recommendations != 0 || client.inits != 0 {
		t.Fatalf("workflow probe failure crossed boundary: client=%+v model=%d", client, caller.i)
	}
}

func TestRunAgentflowAuthor_UsesPlannerPromptAndProjectContext(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "no submission"}
	sess := newTestSession(t, caller, root)
	sess.sysInputs.projectContext = "<<<PROJECT_CONTEXT>>>\nrepo rule\n<<<END_PROJECT_CONTEXT>>>"
	var out, errb bytes.Buffer
	err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("err = %v, want no submission", err)
	}
	if !strings.Contains(caller.system, "Golem's planner") || !strings.Contains(caller.system, "repo rule") ||
		!strings.Contains(caller.system, "acceptance criterion") || !strings.Contains(caller.system, "criterion_ids") || !strings.Contains(caller.system, "task_type") {
		t.Fatalf("planner system omitted prompt/context: %q", caller.system)
	}
}

func TestPlannerBudgetAlignsTurnBudgetWithRouterAdmission(t *testing.T) {
	tests := []struct {
		name        string
		budget      agent.Budget
		options     provider.ModelOptions
		wantReserve int
		wantCeiling int
	}{
		// Zero reserve: the ceiling must drop by NumPredict minus the
		// implicit 2048 the derivation already reserved, or long planner
		// sessions land in an ErrBudgetAdaptationRequired band.
		{name: "zero reserve lowers derived ceiling by planner delta",
			budget: agent.Budget{InputCeiling: 30_720}, wantCeiling: 29_268},
		{name: "zero reserve lowers default ceiling by planner delta",
			wantCeiling: 6_740},
		{name: "zero reserve with larger caller NumPredict",
			budget: agent.Budget{InputCeiling: 30_720}, options: provider.ModelOptions{NumPredict: 8_192}, wantCeiling: 24_576},
		{name: "degenerate delta keeps ceiling positive",
			budget: agent.Budget{InputCeiling: 1_024}, options: provider.ModelOptions{NumPredict: 8_192}, wantCeiling: 1},
		{name: "small reserve floored, ceiling untouched",
			budget: agent.Budget{InputCeiling: 32_768, OutputReserve: 1_024}, wantReserve: minPlannerOutput, wantCeiling: 32_768},
		{name: "large reserve kept, ceiling untouched",
			budget: agent.Budget{InputCeiling: 32_768, OutputReserve: 8_192}, wantReserve: 8_192, wantCeiling: 32_768},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// config.UseCasePlanning is what the author call site passes; its
			// DefaultExpectedOutput is the same 2048 chat fallback the old
			// hard-coded "agent" produced, so every expectation above is the
			// pre-parameterization value -- the proof this change moved
			// nothing for the production path.
			got := plannerBudget(tt.budget, plannerModelOptions(tt.options), config.UseCasePlanning)
			if got.OutputReserve != tt.wantReserve || got.InputCeiling != tt.wantCeiling {
				t.Fatalf("plannerBudget = {ceiling %d, reserve %d}, want {ceiling %d, reserve %d}",
					got.InputCeiling, got.OutputReserve, tt.wantCeiling, tt.wantReserve)
			}
		})
	}
}

func TestPlannerBudget_UsesTheGivenUseCaseDefault(t *testing.T) {
	// The probe use case is "reasoning", not "planning": planning and agent
	// both fall back to chat's 2048 default today, so a planning-vs-agent
	// comparison could not fail whichever literal the implementation
	// hard-coded. "reasoning" is 4096 in defaultExpectedOutputs, which is
	// what makes a hard-coded use case detectable at all.
	opts := provider.ModelOptions{NumPredict: minPlannerOutput}
	base := agent.Budget{InputCeiling: 100_000}

	if got := plannerBudget(base, opts, "agent"); got.InputCeiling != 100_000-(minPlannerOutput-2_048) {
		t.Errorf("agent: InputCeiling = %d, want %d", got.InputCeiling, 100_000-(minPlannerOutput-2_048))
	}
	// reasoning's 4096 exceeds the planner's 3500 NumPredict, so the delta is
	// negative and the ceiling must stay untouched.
	if got := plannerBudget(base, opts, "reasoning"); got.InputCeiling != 100_000 {
		t.Errorf("reasoning: InputCeiling = %d, want the ceiling untouched (100000)", got.InputCeiling)
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

func TestRunAgentflowAuthor_FastApproverInterruptIsInterrupted(t *testing.T) {
	// A Ctrl-C during the plan-lock approval itself: the editor returns
	// errInterrupted, replApprover maps it to context.Canceled, and the author
	// must classify that as errPlannerInterrupted -- not approval-denied (the
	// deny path requires a nil error) and never the raw editor sentinel.
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	approver := newReplApprover(&stubAnswerSource{err: errInterrupted}, io.Discard, false)

	err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, approver)
	if !errors.Is(err, errPlannerInterrupted) {
		t.Fatalf("fast approval interrupt = %v, want errPlannerInterrupted", err)
	}
	if errors.Is(err, errPlannerApprovalDenied) || errors.Is(err, errInterrupted) {
		t.Fatalf("fast approval interrupt misclassified: %v", err)
	}
}
