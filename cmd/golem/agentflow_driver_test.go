package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeAF records the ordered sequence of driver->agentflow calls.
type fakeAF struct {
	seq       []string
	nextSteps []string // ids to hand out, then "" (done)
	i         int
}

func (f *fakeAF) Probe(context.Context) error { f.seq = append(f.seq, "probe"); return nil }
func (f *fakeAF) Init(context.Context) error  { f.seq = append(f.seq, "init"); return nil }
func (f *fakeAF) LockPlan(_ context.Context, p string) error {
	f.seq = append(f.seq, "lock:"+p)
	return nil
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
func (f *fakeAF) RunGate(_ context.Context, step, attempt, gate string, argv []string) error {
	f.seq = append(f.seq, "gate:"+step+":"+gate)
	return nil
}
func (f *fakeAF) FinishStep(_ context.Context, id, attempt string) error {
	f.seq = append(f.seq, "finish-step:"+id+":"+attempt)
	return nil
}
func (f *fakeAF) FinishRun(context.Context) (string, error) {
	f.seq = append(f.seq, "finish-run")
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

func TestDriver_HappyPathOrdering(t *testing.T) {
	af := &fakeAF{nextSteps: []string{"P1"}}
	plan := &agentflow.Plan{Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"src/a.go"},
		Validation: []string{"go test"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test"}}},
	}}}
	plan.AllowedFiles = []string{"src/*"}

	// runStep is scripted to "succeed" without a real model.
	runStep := func(ctx context.Context, step agentflow.Step, attempt string) error { return nil }

	d := &driver{
		af: af, plan: plan, planPath: "plan.json", runStep: runStep,
		evidence: []agentflow.EvidenceEntry{{ID: "E1", Claim: "fixture", Source: "evidence.json"}},
	}
	proof, err := d.run(context.Background())
	if err != nil || proof != "proof-pack.md" {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
	want := []string{
		"probe", "init", "evidence:E1", "lock:plan.json", "init-exec", "doctor",
		"next-step", "claim:P1", "gate:P1:go test", "finish-step:P1:A-P1",
		"next-step", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
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
