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
