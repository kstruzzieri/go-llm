package agentflow

import (
	"encoding/json"
	"testing"
)

const samplePlan = `{
  "schema_version":"0.3.0",
  "objective":"o",
  "scope":["src"],
  "non_goals":[],
  "invariants":["stay within src"],
  "risk_level":"low",
  "drift_budget":{"unrelated_edits":0,"new_dependencies":0,"formatting_drift":0,"architecture_drift":0},
  "allowed_files":["src/*"],
  "blocked_files":[],
  "evidence_ids":[],
  "validation_gates":["unit"],
  "rollback_plan":"git checkout -- .",
  "steps":[{
    "id":"P1","action":"edit","files":["src/a.go"],"preconditions":[],
    "expected_diff":["+x"],"evidence_ids":[],
    "validation":["go test ./src"],
    "gates":[{"kind":"command","run":["go","test","./src"]}]
  }]
}`

func TestParsePlanAndExtractGates(t *testing.T) {
	var p Plan
	if err := json.Unmarshal([]byte(samplePlan), &p); err != nil {
		t.Fatal(err)
	}
	if p.Steps[0].ID != "P1" || len(p.Steps[0].Gates) != 1 {
		t.Fatalf("parse: %+v", p.Steps[0])
	}
	gates, err := ExtractCommandGates(p.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Label != "go test ./src" {
		t.Fatalf("gates = %+v", gates)
	}
	if got := gates[0].Argv; len(got) != 3 || got[0] != "go" {
		t.Fatalf("argv = %v", got)
	}
}

func TestPreflightP0_RejectsNonCommandGate(t *testing.T) {
	p := Plan{Steps: []Step{{
		ID: "P1", Validation: []string{"manual"},
		Gates: []Gate{{Kind: "inspection"}},
	}}}
	if err := PreflightP0(&p); err == nil {
		t.Fatal("expected preflight to reject an inspection-only step in P0")
	}
}

func TestPreflightP0_RejectsMissingGate(t *testing.T) {
	p := Plan{Steps: []Step{{ID: "P1", Validation: []string{"go test"}}}} // no gates[]
	if err := PreflightP0(&p); err == nil {
		t.Fatal("expected preflight to reject a step with no structured command gate")
	}
}

func TestPreflightP0_RejectsValidationWithoutMatchingCommandGate(t *testing.T) {
	p := Plan{Steps: []Step{{
		ID: "P1", Validation: []string{"unit", "lint"},
		Gates: []Gate{{Kind: "command", Run: []string{"go", "test", "./..."}}},
	}}}
	if err := PreflightP0(&p); err == nil {
		t.Fatal("expected every validation entry to have a matching command gate in P0")
	}
}
