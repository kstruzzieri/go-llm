package agentflow

import (
	"fmt"
	"strings"
)

// Plan is the subset of the agentflow plan document Golem authors/consumes.
// Unknown fields are ignored (agentflow's schema is additive), so this struct
// need not be exhaustive.
type Plan struct {
	SchemaVersion string   `json:"schema_version"`
	Objective     string   `json:"objective"`
	AllowedFiles  []string `json:"allowed_files"`
	BlockedFiles  []string `json:"blocked_files"`
	EvidenceIDs   []string `json:"evidence_ids"`
	Steps         []Step   `json:"steps"`
}

type Step struct {
	ID           string   `json:"id"`
	Action       string   `json:"action"`
	Files        []string `json:"files"`
	ExpectedDiff []string `json:"expected_diff"`
	Validation   []string `json:"validation"`
	Gates        []Gate   `json:"gates"`
}

// Gate is a structured validation gate. kind is "command" or "inspection".
type Gate struct {
	Kind string   `json:"kind"`
	Run  []string `json:"run"`
}

// CommandGate is an executable gate resolved for a step: an argv slice plus the
// label agentflow's verify-step credits (aligned to step.validation by index
// when present, else the joined argv).
type CommandGate struct {
	Label string
	Argv  []string
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
		out = append(out, CommandGate{Label: label, Argv: append([]string(nil), g.Run...)})
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
