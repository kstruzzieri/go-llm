package agentflow

import (
	"encoding/json"
	"slices"
	"testing"
)

func sampleIR() PlanIR {
	return PlanIR{
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
