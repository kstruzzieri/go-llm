package agentflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Plan is the agentflow plan document Golem authors and the #209 runner consumes.
// It is now the full lockable document: every field agentflow's validate_plan
// requires is present. Unknown agentflow fields are still ignored on unmarshal.
type Plan struct {
	SchemaVersion   string            `json:"schema_version"`
	Objective       string            `json:"objective"`
	Scope           []string          `json:"scope"`
	NonGoals        []string          `json:"non_goals"`
	Invariants      []string          `json:"invariants"`
	RiskLevel       string            `json:"risk_level"`
	DriftBudget     DriftBudget       `json:"drift_budget"`
	AllowedFiles    []string          `json:"allowed_files"`
	BlockedFiles    []string          `json:"blocked_files"`
	ValidationGates []string          `json:"validation_gates"`
	RollbackPlan    string            `json:"rollback_plan"`
	EvidenceIDs     []string          `json:"evidence_ids"`
	Requirements    []Requirement     `json:"requirements,omitempty"`
	DesignDecisions *[]DesignDecision `json:"design_decisions,omitempty"`
	Steps           []Step            `json:"steps"`
}

// DesignDecision is an optional locked-plan decision selected by step ID.
type DesignDecision struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	References *[]string `json:"references,omitempty"`
}

// Requirement is an Agentflow requirement in the lockable plan contract.
type Requirement struct {
	ID                 string      `json:"id"`
	Text               string      `json:"text"`
	AcceptanceCriteria []Criterion `json:"acceptance_criteria"`
}

// Criterion is an acceptance criterion traced to steps and proof gates.
type Criterion struct {
	ID     string           `json:"id"`
	Text   string           `json:"text"`
	Review *CriterionReview `json:"review,omitempty"`
}

// CriterionReview declares the minimum Agentflow review depth for a criterion.
type CriterionReview struct {
	MinimumDepth string `json:"minimum_depth"`
}

type Step struct {
	ID                string    `json:"id"`
	Action            string    `json:"action"`
	Files             []string  `json:"files"`
	Preconditions     []string  `json:"preconditions"`
	ExpectedDiff      []string  `json:"expected_diff"`
	Validation        []string  `json:"validation"`
	EvidenceIDs       []string  `json:"evidence_ids"`
	CriterionIDs      []string  `json:"criterion_ids,omitempty"`
	DesignDecisionIDs *[]string `json:"design_decision_ids,omitempty"`
	DependsOn         []string  `json:"depends_on,omitempty"`
	Gates             []Gate    `json:"gates,omitempty"`
}

// UnmarshalJSON preserves optional-list presence and rejects null lists.
func (p *Plan) UnmarshalJSON(data []byte) error {
	if err := rejectNullList(data, "design_decisions"); err != nil {
		return err
	}
	type plain Plan
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Plan(decoded)
	return nil
}

// UnmarshalJSON preserves optional-list presence and rejects null lists.
func (d *DesignDecision) UnmarshalJSON(data []byte) error {
	if err := rejectNullList(data, "references"); err != nil {
		return err
	}
	type plain DesignDecision
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*d = DesignDecision(decoded)
	return nil
}

// UnmarshalJSON preserves optional-list presence and rejects null lists.
func (s *Step) UnmarshalJSON(data []byte) error {
	if err := rejectNullList(data, "design_decision_ids"); err != nil {
		return err
	}
	type plain Step
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = Step(decoded)
	return nil
}

func rejectNullList(data []byte, field string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields[field]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s must be a list", field)
	}
	return nil
}

// DriftBudget is agentflow's drift allowance. validate_plan only checks the four
// keys are present, but agentflow's own artifacts use string severities for the
// last two; we match them so a future type-tightening in agentflow does not break
// us. See src/agentflow/artifacts.py.
type DriftBudget struct {
	UnrelatedEdits    int    `json:"unrelated_edits"`
	NewDependencies   int    `json:"new_dependencies"`
	FormattingDrift   string `json:"formatting_drift"`
	ArchitectureDrift string `json:"architecture_drift"`
}

// Gate is a structured validation gate. kind is "command" or "inspection".
type Gate struct {
	Kind         string   `json:"kind"`
	Run          []string `json:"run"`
	CriterionIDs []string `json:"criterion_ids,omitempty"`
}

// CommandGate is an executable gate resolved for a step: an argv slice plus the
// label agentflow's verify-step credits (aligned to step.validation by index
// when present, else the joined argv). CriterionIDs is the source gate's
// acceptance-criterion mapping, carried here so consumers never index back into
// step.gates, whose positions do not survive the kind=="command" filter.
type CommandGate struct {
	Label        string
	Argv         []string
	CriterionIDs []string
}

// ExtractCommandGates returns the P0-executable gates for a step. Each entry in
// step.gates with kind=="command" contributes one CommandGate; its label is
// step.validation[i] when that index exists, otherwise strings.Join(run, " ").
func ExtractCommandGates(s Step) ([]CommandGate, error) {
	var out []CommandGate
	for i, g := range s.Gates {
		if g.Kind != "command" {
			continue
		}
		if len(g.Run) == 0 {
			return nil, fmt.Errorf("step %s gate %d: command gate has empty run", s.ID, i)
		}
		label := strings.Join(g.Run, " ")
		if i < len(s.Validation) {
			label = s.Validation[i]
		}
		out = append(out, CommandGate{
			Label:        label,
			Argv:         append([]string(nil), g.Run...),
			CriterionIDs: append([]string(nil), g.CriterionIDs...),
		})
	}
	return out, nil
}

// PreflightP0 rejects a plan Golem cannot run in P0: every step must have at
// least one structured command gate, and every validation[] label must have a
// matching gates[] command at the same index. Inspection-only gates and bare
// validation strings are out of scope (Golem does not parse shell strings out of
// validation).
func PreflightP0(p *Plan) error {
	for _, s := range p.Steps {
		if len(s.Validation) > 0 && len(s.Gates) != len(s.Validation) {
			return fmt.Errorf("step %s: P0 requires one gates[] command for each validation[] entry", s.ID)
		}
		for i, g := range s.Gates {
			if g.Kind != "command" {
				return fmt.Errorf("step %s gate %d: P0 supports only command gates", s.ID, i)
			}
		}
		gates, err := ExtractCommandGates(s)
		if err != nil {
			return err
		}
		if len(gates) == 0 {
			return fmt.Errorf("step %s: P0 requires a structured command gate (kind=command with run[]); "+
				"inspection gates and bare validation strings are not supported", s.ID)
		}
	}
	return nil
}
