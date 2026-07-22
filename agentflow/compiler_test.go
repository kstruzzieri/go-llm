package agentflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func sampleIR() PlanIR {
	return PlanIR{
		TaskType:     "feature",
		Objective:    "add a health endpoint",
		Scope:        []string{"internal/http"},
		Invariants:   []string{"no new deps"},
		RiskLevel:    "low",
		RollbackPlan: "git checkout -- .",
		AllowedFiles: []string{"internal/http/*"},
		Steps: []StepIR{{
			ID:           "S1",
			Action:       "add GET /healthz handler",
			Files:        []string{"internal/http/health.go"},
			ExpectedDiff: []string{"new health.go with a handler"},
			Validations:  []GateIR{{Label: "unit", Argv: []string{"go", "test", "./internal/http/..."}}},
		}},
	}
}

func TestTaskBriefFromPlan_UsesExactCompiledFacts(t *testing.T) {
	ir := sampleIR()
	ir.RiskLevel = "medium"
	ir.Steps[0].Files = []string{"internal/http/health.go", "internal/http/shared.go"}
	ir.Steps[0].Validations = append(ir.Steps[0].Validations,
		GateIR{Label: "integration", Argv: []string{"go", "test", "./integration/..."}},
	)
	ir.Steps = append(ir.Steps, StepIR{
		ID:           "S2",
		Action:       "reuse shared helper",
		Files:        []string{"internal/http/shared.go"},
		ExpectedDiff: []string{"shared helper used by handler"},
		Validations: []GateIR{
			{Label: "unit", Argv: []string{"go", "test", "./internal/http/..."}},
			{Label: "race", Argv: []string{"go", "test", "-race", "./internal/http/..."}},
		},
	})

	brief := TaskBriefFromPlan(Compile(ir), "feature")
	if brief.SchemaVersion != TaskBriefSchemaVersion || brief.TaskType != "feature" || brief.DeclaredRisk != "medium" {
		t.Fatalf("task brief identity = %+v", brief)
	}
	if brief.CandidateFiles == nil || !slices.Equal(*brief.CandidateFiles, []string{"internal/http/health.go", "internal/http/shared.go"}) {
		t.Fatalf("candidate files = %v, want first-seen exact union", brief.CandidateFiles)
	}
	if brief.ValidationNeeds == nil || !slices.Equal(*brief.ValidationNeeds, []string{"unit", "integration", "race"}) {
		t.Fatalf("validation needs = %v, want compiled deduplicated gates", brief.ValidationNeeds)
	}
	if brief.SecuritySensitive != nil || brief.BlastRadius != nil || brief.DeclaredSize != nil {
		t.Fatalf("unknown optional signals must stay absent: %+v", brief)
	}

	empty := TaskBriefFromPlan(Plan{RiskLevel: "low"}, "bugfix")
	if empty.CandidateFiles != nil || empty.ValidationNeeds != nil {
		t.Fatalf("empty optional facts must be omitted: %+v", empty)
	}
}

func TestCompile_FillsContractBoilerplate(t *testing.T) {
	p := Compile(sampleIR())

	if p.SchemaVersion != "0.3.0" {
		t.Errorf("schema_version = %q", p.SchemaVersion)
	}
	if p.DriftBudget != (DriftBudget{0, 0, "minimal", "requires_approval"}) {
		t.Errorf("drift = %+v", p.DriftBudget)
	}
	// .agent/ auto-covered in allowed_files.
	if !slices.Contains(p.AllowedFiles, ".agent/") {
		t.Errorf("allowed_files missing .agent/: %v", p.AllowedFiles)
	}
	// evidence + preconditions default to non-nil empty.
	if p.EvidenceIDs == nil || p.Steps[0].Preconditions == nil || p.Steps[0].EvidenceIDs == nil {
		t.Errorf("nil slices leaked into compiled plan")
	}
	// index-aligned validation/gates from argv.
	s := p.Steps[0]
	if len(s.Validation) != 1 || s.Validation[0] != "unit" {
		t.Errorf("validation = %v", s.Validation)
	}
	if len(s.Gates) != 1 || s.Gates[0].Kind != "command" ||
		len(s.Gates[0].Run) != 3 || s.Gates[0].Run[0] != "go" {
		t.Errorf("gates = %+v", s.Gates)
	}
	// top-level validation_gates = union of labels.
	if len(p.ValidationGates) != 1 || p.ValidationGates[0] != "unit" {
		t.Errorf("validation_gates = %v", p.ValidationGates)
	}
}

func TestCompile_ProjectsRequirementTraceability(t *testing.T) {
	ir := sampleIR()
	ir.Requirements = []RequirementIR{{
		ID:   "REQ-1",
		Text: "The service exposes a health endpoint.",
		AcceptanceCriteria: []CriterionIR{{
			ID:   "AC-1",
			Text: "GET /healthz returns success.",
		}},
	}}
	ir.Steps[0].CriterionIDs = []string{"AC-1"}
	ir.Steps[0].Validations[0].CriterionIDs = []string{"AC-1"}

	p := Compile(ir)
	if len(p.Requirements) != 1 || p.Requirements[0].ID != "REQ-1" ||
		len(p.Requirements[0].AcceptanceCriteria) != 1 || p.Requirements[0].AcceptanceCriteria[0].ID != "AC-1" {
		t.Fatalf("requirements = %+v", p.Requirements)
	}
	if !slices.Equal(p.Steps[0].CriterionIDs, []string{"AC-1"}) {
		t.Errorf("step criterion_ids = %v", p.Steps[0].CriterionIDs)
	}
	if !slices.Equal(p.Steps[0].Gates[0].CriterionIDs, []string{"AC-1"}) {
		t.Errorf("gate criterion_ids = %v", p.Steps[0].Gates[0].CriterionIDs)
	}
}

func TestCheckPlan_Traceability(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*PlanIR)
		want    []string
		notWant []string
	}{
		{name: "duplicate ids", mutate: func(ir *PlanIR) {
			ir.Requirements = append(ir.Requirements, ir.Requirements[0])
		}, want: []string{"duplicate_requirement_id", "duplicate_criterion_id"}},
		{name: "dangling step criterion", mutate: func(ir *PlanIR) {
			ir.Steps[0].CriterionIDs = []string{"AC-MISSING"}
		}, want: []string{"dangling_step_criterion"}},
		{name: "gate criterion outside parent step", mutate: func(ir *PlanIR) {
			ir.Requirements[0].AcceptanceCriteria = append(ir.Requirements[0].AcceptanceCriteria, CriterionIR{ID: "AC-2", Text: "health is tested"})
			ir.Steps[0].Validations[0].CriterionIDs = []string{"AC-2"}
		}, want: []string{"gate_criterion_not_in_step"}},
		{name: "criterion without implementing step", mutate: func(ir *PlanIR) {
			ir.Steps[0].CriterionIDs = nil
			ir.Steps[0].Validations[0].CriterionIDs = nil
		}, want: []string{"unmapped_criterion_step"}},
		{name: "criterion without verification", mutate: func(ir *PlanIR) {
			ir.Steps[0].Validations[0].CriterionIDs = nil
		}, want: []string{"unmapped_criterion_verification"}},
		{name: "review-backed criterion", mutate: func(ir *PlanIR) {
			ir.Steps[0].Validations[0].CriterionIDs = nil
			ir.Requirements[0].AcceptanceCriteria[0].Review = &CriterionReview{MinimumDepth: "spec_quality"}
		}},
		{name: "malformed traceability", mutate: func(ir *PlanIR) {
			ir.Requirements = []RequirementIR{
				{ID: "REQ 1", Text: " ", AcceptanceCriteria: nil},
				{ID: "REQ-2", Text: "health endpoint", AcceptanceCriteria: []CriterionIR{{ID: "1AC", Text: " ", Review: &CriterionReview{MinimumDepth: "standard"}}}},
			}
			ir.Steps[0].CriterionIDs = []string{"1AC"}
			ir.Steps[0].Validations[0].CriterionIDs = nil
		}, want: []string{"invalid_requirement_id", "missing_requirement_text", "missing_acceptance_criteria", "invalid_criterion_id", "missing_criterion_text", "bad_review_depth"},
			// An invalid depth is one authoring mistake: bad_review_depth alone
			// reports it, without a second unmapped-verification finding.
			notWant: []string{"unmapped_criterion_verification"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ir := sampleTraceableIR()
			tt.mutate(&ir)
			codes := diagnosticCodes(CheckPlan(Compile(ir)))
			if len(tt.want) == 0 && len(codes) != 0 {
				t.Fatalf("unexpected diagnostic codes: %v", codes)
			}
			for _, want := range tt.want {
				if !slices.Contains(codes, want) {
					t.Errorf("diagnostic codes %v missing %s", codes, want)
				}
			}
			for _, notWant := range tt.notWant {
				if slices.Contains(codes, notWant) {
					t.Errorf("diagnostic codes %v must not include %s", codes, notWant)
				}
			}
		})
	}
}

func sampleTraceableIR() PlanIR {
	ir := sampleIR()
	ir.Requirements = []RequirementIR{{ID: "REQ-1", Text: "health endpoint", AcceptanceCriteria: []CriterionIR{{ID: "AC-1", Text: "health succeeds"}}}}
	ir.Steps[0].CriterionIDs = []string{"AC-1"}
	ir.Steps[0].Validations[0].CriterionIDs = []string{"AC-1"}
	return ir
}

func diagnosticCodes(ds []Diagnostic) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

func TestCompile_LabelDefaultsToJoinedArgv(t *testing.T) {
	ir := sampleIR()
	ir.Steps[0].Validations[0].Label = ""
	p := Compile(ir)
	if p.Steps[0].Validation[0] != "go test ./internal/http/..." {
		t.Errorf("label = %q", p.Steps[0].Validation[0])
	}
}

func TestCompile_NilSlicesMarshalAsEmpty(t *testing.T) {
	p := Compile(PlanIR{Objective: "x", RiskLevel: "low"}) // no steps, no slices
	b, _ := json.Marshal(p)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"scope", "non_goals", "invariants", "blocked_files", "validation_gates", "evidence_ids", "steps"} {
		if string(m[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, m[k])
		}
	}
	// allowed_files gets .agent/ even with no authored entries.
	if string(m["allowed_files"]) != `[".agent/"]` {
		t.Errorf("allowed_files = %s", m["allowed_files"])
	}
}

func TestCompile_LegacyPlanOmitsTraceability(t *testing.T) {
	p := Compile(sampleIR())
	if ds := CheckPlan(p); len(ds) != 0 {
		t.Fatalf("legacy plan diagnostics: %+v", ds)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"requirements"`) || strings.Contains(string(b), `"criterion_ids"`) {
		t.Fatalf("legacy plan gained traceability fields: %s", b)
	}
}

func TestCompile_DoesNotAliasIR(t *testing.T) {
	ir := sampleIR()
	p := Compile(ir)
	p.Scope[0] = "changed"
	p.Steps[0].Files[0] = "changed.go"
	p.Steps[0].Gates[0].Run[0] = "changed"
	if ir.Scope[0] == "changed" || ir.Steps[0].Files[0] == "changed.go" ||
		ir.Steps[0].Validations[0].Argv[0] == "changed" {
		t.Fatal("Compile must not alias PlanIR slices")
	}
}

func TestCompile_CanonicalizesStepFilesAndPreservesDAG(t *testing.T) {
	ir := sampleIR()
	ir.AllowedFiles = []string{"src/*"}
	ir.Steps = []StepIR{
		{ID: "S1", Action: "first", Files: []string{"tmp/../src/a.go"}, ExpectedDiff: []string{"a"}, DependsOn: []string{"S2"}, Validations: []GateIR{{Argv: []string{"true"}}}},
		{ID: "S2", Action: "second", Files: []string{"src/b.go"}, ExpectedDiff: []string{"b"}, Validations: []GateIR{{Argv: []string{"true"}}}},
		{ID: "S3", Action: "independent", Files: []string{"src/c.go"}, ExpectedDiff: []string{"c"}, Validations: []GateIR{{Argv: []string{"true"}}}},
	}

	p := Compile(ir)
	if p.Steps[0].Files[0] != "src/a.go" {
		t.Fatalf("step file = %q, want canonical src/a.go", p.Steps[0].Files[0])
	}
	if !slices.Equal(p.Steps[0].DependsOn, []string{"S2"}) || len(p.Steps[2].DependsOn) != 0 {
		t.Fatalf("DAG edges not preserved: %+v", p.Steps)
	}
	if ds := CheckPlan(p); len(ds) != 0 {
		t.Fatalf("valid forward-reference DAG rejected: %v", ds)
	}
	allowed, _ := EffectiveScope(&p, "S1")
	if !slices.Equal(allowed, []string{"src/a.go"}) {
		t.Fatalf("effective scope = %v, want canonical step path", allowed)
	}
}

func codes(ds []Diagnostic) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

func hasCode(ds []Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestCheckPlan_CleanPlanHasNoDiagnostics(t *testing.T) {
	if ds := CheckPlan(Compile(sampleIR())); len(ds) != 0 {
		t.Errorf("clean plan produced diagnostics: %v", codes(ds))
	}
}

func TestTraceabilityDiagnostics_DesignDecisionSchemaGatePrecedesFieldDiagnostics(t *testing.T) {
	emptyDecisions := []DesignDecision{}
	emptySelections := []string{}
	for _, plan := range []Plan{
		{SchemaVersion: "0.3.0", DesignDecisions: &emptyDecisions},
		{SchemaVersion: "0.3.0", Steps: []Step{{ID: "P1", DesignDecisionIDs: &emptySelections}}},
		{SchemaVersion: "not-a-version", DesignDecisions: &emptyDecisions},
	} {
		ds := TraceabilityDiagnostics(plan)
		if len(ds) != 1 || ds[0].Code != "design_decision_schema" {
			t.Fatalf("diagnostics = %+v, want schema gate only", ds)
		}
	}
}

func TestTraceabilityDiagnostics_RejectsInvalidDesignDecisionDeclarations(t *testing.T) {
	valid := DesignDecision{ID: "DD-1", Text: "keep the existing ledger"}
	blankReferences := []string{"ADR-1", "   "}
	tests := []struct {
		name      string
		decisions []DesignDecision
		wantCode  string
	}{
		{name: "empty list", decisions: []DesignDecision{}, wantCode: "missing_design_decisions"},
		{name: "empty id", decisions: []DesignDecision{{Text: "x"}}, wantCode: "invalid_design_decision_id"},
		{name: "invalid id", decisions: []DesignDecision{{ID: "1DD", Text: "x"}}, wantCode: "invalid_design_decision_id"},
		{name: "duplicate id", decisions: []DesignDecision{valid, valid}, wantCode: "duplicate_design_decision_id"},
		{name: "blank text", decisions: []DesignDecision{{ID: "DD-1", Text: "  "}}, wantCode: "missing_design_decision_text"},
		{name: "blank reference", decisions: []DesignDecision{{ID: "DD-1", Text: "x", References: &blankReferences}}, wantCode: "blank_design_decision_reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Plan{SchemaVersion: "0.4.0", DesignDecisions: &tt.decisions}
			if ds := TraceabilityDiagnostics(plan); !hasCode(ds, tt.wantCode) {
				t.Fatalf("diagnostics = %+v, want %s", ds, tt.wantCode)
			}
		})
	}
}

func TestTraceabilityDiagnostics_RejectsInvalidStepDesignDecisionIDs(t *testing.T) {
	decisions := []DesignDecision{{ID: "DD-1", Text: "keep the existing ledger"}}
	tests := []struct {
		name     string
		ids      []string
		wantCode string
	}{
		{name: "empty", ids: []string{}, wantCode: "empty_step_design_decisions"},
		{name: "blank", ids: []string{"  "}, wantCode: "blank_step_design_decision"},
		{name: "duplicate", ids: []string{"DD-1", "DD-1"}, wantCode: "duplicate_step_design_decision"},
		{name: "dangling", ids: []string{"DD-MISSING"}, wantCode: "dangling_step_design_decision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Plan{
				SchemaVersion: "0.4.0", DesignDecisions: &decisions,
				Steps: []Step{{ID: "P1", DesignDecisionIDs: &tt.ids}},
			}
			if ds := TraceabilityDiagnostics(plan); !hasCode(ds, tt.wantCode) {
				t.Fatalf("diagnostics = %+v, want %s", ds, tt.wantCode)
			}
		})
	}
}

func TestTraceabilityDiagnostics_AllowsUnselectedDecisionsAndDuplicateOpaqueReferences(t *testing.T) {
	references := []string{"ADR-1", "ADR-1"}
	decisions := []DesignDecision{
		{ID: "DD-1", Text: "selected", References: &references},
		{ID: "DD-UNSELECTED", Text: "may remain unselected"},
	}
	ids := []string{"DD-1"}
	plan := Plan{
		SchemaVersion: "0.4.0", DesignDecisions: &decisions,
		Steps: []Step{{ID: "P1", DesignDecisionIDs: &ids}},
	}
	if ds := TraceabilityDiagnostics(plan); len(ds) != 0 {
		t.Fatalf("valid design traceability produced diagnostics: %+v", ds)
	}
}

func TestCheckPlan_EmptySteps(t *testing.T) {
	ir := sampleIR()
	ir.Steps = nil
	if ds := CheckPlan(Compile(ir)); !hasCode(ds, "no_steps") {
		t.Errorf("want no_steps, got %v", codes(ds))
	}
}

func TestCheckPlan_MissingSemantics(t *testing.T) {
	ir := sampleIR()
	ir.Scope = []string{"   "}
	ir.Invariants = []string{""}
	ir.AllowedFiles = []string{".agent/", ".agent/"}
	ir.RiskLevel = "extreme"
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "missing_scope") || !hasCode(ds, "missing_invariants") ||
		!hasCode(ds, "missing_allowed_files") || !hasCode(ds, "bad_risk_level") {
		t.Errorf("got %v", codes(ds))
	}
}

func TestCheckPlan_UnusableStep(t *testing.T) {
	ir := sampleIR()
	ir.Steps[0].ExpectedDiff = nil // present-but-empty after compile
	ir.Steps[0].Action = "   "
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "empty_expected_diff") || !hasCode(ds, "empty_action") {
		t.Errorf("got %v", codes(ds))
	}
}

func TestCheckPlan_BlankExpectedDiffEntry(t *testing.T) {
	ir := sampleIR()
	ir.Steps[0].ExpectedDiff = []string{"real change", "   "}
	if ds := CheckPlan(Compile(ir)); !hasCode(ds, "blank_expected_diff_entry") {
		t.Errorf("want blank_expected_diff_entry, got %v", codes(ds))
	}
}

func TestCheckPlan_DuplicateAndUnknownDeps(t *testing.T) {
	ir := sampleIR()
	ir.Steps = []StepIR{
		{ID: "S1", Action: "a", Files: []string{"internal/http/a.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"NOPE"}},
		{ID: "S1", Action: "b", Files: []string{"internal/http/b.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}},
	}
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "duplicate_step_id") || !hasCode(ds, "unknown_dependency") {
		t.Errorf("got %v", codes(ds))
	}
}

func TestCheckPlan_Cycle(t *testing.T) {
	ir := sampleIR()
	ir.AllowedFiles = []string{"a/*"}
	ir.Steps = []StepIR{
		{ID: "A", Action: "a", Files: []string{"a/x.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"B"}},
		{ID: "B", Action: "b", Files: []string{"a/y.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"A"}},
	}
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "dependency_cycle") {
		t.Fatalf("want dependency_cycle, got %v", codes(ds))
	}
}

func TestCheckPlan_CycleMessageRendersClosedPath(t *testing.T) {
	ir := sampleIR()
	ir.AllowedFiles = []string{"a/*"}
	ir.Steps = []StepIR{
		{ID: "A", Action: "a", Files: []string{"a/x.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"B"}},
		{ID: "B", Action: "b", Files: []string{"a/y.go"}, ExpectedDiff: []string{"x"}, Validations: []GateIR{{Argv: []string{"true"}}}, DependsOn: []string{"A"}},
	}
	var msg string
	for _, d := range CheckPlan(Compile(ir)) {
		if d.Code == "dependency_cycle" {
			msg = d.Message
			break
		}
	}
	if msg == "" {
		t.Fatal("no dependency_cycle diagnostic produced")
	}
	// Accept either the A- or B-rooted ordering the sorted DFS produces, but the
	// rendered path must name both nodes and close on the node it started from.
	if !strings.Contains(msg, "-> A") || !strings.Contains(msg, "-> B") {
		t.Errorf("cycle message must name both nodes: %q", msg)
	}
	i := strings.Index(msg, ": ")
	path := strings.Split(msg[i+2:], " -> ")
	if len(path) < 3 || path[0] != path[len(path)-1] {
		t.Errorf("cycle path must close on its root: %q", msg)
	}
}

func TestCheckPlan_EmptyDependency(t *testing.T) {
	ir := sampleIR()
	ir.Steps[0].DependsOn = []string{"   "}
	if ds := CheckPlan(Compile(ir)); !hasCode(ds, "empty_dependency") {
		t.Errorf("want empty_dependency, got %v", codes(ds))
	}
}

func TestCheckPlan_PathSafetyAndScope(t *testing.T) {
	ir := sampleIR()
	ir.AllowedFiles = []string{"internal/http/*"}
	ir.Steps[0].Files = []string{"../secrets.txt", "/etc/passwd", "other/pkg.go", ".agent/state.json"}
	ir.BlockedFiles = []string{".agent/"}
	ir.AllowedFiles = append(ir.AllowedFiles, "../outside/*")
	ds := CheckPlan(Compile(ir))
	// ../ and / -> traversal/absolute; .agent/state.json -> proof-state target;
	// other/pkg.go -> not covered by allowed_files; .agent/ in blocked_files ->
	// blocked_files_covers_proof_state. The unsafe allowed_files entry is rejected too.
	for _, want := range []string{"unsafe_path", "step_targets_proof_state", "file_out_of_scope", "blocked_files_covers_proof_state"} {
		if !hasCode(ds, want) {
			t.Errorf("missing %q; got %v", want, codes(ds))
		}
	}
}

func TestCheckPlan_BlankScopeAndInvariantEntries(t *testing.T) {
	// A non-empty list with a blank entry passes anyNonBlank but is rejected by
	// validate_plan's _is_non_empty_string_list; flag it locally instead.
	ir := sampleIR()
	ir.Scope = []string{"internal/http", ""}
	ir.Invariants = []string{"no new deps", "   "}
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "blank_scope_entry") || !hasCode(ds, "blank_invariant_entry") {
		t.Errorf("want blank_scope_entry + blank_invariant_entry, got %v", codes(ds))
	}
	// The missing_* codes must not also fire: these lists are non-empty.
	if hasCode(ds, "missing_scope") || hasCode(ds, "missing_invariants") {
		t.Errorf("non-empty lists must not report missing_*, got %v", codes(ds))
	}
}

func TestCheckPlan_ProofStateTargetSurvivesDotSlash(t *testing.T) {
	// "./.agent/x" has raw first segment "." — path.Clean must collapse it so the
	// proof-state pre-check still fires.
	ir := sampleIR()
	ir.AllowedFiles = []string{"internal/http/*"}
	ir.Steps[0].Files = []string{"./.agent/state.json"}
	if ds := CheckPlan(Compile(ir)); !hasCode(ds, "step_targets_proof_state") {
		t.Errorf("want step_targets_proof_state for ./.agent/x, got %v", codes(ds))
	}
	// An in-bounds "..": ".agent/../src/x" cleans to "src/x" — real target is src/,
	// not proof state, so it must NOT be flagged as proof state (nor unsafe).
	ir2 := sampleIR()
	ir2.AllowedFiles = []string{"src/*"}
	ir2.Scope = []string{"src"}
	ir2.Steps[0].Files = []string{".agent/../src/x.go"}
	ds2 := CheckPlan(Compile(ir2))
	if hasCode(ds2, "step_targets_proof_state") || hasCode(ds2, "unsafe_path") || hasCode(ds2, "file_out_of_scope") {
		t.Errorf(".agent/../src/x.go cleans to src/x.go and must be clean, got %v", codes(ds2))
	}
}

func TestCheckPlan_GateShape(t *testing.T) {
	ir := sampleIR()
	ir.Steps[0].Validations = []GateIR{{Label: "empty", Argv: nil}}
	ds := CheckPlan(Compile(ir))
	if !hasCode(ds, "empty_gate_argv") {
		t.Errorf("want empty_gate_argv, got %v", codes(ds))
	}
	// A step with no validations at all.
	ir2 := sampleIR()
	ir2.Steps[0].Validations = nil
	if ds := CheckPlan(Compile(ir2)); !hasCode(ds, "no_validation") {
		t.Errorf("want no_validation, got %v", codes(ds))
	}
}
