package agentflow

import (
	"slices"
	"strings"
)

// PlanIR is the ergonomic authoring shape the planner model submits via the
// submit_plan tool. It carries only semantic decisions; Compile supplies every
// rigid contract field. See docs/superpowers/specs/2026-07-09-agentflow-plan-compiler-210-design.md.
type PlanIR struct {
	Objective    string   `json:"objective"`
	Scope        []string `json:"scope"`
	Invariants   []string `json:"invariants"`
	RiskLevel    string   `json:"risk_level"` // low|medium|high
	RollbackPlan string   `json:"rollback_plan"`
	AllowedFiles []string `json:"allowed_files"`
	BlockedFiles []string `json:"blocked_files"` // optional
	NonGoals     []string `json:"non_goals"`     // optional
	Steps        []StepIR `json:"steps"`
}

// StepIR is one authored step. Validations are executable commands (argv), not
// shell strings, so Compile can emit index-aligned validation[]/gates[].
type StepIR struct {
	ID           string   `json:"id"`
	Action       string   `json:"action"`
	Files        []string `json:"files"`
	ExpectedDiff []string `json:"expected_diff"`
	DependsOn    []string `json:"depends_on"` // optional
	Validations  []GateIR `json:"validations"`
}

// GateIR is one executable validation: argv plus an optional human label.
type GateIR struct {
	Label string   `json:"label"` // optional
	Argv  []string `json:"argv"`
}

const agentProofDir = ".agent/"

// Compile is a total, deterministic projection PlanIR -> Plan. It never
// validates intent (CheckPlan owns all local diagnostics) and never returns an
// error: a structurally poor IR still compiles into a Plan that CheckPlan then
// rejects. Every required list field is a non-nil empty slice so the plan
// marshals with all keys present.
func Compile(ir PlanIR) Plan {
	p := Plan{
		SchemaVersion: "0.3.0",
		Objective:     ir.Objective,
		Scope:         cloneStrings(ir.Scope),
		NonGoals:      cloneStrings(ir.NonGoals),
		Invariants:    cloneStrings(ir.Invariants),
		RiskLevel:     ir.RiskLevel,
		DriftBudget:   DriftBudget{UnrelatedEdits: 0, NewDependencies: 0, FormattingDrift: "minimal", ArchitectureDrift: "requires_approval"},
		AllowedFiles:  withAgentDir(cloneStrings(ir.AllowedFiles)),
		BlockedFiles:  cloneStrings(ir.BlockedFiles),
		RollbackPlan:  ir.RollbackPlan,
		EvidenceIDs:   []string{},
		Steps:         make([]Step, 0, len(ir.Steps)),
	}
	seen := map[string]bool{}
	gates := []string{}
	for _, s := range ir.Steps {
		st := Step{
			ID:            s.ID,
			Action:        s.Action,
			Files:         cloneStrings(s.Files),
			Preconditions: []string{},
			ExpectedDiff:  cloneStrings(s.ExpectedDiff),
			Validation:    make([]string, 0, len(s.Validations)),
			EvidenceIDs:   []string{},
		}
		if len(s.DependsOn) > 0 {
			st.DependsOn = cloneStrings(s.DependsOn)
		}
		for _, g := range s.Validations {
			label := strings.TrimSpace(g.Label)
			if label == "" {
				label = strings.Join(g.Argv, " ")
			}
			st.Validation = append(st.Validation, label)
			st.Gates = append(st.Gates, Gate{Kind: "command", Run: cloneStrings(g.Argv)})
			if label != "" && !seen[label] {
				seen[label] = true
				gates = append(gates, label)
			}
		}
		p.Steps = append(p.Steps, st)
	}
	p.ValidationGates = gates
	return p
}

func cloneStrings(xs []string) []string {
	return append([]string{}, xs...)
}

// withAgentDir appends the exact ".agent/" prefix when absent so agentflow's own
// proof-state writes during execution stay in scope for drift accounting even in
// repositories that do not gitignore .agent/.
func withAgentDir(allowed []string) []string {
	if slices.Contains(allowed, agentProofDir) {
		return allowed
	}
	return append(allowed, agentProofDir)
}
