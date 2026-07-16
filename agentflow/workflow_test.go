package agentflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const validRecommendationJSON = `{
  "schema_version":"0.1.0",
  "recommended":{"pack":"agentflow-default","profile":"small-bugfix"},
  "selected":{"pack":"agentflow-default","profile":"small-bugfix"},
  "signals":["task_type=bugfix","declared_risk=low"],
  "rationale":"Recommended small-bugfix: bounded low-risk fix.",
  "alternatives":[
    {"profile":"docs-only","relation":"cheaper","reason":"documentation only"},
    {"profile":"medium-feature","relation":"safer","reason":"bounded new behavior"}
  ],
  "override":null,
  "workflow_contract_candidate":{
    "schema_version":"0.1.0",
    "workflow_pack":"agentflow-default",
    "workflow_profile":"small-bugfix",
    "selected_by":"recommend-workflow",
    "selection_reason":"Recommended small-bugfix: bounded low-risk fix.",
    "required_capabilities":[],
    "review_depth":"light",
    "validation_policy":{"required_gates":["unit-tests"]},
    "proof_policy":{"hunk_attribution":"enforce","require_review_run":false}
  }
}`

func validOverrideRecommendationJSON() string {
	overridden := strings.Replace(validRecommendationJSON,
		`"selected":{"pack":"agentflow-default","profile":"small-bugfix"}`,
		`"selected":{"pack":"agentflow-default","profile":"medium-feature"}`,
		1)
	overridden = strings.Replace(overridden, `"override":null`,
		`"override":{"from_profile":"small-bugfix","to_profile":"medium-feature","reason":"shared API"}`, 1)
	overridden = strings.Replace(overridden, `"workflow_profile":"small-bugfix"`, `"workflow_profile":"medium-feature"`, 1)
	overridden = strings.Replace(overridden, `"selected_by":"recommend-workflow"`, `"selected_by":"recommend-workflow --selected-profile"`, 1)
	return strings.Replace(overridden,
		`"selection_reason":"Recommended small-bugfix: bounded low-risk fix."`,
		`"selection_reason":"Override: small-bugfix -> medium-feature. shared API"`, 1)
}

func TestClient_RecommendWorkflow_UsesStdinJSONAndParsesTypedProjection(t *testing.T) {
	c, runner := newTestClient(map[string]fakeReply{
		"recommend-workflow": {stdout: []byte(validRecommendationJSON)},
	})
	security := false
	files := []string{"a.go", "a_test.go"}
	blast := "local"
	size := "s"
	validations := []string{"unit-tests"}
	brief := TaskBrief{
		SchemaVersion: TaskBriefSchemaVersion,
		TaskType:      "bugfix", DeclaredRisk: "low", SecuritySensitive: &security,
		CandidateFiles: &files, BlastRadius: &blast, ValidationNeeds: &validations,
		DeclaredSize: &size,
	}

	recommendation, err := c.RecommendWorkflow(context.Background(), brief, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if recommendation.Recommended.Profile != "small-bugfix" ||
		recommendation.Selected.Profile != "small-bugfix" ||
		recommendation.Contract.ReviewDepth != "light" ||
		recommendation.Contract.ProofPolicy.RequireReviewRun {
		t.Fatalf("recommendation = %+v", recommendation)
	}
	if want := []string{"recommend-workflow", "--stdin", "--json"}; !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("argv = %v, want %v", runner.calls[0], want)
	}
	var sent TaskBrief
	if err := json.Unmarshal(runner.inputs[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.CandidateFiles == nil || !reflect.DeepEqual(*sent.CandidateFiles, files) ||
		sent.ValidationNeeds == nil || !reflect.DeepEqual(*sent.ValidationNeeds, validations) {
		t.Fatalf("stdin brief = %+v", sent)
	}
	var candidate map[string]any
	if err := json.Unmarshal(recommendation.CandidateJSON(), &candidate); err != nil {
		t.Fatal(err)
	}
	if candidate["workflow_profile"] != "small-bugfix" {
		t.Fatalf("candidate = %v", candidate)
	}
}

func TestClient_RecommendWorkflow_ForwardsExplicitSelectionAndReason(t *testing.T) {
	c, runner := newTestClient(map[string]fakeReply{"recommend-workflow": {stdout: []byte(validOverrideRecommendationJSON())}})

	got, err := c.RecommendWorkflow(context.Background(), TaskBrief{
		SchemaVersion: TaskBriefSchemaVersion, TaskType: "bugfix", DeclaredRisk: "low",
	}, "medium-feature", "shared API")
	if err != nil {
		t.Fatal(err)
	}
	if got.Override == nil || got.Override.Reason != "shared API" {
		t.Fatalf("override = %+v", got.Override)
	}
	want := []string{"recommend-workflow", "--stdin", "--json", "--selected-profile", "medium-feature", "--reason", "shared API"}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("argv = %v, want %v", runner.calls[0], want)
	}
}

func TestWorkflowRecommendation_JSONHandoffRoundTripsAndVerifiesMaterializedContract(t *testing.T) {
	recommendation, err := parseWorkflowRecommendation([]byte(validRecommendationJSON))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(recommendation, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"workflow_contract_candidate"`)) || !bytes.Contains(b, []byte(`"selected"`)) {
		t.Fatalf("handoff JSON omitted Agentflow fields:\n%s", b)
	}
	var restored WorkflowRecommendation
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	materialized := recommendation.CandidateJSON()
	var gotCandidate, wantCandidate any
	if err := json.Unmarshal(restored.CandidateJSON(), &gotCandidate); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(recommendation.CandidateJSON(), &wantCandidate); err != nil {
		t.Fatal(err)
	}
	restored.candidateJSON = nil
	recommendation.candidateJSON = nil
	if !reflect.DeepEqual(restored, recommendation) || !reflect.DeepEqual(gotCandidate, wantCandidate) {
		t.Fatalf("round trip changed recommendation:\n got: %+v\nwant: %+v", restored, recommendation)
	}
	if err := restored.VerifyMaterializedWorkflowContract(materialized); err != nil {
		t.Fatalf("matching materialized contract: %v", err)
	}
	tampered := bytes.Replace(materialized, []byte(`"review_depth":"light"`), []byte(`"review_depth":"deep"`), 1)
	if err := restored.VerifyMaterializedWorkflowContract(tampered); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered materialized contract error = %v", err)
	}
}

// A recommendation with an empty alternatives list is accepted by
// parseWorkflowRecommendation, so MarshalJSON must round-trip it too: a nil
// slice re-serialized to null would be rejected as a missing required field
// and would fail the durable handoff write after proof state was mutated.
func TestWorkflowRecommendation_EmptyAlternativesRoundTrips(t *testing.T) {
	empty := strings.Replace(validRecommendationJSON,
		`"alternatives":[
    {"profile":"docs-only","relation":"cheaper","reason":"documentation only"},
    {"profile":"medium-feature","relation":"safer","reason":"bounded new behavior"}
  ]`,
		`"alternatives":[]`, 1)
	recommendation, err := parseWorkflowRecommendation([]byte(empty))
	if err != nil {
		t.Fatalf("parse empty alternatives: %v", err)
	}
	b, err := json.Marshal(recommendation)
	if err != nil {
		t.Fatalf("marshal empty-alternatives recommendation: %v", err)
	}
	if !bytes.Contains(b, []byte(`"alternatives":[]`)) {
		t.Fatalf("empty alternatives must serialize as [] not null:\n%s", b)
	}
	var restored WorkflowRecommendation
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatalf("re-parse marshaled empty-alternatives handoff: %v", err)
	}
	if len(restored.Alternatives) != 0 {
		t.Fatalf("alternatives = %+v, want empty", restored.Alternatives)
	}
}

func TestClient_RecommendWorkflow_RejectsMalformedOrInconsistentSuccess(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		want string
	}{
		{"missing report version", func(s string) string { return strings.Replace(s, `"schema_version":"0.1.0",`, "", 1) }, "schema_version"},
		{"unknown report version", func(s string) string {
			return strings.Replace(s, `"schema_version":"0.1.0"`, `"schema_version":"0.2.0"`, 1)
		}, "schema_version"},
		{"unknown selected profile", func(s string) string {
			return strings.Replace(s,
				`"selected":{"pack":"agentflow-default","profile":"small-bugfix"}`,
				`"selected":{"pack":"agentflow-default","profile":"tiny"}`, 1)
		}, "selected.profile"},
		{"silent selection change", func(s string) string {
			return strings.Replace(s, `"selected":{"pack":"agentflow-default","profile":"small-bugfix"}`, `"selected":{"pack":"agentflow-default","profile":"medium-feature"}`, 1)
		}, "override"},
		{"candidate selection mismatch", func(s string) string {
			return strings.Replace(s, `"workflow_profile":"small-bugfix"`, `"workflow_profile":"medium-feature"`, 1)
		}, "workflow_profile"},
		{"candidate automatic selector mismatch", func(s string) string {
			return strings.Replace(s, `"selected_by":"recommend-workflow"`, `"selected_by":"recommend-workflow --selected-profile"`, 1)
		}, "selected_by"},
		{"candidate automatic reason mismatch", func(s string) string {
			return strings.Replace(s, `"selection_reason":"Recommended small-bugfix: bounded low-risk fix."`, `"selection_reason":"different reason"`, 1)
		}, "selection_reason"},
		{"candidate override selector mismatch", func(string) string {
			return strings.Replace(validOverrideRecommendationJSON(), `"selected_by":"recommend-workflow --selected-profile"`, `"selected_by":"recommend-workflow"`, 1)
		}, "selected_by"},
		{"candidate override reason mismatch", func(string) string {
			return strings.Replace(validOverrideRecommendationJSON(), `"selection_reason":"Override: small-bugfix -> medium-feature. shared API"`, `"selection_reason":"different reason"`, 1)
		}, "selection_reason"},
		{"unknown review depth", func(s string) string {
			return strings.Replace(s, `"review_depth":"light"`, `"review_depth":"maximum"`, 1)
		}, "review_depth"},
		{"missing review run policy", func(s string) string { return strings.Replace(s, `,"require_review_run":false`, "", 1) }, "require_review_run"},
		{"unknown candidate field", func(s string) string {
			return strings.Replace(s, `"review_depth":"light",`, `"review_depth":"light","provider":"x",`, 1)
		}, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(map[string]fakeReply{"recommend-workflow": {stdout: []byte(tt.edit(validRecommendationJSON))}})
			_, err := c.RecommendWorkflow(context.Background(), TaskBrief{
				SchemaVersion: TaskBriefSchemaVersion, TaskType: "bugfix", DeclaredRisk: "low",
			}, "", "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

type workflowStagingRunner struct {
	recommendation []byte
	calls          [][]string
	staged         []byte
	stagedMode     os.FileMode
	stagedPath     string
}

func (r *workflowStagingRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "recommend-workflow":
		return r.recommendation, nil, 0, nil
	case "workflow-contract":
		r.stagedPath = args[len(args)-1]
		info, err := os.Stat(r.stagedPath)
		if err != nil {
			return nil, nil, 0, err
		}
		r.stagedMode = info.Mode().Perm()
		r.staged, err = os.ReadFile(r.stagedPath)
		return []byte("wrote .agent/workflow.contract.json\n"), nil, 0, err
	default:
		return nil, nil, 0, nil
	}
}

func TestClient_MaterializeWorkflowContract_StagesExactCandidateOnceAndRemovesIt(t *testing.T) {
	runner := &workflowStagingRunner{recommendation: []byte(validRecommendationJSON)}
	c := NewClient(runner, t.TempDir())
	recommendation, err := c.RecommendWorkflow(context.Background(), TaskBrief{
		SchemaVersion: TaskBriefSchemaVersion, TaskType: "bugfix", DeclaredRisk: "low",
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.MaterializeWorkflowContract(context.Background(), recommendation); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %v, want recommendation plus one materialization", runner.calls)
	}
	wantArgv := []string{"workflow-contract", "--root", c.root, "--from-json", runner.stagedPath}
	if !reflect.DeepEqual(runner.calls[1], wantArgv) {
		t.Fatalf("argv = %v, want %v", runner.calls[1], wantArgv)
	}
	if runner.stagedMode != 0o600 {
		t.Fatalf("staged mode = %o, want 600", runner.stagedMode)
	}
	if !reflect.DeepEqual(runner.staged, recommendation.CandidateJSON()) {
		t.Fatalf("staged candidate changed:\n%s\nwant:\n%s", runner.staged, recommendation.CandidateJSON())
	}
	if _, err := os.Stat(runner.stagedPath); !os.IsNotExist(err) {
		t.Fatalf("temporary candidate still exists: %v", err)
	}
}
