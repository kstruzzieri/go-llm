package agentflow

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// PlanIR is the ergonomic authoring shape the planner model submits via the
// submit_plan tool. It carries only semantic decisions; Compile supplies every
// rigid contract field. See docs/superpowers/specs/2026-07-09-agentflow-plan-compiler-210-design.md.
type PlanIR struct {
	TaskType     string   `json:"task_type"` // docs|bugfix|feature|refactor
	Objective    string   `json:"objective"`
	Scope        []string `json:"scope"`
	Invariants   []string `json:"invariants"`
	RiskLevel    string   `json:"risk_level"` // low|medium|high
	RollbackPlan string   `json:"rollback_plan"`
	AllowedFiles []string `json:"allowed_files"`
	BlockedFiles []string `json:"blocked_files"` // optional
	NonGoals     []string `json:"non_goals"`     // optional
	// Optional task signals are forwarded only when the author explicitly sets
	// them. Golem does not infer routing policy from plan content.
	SecuritySensitive *bool           `json:"security_sensitive,omitempty"`
	BlastRadius       string          `json:"blast_radius,omitempty"`  // isolated|local|cross_cutting
	DeclaredSize      string          `json:"declared_size,omitempty"` // xs|s|m|l|xl
	Requirements      []RequirementIR `json:"requirements"`            // optional
	Steps             []StepIR        `json:"steps"`
}

// RequirementIR is one authored requirement and its acceptance criteria.
type RequirementIR struct {
	ID                 string        `json:"id"`
	Text               string        `json:"text"`
	AcceptanceCriteria []CriterionIR `json:"acceptance_criteria"`
}

// CriterionIR is one authored acceptance criterion and optional review floor.
type CriterionIR struct {
	ID     string           `json:"id"`
	Text   string           `json:"text"`
	Review *CriterionReview `json:"review,omitempty"`
}

// StepIR is one authored step. Validations are executable commands (argv), not
// shell strings, so Compile can emit index-aligned validation[]/gates[].
type StepIR struct {
	ID           string   `json:"id"`
	Action       string   `json:"action"`
	Files        []string `json:"files"`
	ExpectedDiff []string `json:"expected_diff"`
	DependsOn    []string `json:"depends_on"`    // optional
	CriterionIDs []string `json:"criterion_ids"` // optional
	Validations  []GateIR `json:"validations"`
}

// GateIR is one executable validation: argv plus an optional human label.
type GateIR struct {
	Label        string   `json:"label"` // optional
	Argv         []string `json:"argv"`
	CriterionIDs []string `json:"criterion_ids"` // optional
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
		Requirements:  compileRequirements(ir.Requirements),
		Steps:         make([]Step, 0, len(ir.Steps)),
	}
	seen := map[string]bool{}
	gates := []string{}
	for _, s := range ir.Steps {
		st := Step{
			ID:            s.ID,
			Action:        s.Action,
			Files:         cleanPaths(s.Files),
			Preconditions: []string{},
			ExpectedDiff:  cloneStrings(s.ExpectedDiff),
			Validation:    make([]string, 0, len(s.Validations)),
			EvidenceIDs:   []string{},
			CriterionIDs:  cloneStrings(s.CriterionIDs),
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
			st.Gates = append(st.Gates, Gate{Kind: "command", Run: cloneStrings(g.Argv), CriterionIDs: cloneStrings(g.CriterionIDs)})
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

func compileRequirements(in []RequirementIR) []Requirement {
	if len(in) == 0 {
		return nil
	}
	out := make([]Requirement, 0, len(in))
	for _, r := range in {
		req := Requirement{ID: r.ID, Text: r.Text, AcceptanceCriteria: make([]Criterion, 0, len(r.AcceptanceCriteria))}
		for _, c := range r.AcceptanceCriteria {
			criterion := Criterion{ID: c.ID, Text: c.Text}
			if c.Review != nil {
				review := *c.Review
				criterion.Review = &review
			}
			req.AcceptanceCriteria = append(req.AcceptanceCriteria, criterion)
		}
		out = append(out, req)
	}
	return out
}

func cloneStrings(xs []string) []string {
	return append([]string{}, xs...)
}

func cleanPaths(xs []string) []string {
	out := cloneStrings(xs)
	for i, x := range out {
		if strings.TrimSpace(x) != "" {
			out[i] = path.Clean(x)
		}
	}
	return out
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

// Diagnostic is one machine-readable local pre-check finding. Code is stable and
// testable; Message is operator/model-facing.
type Diagnostic struct {
	Code    string
	Message string
}

// CheckPlan runs Golem's narrow local pre-check over a compiled plan. It is NOT a
// mirror of agentflow's validate_plan: it only reports classes Golem can explain
// better than the CLI (semantic gaps, dependency errors, cycles) or must enforce
// because the CLI defers them to execution (step-file scope, proof-state safety).
// All findings are collected, not fail-first.
func CheckPlan(p Plan) []Diagnostic {
	var ds []Diagnostic
	add := func(code, msg string) { ds = append(ds, Diagnostic{Code: code, Message: msg}) }

	// 1. Non-empty steps (the CLI accepts an empty list).
	if len(p.Steps) == 0 {
		add("no_steps", "plan has no steps; a plan must contain at least one executable step")
	}

	// 2. Required semantic content.
	if strings.TrimSpace(p.Objective) == "" {
		add("missing_objective", "objective is empty")
	}
	if !anyNonBlank(p.Scope) {
		add("missing_scope", "scope is empty; name at least one area the plan touches")
	} else if !allNonBlank(p.Scope) {
		// validate_plan's _is_non_empty_string_list rejects a blank entry outright,
		// so flag it here: a mixed ["real",""] passes anyNonBlank but would be
		// rejected at lock-plan, burning one of the two submissions.
		add("blank_scope_entry", "scope contains a blank entry; every scope area must be a non-empty string")
	}
	if !anyNonBlank(p.Invariants) {
		add("missing_invariants", "invariants is empty; state at least one property that must hold")
	} else if !allNonBlank(p.Invariants) {
		add("blank_invariant_entry", "invariants contains a blank entry; every invariant must be a non-empty string")
	}
	// allowed_files always has compiler-injected .agent/; require a non-proof-state
	// workspace entry rather than relying on slice length or duplicate entries.
	if !hasWorkspaceAllowance(p.AllowedFiles) {
		add("missing_allowed_files", "allowed_files names no workspace paths the plan may change")
	}
	if strings.TrimSpace(p.RollbackPlan) == "" {
		add("missing_rollback", "rollback_plan is empty")
	}
	if p.RiskLevel != "low" && p.RiskLevel != "medium" && p.RiskLevel != "high" {
		add("bad_risk_level", fmt.Sprintf("risk_level %q must be one of: low, medium, high", p.RiskLevel))
	}

	// 6 (plan-level): blocked_files must not cover proof state.
	if MatchesPath(".agent/x", p.BlockedFiles) {
		add("blocked_files_covers_proof_state", "blocked_files may not cover .agent/ proof state")
	}
	for _, f := range p.AllowedFiles {
		if code, msg := unsafePath(f); code != "" {
			add(code, "allowed_files entry "+msg)
		}
	}
	for _, f := range p.BlockedFiles {
		if code, msg := unsafePath(f); code != "" {
			add(code, "blocked_files entry "+msg)
		}
	}

	// 3-8 per step.
	ids := map[string]bool{}
	for _, s := range p.Steps {
		sid := s.ID // for messages
		if strings.TrimSpace(s.ID) == "" {
			add("empty_step_id", "a step has an empty id")
		} else if ids[s.ID] {
			add("duplicate_step_id", "duplicate step id: "+s.ID)
		} else {
			ids[s.ID] = true
		}
		if strings.TrimSpace(s.Action) == "" {
			add("empty_action", "step "+sid+" has an empty action")
		}
		if len(s.Files) == 0 {
			add("no_files", "step "+sid+" names no files")
		}
		if !anyNonBlank(s.ExpectedDiff) {
			add("empty_expected_diff", "step "+sid+" has no expected_diff; state the intended outcome")
		} else if !allNonBlank(s.ExpectedDiff) {
			add("blank_expected_diff_entry", "step "+sid+" expected_diff contains a blank entry")
		}
		if len(s.Validation) == 0 {
			add("no_validation", "step "+sid+" has no validation command")
		}
		if len(s.Validation) != len(s.Gates) {
			add("gate_misalignment", "step "+sid+" validation/gates are not index-aligned")
		}
		for _, g := range s.Gates {
			if len(g.Run) == 0 || !allNonBlank(g.Run) {
				add("empty_gate_argv", "step "+sid+" has a validation with empty argv")
			}
		}
		// 6 (step-level): path safety + proof-state target. path.Clean collapses an
		// in-bounds "." / "a/../b" so "./.agent/x" (raw first segment ".") cannot slip
		// a proof-state target past firstSegmentIsAgent. Write-time enforcement still
		// blocks the actual write, but the lock-time pre-check must not drift from it.
		for _, f := range s.Files {
			cleaned := path.Clean(f)
			if firstSegmentIsAgent(cleaned) {
				add("step_targets_proof_state", "step "+sid+" file "+f+" targets .agent/ proof state")
			}
			// unsafePath cleans internally (keeping the raw entry for its blank check).
			if code, msg := unsafePath(f); code != "" {
				add(code, "step "+sid+" file "+msg)
			}
		}
		// 7: scope coverage (every file allowed and not blocked). Match on the cleaned
		// path so an in-bounds ".agent/../src/x" is scoped as its real target src/x,
		// matching write-time filepath.Rel resolution.
		for _, f := range s.Files {
			cleaned := path.Clean(f)
			if firstSegmentIsAgent(cleaned) {
				continue // already reported; scope check is meaningless for proof state
			}
			if !MatchesPath(cleaned, p.AllowedFiles) {
				add("file_out_of_scope", "step "+sid+" file "+f+" is not covered by allowed_files")
			}
			if MatchesPath(cleaned, p.BlockedFiles) {
				add("file_blocked", "step "+sid+" file "+f+" is covered by blocked_files")
			}
		}
	}

	// 4-5: dependency integrity + cycle detection.
	ds = append(ds, dependencyDiagnostics(p.Steps)...)
	ds = append(ds, TraceabilityDiagnostics(p)...)
	return ds
}

var (
	traceIDPattern       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
	schemaVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// TraceabilityDiagnostics validates optional locked-plan traceability without
// applying CheckPlan's authoring-only plan-shape policy. It is a no-op for
// legacy plans that omit both traceability families.
func TraceabilityDiagnostics(p Plan) []Diagnostic {
	var ds []Diagnostic
	requirementIDs := map[string]bool{}
	criterionIDs := map[string]bool{}
	criterionVerificationMapped := map[string]bool{}
	for _, requirement := range p.Requirements {
		if !traceIDPattern.MatchString(requirement.ID) {
			ds = append(ds, Diagnostic{"invalid_requirement_id", "requirement id is not stable: " + requirement.ID})
		} else if requirementIDs[requirement.ID] {
			ds = append(ds, Diagnostic{"duplicate_requirement_id", "duplicate requirement id: " + requirement.ID})
		}
		requirementIDs[requirement.ID] = true
		if strings.TrimSpace(requirement.Text) == "" {
			ds = append(ds, Diagnostic{"missing_requirement_text", "requirement " + requirement.ID + " has empty text"})
		}
		if len(requirement.AcceptanceCriteria) == 0 {
			ds = append(ds, Diagnostic{"missing_acceptance_criteria", "requirement " + requirement.ID + " has no acceptance criteria"})
		}
		for _, criterion := range requirement.AcceptanceCriteria {
			if !traceIDPattern.MatchString(criterion.ID) {
				ds = append(ds, Diagnostic{"invalid_criterion_id", "acceptance criterion id is not stable: " + criterion.ID})
			} else if criterionIDs[criterion.ID] {
				ds = append(ds, Diagnostic{"duplicate_criterion_id", "duplicate acceptance criterion id: " + criterion.ID})
			}
			criterionIDs[criterion.ID] = true
			if strings.TrimSpace(criterion.Text) == "" {
				ds = append(ds, Diagnostic{"missing_criterion_text", "acceptance criterion " + criterion.ID + " has empty text"})
			}
			if criterion.Review != nil && criterion.Review.MinimumDepth != "spec_quality" && criterion.Review.MinimumDepth != "deep" {
				ds = append(ds, Diagnostic{"bad_review_depth", "acceptance criterion " + criterion.ID + " review minimum_depth must be spec_quality or deep"})
			}
			if criterion.Review != nil {
				// Any declared review floor counts as the verification mapping,
				// even with an invalid depth: bad_review_depth already reports
				// that mistake, and it must not double-report as an unmapped
				// criterion.
				criterionVerificationMapped[criterion.ID] = true
			}
		}
	}
	criterionStepMapped := map[string]bool{}
	for _, step := range p.Steps {
		stepCriterionIDs := map[string]bool{}
		for _, criterionID := range step.CriterionIDs {
			if stepCriterionIDs[criterionID] {
				ds = append(ds, Diagnostic{"duplicate_step_criterion", "step " + step.ID + " repeats acceptance criterion " + criterionID})
			}
			stepCriterionIDs[criterionID] = true
			if !criterionIDs[criterionID] {
				ds = append(ds, Diagnostic{"dangling_step_criterion", "step " + step.ID + " references unknown acceptance criterion " + criterionID})
			} else {
				criterionStepMapped[criterionID] = true
			}
		}
		for gateIndex, gate := range step.Gates {
			gateCriterionIDs := map[string]bool{}
			for _, criterionID := range gate.CriterionIDs {
				if gateCriterionIDs[criterionID] {
					ds = append(ds, Diagnostic{"duplicate_gate_criterion", fmt.Sprintf("step %s gate %d repeats acceptance criterion %s", step.ID, gateIndex, criterionID)})
				}
				gateCriterionIDs[criterionID] = true
				if !criterionIDs[criterionID] {
					ds = append(ds, Diagnostic{"dangling_gate_criterion", fmt.Sprintf("step %s gate %d references unknown acceptance criterion %s", step.ID, gateIndex, criterionID)})
				}
				if !stepCriterionIDs[criterionID] {
					ds = append(ds, Diagnostic{"gate_criterion_not_in_step", fmt.Sprintf("step %s gate %d criterion %s is not mapped to its parent step", step.ID, gateIndex, criterionID)})
				} else if criterionIDs[criterionID] {
					criterionVerificationMapped[criterionID] = true
				}
			}
		}
	}
	ids := make([]string, 0, len(criterionIDs))
	for criterionID := range criterionIDs {
		ids = append(ids, criterionID)
	}
	sort.Strings(ids)
	for _, criterionID := range ids {
		if !criterionStepMapped[criterionID] {
			ds = append(ds, Diagnostic{"unmapped_criterion_step", "acceptance criterion " + criterionID + " is not mapped to any step"})
		}
		if !criterionVerificationMapped[criterionID] {
			ds = append(ds, Diagnostic{"unmapped_criterion_verification", "acceptance criterion " + criterionID + " has no proving gate or review floor"})
		}
	}
	ds = append(ds, designDecisionDiagnostics(p)...)
	return ds
}

func designDecisionDiagnostics(p Plan) []Diagnostic {
	usesDesignDecisions := p.DesignDecisions != nil
	for _, step := range p.Steps {
		usesDesignDecisions = usesDesignDecisions || step.DesignDecisionIDs != nil
	}
	if !usesDesignDecisions {
		return nil
	}
	if !schemaVersionAtLeast(p.SchemaVersion, 0, 4, 0) {
		return []Diagnostic{{
			"design_decision_schema",
			"design decision fields require plan-lock schema_version 0.4.0 or newer",
		}}
	}

	var ds []Diagnostic
	decisionIDs := map[string]bool{}
	if p.DesignDecisions != nil {
		if len(*p.DesignDecisions) == 0 {
			ds = append(ds, Diagnostic{"missing_design_decisions", "design_decisions must contain at least one design decision"})
		}
		for _, decision := range *p.DesignDecisions {
			if !traceIDPattern.MatchString(decision.ID) {
				ds = append(ds, Diagnostic{"invalid_design_decision_id", "design decision id is not stable: " + decision.ID})
			} else if decisionIDs[decision.ID] {
				ds = append(ds, Diagnostic{"duplicate_design_decision_id", "duplicate design decision id: " + decision.ID})
			} else {
				decisionIDs[decision.ID] = true
			}
			if designStringIsBlank(decision.Text) {
				ds = append(ds, Diagnostic{"missing_design_decision_text", "design decision " + decision.ID + " has empty text"})
			}
			if decision.References != nil && slices.ContainsFunc(*decision.References, designStringIsBlank) {
				ds = append(ds, Diagnostic{"blank_design_decision_reference", "design decision " + decision.ID + " references must contain only non-blank strings"})
			}
		}
	}
	for _, step := range p.Steps {
		if step.DesignDecisionIDs == nil {
			continue
		}
		if len(*step.DesignDecisionIDs) == 0 {
			ds = append(ds, Diagnostic{"empty_step_design_decisions", "step " + step.ID + " design_decision_ids must contain at least one design decision id"})
			continue
		}
		if slices.ContainsFunc(*step.DesignDecisionIDs, designStringIsBlank) {
			ds = append(ds, Diagnostic{"blank_step_design_decision", "step " + step.ID + " design_decision_ids must contain only non-blank strings"})
			continue
		}
		seen := map[string]bool{}
		for _, decisionID := range *step.DesignDecisionIDs {
			if seen[decisionID] {
				ds = append(ds, Diagnostic{"duplicate_step_design_decision", "step " + step.ID + " repeats design decision " + decisionID})
			}
			seen[decisionID] = true
			if !decisionIDs[decisionID] {
				ds = append(ds, Diagnostic{"dangling_step_design_decision", "step " + step.ID + " references unknown design decision " + decisionID})
			}
		}
	}
	return ds
}

func designStringIsBlank(value string) bool {
	for _, r := range value {
		if !unicode.IsSpace(r) && (r < '\u001c' || r > '\u001f') {
			return false
		}
	}
	return true
}

func schemaVersionAtLeast(value string, wantMajor, wantMinor, wantPatch int) bool {
	parts := schemaVersionPattern.FindStringSubmatch(value)
	if parts == nil {
		return false
	}
	var got [3]int
	for i := range got {
		part, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return false
		}
		got[i] = part
	}
	if got[0] != wantMajor {
		return got[0] > wantMajor
	}
	if got[1] != wantMinor {
		return got[1] > wantMinor
	}
	return got[2] >= wantPatch
}

func hasWorkspaceAllowance(xs []string) bool {
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" && x != agentProofDir {
			return true
		}
	}
	return false
}

func anyNonBlank(xs []string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return true
		}
	}
	return false
}

func allNonBlank(xs []string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == "" {
			return false
		}
	}
	return true
}

// firstSegmentIsAgent reports whether rel's first path segment is .agent. It is
// the ONE proof-state check that is case-insensitive (EqualFold), unlike
// withAgentDir, hasWorkspaceAllowance, and the blocked_files_covers_proof_state
// probe, which match ".agent/" case-sensitively. The asymmetry is deliberate:
// this guards a real filesystem write target, where APFS case-folding lets
// ".Agent/x" resolve to the same path as ".agent/x" (this exact class was the
// CRITICAL bug in #209), whereas the pattern probes intentionally mirror
// agentflow's own case-sensitive matches_path. Do not "fix" the asymmetry.
func firstSegmentIsAgent(rel string) bool {
	first := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		first = rel[:i]
	}
	return strings.EqualFold(strings.TrimSpace(first), ".agent")
}

// unsafePath rejects absolute paths and parent traversal. It cleans before the
// abs/escape checks so an in-bounds "a/../b" collapses to "b" and is allowed
// (matching write-time filepath.Rel resolution), while an escaping "../x" or
// absolute "/x" survives path.Clean and is still rejected. The blank check runs
// on the raw input first because path.Clean("") == "." would hide an empty entry.
// Returns ("","") when safe, else a code and a message fragment ("<path> ...").
func unsafePath(rel string) (string, string) {
	r := strings.TrimSpace(rel)
	if r == "" {
		return "unsafe_path", "is blank"
	}
	c := path.Clean(r)
	if path.IsAbs(c) {
		return "unsafe_path", r + " is an absolute path"
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return "unsafe_path", r + " escapes the workspace with .."
		}
	}
	return "", ""
}

// dependencyDiagnostics reports unknown/blank dependencies and cycles, mirroring
// agentflow validation.py::_detect_depends_on_errors so local diagnostics match
// the CLI's, but running before the lock so the model can repair without a round
// trip.
func dependencyDiagnostics(steps []Step) []Diagnostic {
	var ds []Diagnostic
	known := map[string]bool{}
	for _, s := range steps {
		if s.ID != "" {
			known[s.ID] = true
		}
	}
	graph := map[string][]string{}
	for _, s := range steps {
		if s.ID == "" {
			continue
		}
		graph[s.ID] = nil
		for _, dep := range s.DependsOn {
			d := strings.TrimSpace(dep)
			if d == "" {
				ds = append(ds, Diagnostic{"empty_dependency", "step " + s.ID + " has a blank depends_on entry"})
				continue
			}
			if !known[d] {
				ds = append(ds, Diagnostic{"unknown_dependency", "step " + s.ID + " depends_on unknown step " + d})
			}
			graph[s.ID] = append(graph[s.ID], d)
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(id string, chain []string)
	visit = func(id string, chain []string) {
		if visited[id] {
			return
		}
		if visiting[id] {
			// Copy chain before appending: sibling recursions share chain's backing
			// array, so append(chain, id) could clobber a sibling's rendered path.
			ds = append(ds, Diagnostic{"dependency_cycle", "depends_on cycle detected: " + strings.Join(append(append([]string{}, chain...), id), " -> ")})
			return
		}
		visiting[id] = true
		for _, dep := range graph[id] {
			if _, ok := graph[dep]; ok {
				visit(dep, append(append([]string{}, chain...), id))
			}
		}
		visiting[id] = false
		visited[id] = true
	}
	ids := make([]string, 0, len(graph))
	for id := range graph {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		visit(id, nil)
	}
	return ds
}
