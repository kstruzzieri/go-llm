package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeAF records the ordered sequence of driver->agentflow calls.
type fakeAF struct {
	seq        []string
	nextSteps  []string // ids to hand out, then "" (done)
	i          int
	review     agentflow.ReviewRun
	failAt     map[string]error
	proofError error
}

func (f *fakeAF) failure(name string) error {
	if f.failAt == nil {
		return nil
	}
	return f.failAt[name]
}

func (f *fakeAF) Probe(context.Context) error { f.seq = append(f.seq, "probe"); return nil }
func (f *fakeAF) ProbeReview(context.Context) error {
	f.seq = append(f.seq, "probe-review")
	return f.failure("probe-review")
}
func (f *fakeAF) Init(context.Context) error { f.seq = append(f.seq, "init"); return nil }
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
		"probe", "init", "evidence:E1", "lock:plan.json", "init-exec", "doctor",
		"next-step", "claim:P1", "gate:P1:go test", "finish-step:P1:A-P1",
		"next-step", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
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
		"probe", "probe-review", "init", "lock:plan.json", "init-exec", "doctor", "next-step",
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
