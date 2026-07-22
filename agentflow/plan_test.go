package agentflow

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

const samplePlan = `{
  "schema_version":"0.3.0",
  "objective":"o",
  "scope":["src"],
  "non_goals":[],
  "invariants":["stay within src"],
  "risk_level":"low",
  "drift_budget":{"unrelated_edits":0,"new_dependencies":0,"formatting_drift":"minimal","architecture_drift":"requires_approval"},
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

func TestPlan_MarshalsAllRequiredKeys(t *testing.T) {
	// A zero-content Plan (as Compile would build for an empty-but-initialized
	// plan) must still emit every required list key as [] and drift as strings,
	// or agentflow's validate_plan rejects it with "missing required field".
	p := Plan{
		SchemaVersion:   "0.3.0",
		Scope:           []string{},
		NonGoals:        []string{},
		Invariants:      []string{},
		AllowedFiles:    []string{},
		BlockedFiles:    []string{},
		ValidationGates: []string{},
		EvidenceIDs:     []string{},
		DriftBudget:     DriftBudget{FormattingDrift: "minimal", ArchitectureDrift: "requires_approval"},
		Steps:           []Step{},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"schema_version", "objective", "scope", "non_goals", "invariants",
		"allowed_files", "blocked_files", "validation_gates", "rollback_plan",
		"risk_level", "drift_budget", "steps", "evidence_ids",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("required key %q missing from marshaled plan", k)
		}
	}
	// blocked_files must be [] not null.
	if string(m["blocked_files"]) != "[]" {
		t.Errorf("blocked_files = %s, want []", m["blocked_files"])
	}
	if string(m["drift_budget"]) == "" || !bytes.Contains(b, []byte(`"formatting_drift":"minimal"`)) {
		t.Errorf("drift_budget not string-valued: %s", b)
	}
}

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

func TestExtractCommandGates_CarriesCriterionIDsAcrossKindFilter(t *testing.T) {
	step := Step{
		ID:         "P1",
		Validation: []string{"inspect", "unit"},
		Gates: []Gate{
			{Kind: "inspection", CriterionIDs: []string{"AC-INSPECT"}},
			{Kind: "command", Run: []string{"go", "test", "./..."}, CriterionIDs: []string{"AC-1", "AC-2"}},
		},
	}
	gates, err := ExtractCommandGates(step)
	if err != nil {
		t.Fatal(err)
	}
	if len(gates) != 1 || gates[0].Label != "unit" {
		t.Fatalf("gates = %+v, want the single command gate labeled by its original index", gates)
	}
	if got := gates[0].CriterionIDs; len(got) != 2 || got[0] != "AC-1" || got[1] != "AC-2" {
		t.Fatalf("criterion ids = %v, want the command gate's own mapping", got)
	}
	gates[0].CriterionIDs[0] = "mutated"
	if step.Gates[1].CriterionIDs[0] != "AC-1" {
		t.Fatal("extracted criterion ids alias the step's gate slice")
	}
}

func TestPlan_RequirementTraceabilityRoundTrip(t *testing.T) {
	p := Compile(PlanIR{
		Objective: "o", Scope: []string{"src"}, Invariants: []string{"i"}, RiskLevel: "low",
		RollbackPlan: "git checkout -- .", AllowedFiles: []string{"src/*"},
		Requirements: []RequirementIR{{
			ID: "REQ-1", Text: "behavior",
			AcceptanceCriteria: []CriterionIR{{ID: "AC-1", Text: "verified"}},
		}},
		Steps: []StepIR{{
			ID: "S1", Action: "do", Files: []string{"src/a.go"}, ExpectedDiff: []string{"x"},
			CriterionIDs: []string{"AC-1"},
			Validations:  []GateIR{{Argv: []string{"true"}, CriterionIDs: []string{"AC-1"}}},
		}},
	})
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var got Plan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Requirements[0].AcceptanceCriteria[0].ID != "AC-1" || got.Steps[0].Gates[0].CriterionIDs[0] != "AC-1" {
		t.Fatalf("round trip lost traceability: %+v", got)
	}
}

func TestPlan_DesignDecisionTraceabilityRoundTrip(t *testing.T) {
	var plan Plan
	if err := json.Unmarshal([]byte(`{
		"schema_version":"0.4.0",
		"design_decisions":[
			{"id":"DD-2","text":"second","references":["ADR-2","ADR-1"]},
			{"id":"DD-1","text":"first"},
			{"id":"DD-3","text":"third","references":[]}
		],
		"steps":[{"id":"P1","design_decision_ids":["DD-1","DD-2"]}]
	}`), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.DesignDecisions == nil || len(*plan.DesignDecisions) != 3 {
		t.Fatalf("design decisions = %#v", plan.DesignDecisions)
	}
	decisions := *plan.DesignDecisions
	if decisions[0].ID != "DD-2" || decisions[1].ID != "DD-1" || decisions[2].ID != "DD-3" {
		t.Fatalf("declaration order changed: %+v", decisions)
	}
	if decisions[0].References == nil || !slices.Equal(*decisions[0].References, []string{"ADR-2", "ADR-1"}) {
		t.Fatalf("reference order changed: %#v", decisions[0].References)
	}
	if decisions[1].References != nil {
		t.Fatalf("omitted references became present: %#v", decisions[1].References)
	}
	if decisions[2].References == nil || len(*decisions[2].References) != 0 {
		t.Fatalf("explicit empty references were not preserved: %#v", decisions[2].References)
	}
	if plan.Steps[0].DesignDecisionIDs == nil || !slices.Equal(*plan.Steps[0].DesignDecisionIDs, []string{"DD-1", "DD-2"}) {
		t.Fatalf("step selection order changed: %#v", plan.Steps[0].DesignDecisionIDs)
	}

	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		DesignDecisions []struct {
			References json.RawMessage `json:"references"`
		} `json:"design_decisions"`
	}
	if err := json.Unmarshal(b, &encoded); err != nil {
		t.Fatal(err)
	}
	if encoded.DesignDecisions[1].References != nil || string(encoded.DesignDecisions[2].References) != "[]" {
		t.Fatalf("round trip lost omitted/empty references distinction: %s", b)
	}
}

func TestPlan_RejectsMalformedDesignDecisionLists(t *testing.T) {
	for name, input := range map[string]string{
		"null declarations":   `{"design_decisions":null}`,
		"object declarations": `{"design_decisions":{}}`,
		"null references":     `{"design_decisions":[{"id":"DD-1","text":"x","references":null}]}`,
		"object references":   `{"design_decisions":[{"id":"DD-1","text":"x","references":{}}]}`,
		"null selections":     `{"steps":[{"design_decision_ids":null}]}`,
		"object selections":   `{"steps":[{"design_decision_ids":{}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var plan Plan
			if err := json.Unmarshal([]byte(input), &plan); err == nil {
				t.Fatal("expected malformed design list rejection")
			}
		})
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
