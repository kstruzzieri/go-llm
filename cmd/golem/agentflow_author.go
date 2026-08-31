package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// afLocker is the narrow agentflow surface the planner needs; *agentflow.Client
// satisfies it. Tests inject a stub instead of a second client implementation.
type afLocker interface {
	Probe(ctx context.Context) error
	ProbeWorkflow(ctx context.Context) error
	RecommendWorkflow(ctx context.Context, brief agentflow.TaskBrief, selectedProfile, reason string) (agentflow.WorkflowRecommendation, error)
	Init(ctx context.Context) error
	LockPlan(ctx context.Context, planPath string) error
	MaterializeWorkflowContract(ctx context.Context, recommendation agentflow.WorkflowRecommendation) error
}

// authorSession is the shared, single-Run state the submit_plan tool mutates and
// the -goal flow reads after the loop returns. The 2-submission budget lives here,
// not in prompt text, so it holds regardless of model behavior.
type authorSession struct {
	client afLocker
	root   string
	cancel context.CancelFunc

	attempts       int
	lockedPath     string
	terminalErr    error
	lastDiags      []agentflow.Diagnostic
	approvalDenied bool
	// deniedPlanPath/deniedPlanSaveErr record the best-effort artifact written
	// when the operator denies a previewed plan, so the denial error can point
	// at the compiled plan instead of discarding the authoring work.
	deniedPlanPath    string
	deniedPlanSaveErr error
	// lastPreview is the previous submission's rendered preview; a resubmission
	// preview appends a line-level delta against it so re-approval does not
	// require re-reading the whole plan.
	lastPreview string
	// The displayed route and exact tool arguments form one approval unit. Invoke
	// may mutate state only when both are still bound to the approved call.
	previewArgs           []byte
	previewRecommendation *agentflow.WorkflowRecommendation
	previewBrief          *agentflow.TaskBrief
	workflowProfile       string
	workflowReason        string
	taskBriefPath         string
	workflowHandoffPath   string
}

type submitPlanTool struct {
	sess *authorSession
}

func newSubmitPlanTool(sess *authorSession) *submitPlanTool {
	return &submitPlanTool{sess: sess}
}

const (
	maxPlanSubmissions                   = 2
	minPlannerOutput                     = 3500
	lockedPlanRel                        = ".agent/plan.lock.json"
	approvedWorkflowHandoffSchemaVersion = "0.1.0"
	// submitPlanToolName links the tool spec, the REPL approver's lock prompt,
	// and the author approver's denial handling; they must agree or a rename
	// silently downgrades the approval UX to the generic edit prompt.
	submitPlanToolName = "submit_plan"
)

type approvedWorkflowHandoff struct {
	SchemaVersion   string                           `json:"schema_version"`
	PlanSHA256      string                           `json:"plan_sha256"`
	TaskBriefSHA256 string                           `json:"task_brief_sha256"`
	Recommendation  agentflow.WorkflowRecommendation `json:"recommendation"`
}

func plannerModelOptions(options provider.ModelOptions) provider.ModelOptions {
	off := false
	options.NumPredict = max(options.NumPredict, minPlannerOutput)
	options.Think = &off
	options.ThinkEffort = ""
	return options
}

// plannerBudget aligns the planner's turn budget with the router's admission
// budget. A nonzero -output-reserve is floored at minPlannerOutput: the agent
// layer forwards Budget.OutputReserve over Options.NumPredict, so a smaller
// reserve would silently undo plannerModelOptions (and both sides then agree
// on the same reserve). A zero reserve leaves NumPredict untouched, but the
// router's ExpectedOutput then comes from the planner's NumPredict
// (>= minPlannerOutput) while the derived input ceiling reserved only the
// implicit DefaultExpectedOutput(useCase); lower the ceiling by the
// difference, or long planner sessions assemble input in a band every
// candidate must refuse (ErrBudgetAdaptationRequired) instead of compacting.
//
// useCase is the use case the author's caller actually routes with (#476
// D4). It is a parameter, not a literal: hard-coding a peer's use case here
// silently desynchronizes this reservation from the router's ExpectedOutput
// the moment the two use cases get different defaults.
//
// options is the planner's already-floored ModelOptions.
func plannerBudget(budget agent.Budget, options provider.ModelOptions, useCase string) agent.Budget {
	if budget.OutputReserve > 0 {
		budget.OutputReserve = max(budget.OutputReserve, minPlannerOutput)
		return budget
	}
	ceiling := budget.InputCeiling
	if ceiling <= 0 {
		ceiling = agent.DefaultInputCeiling
	}
	if delta := options.NumPredict - provider.DefaultExpectedOutput(useCase); delta > 0 {
		// Keep the result positive: a zero InputCeiling means "unset" to
		// turnBudget and would silently restore the full default ceiling.
		budget.InputCeiling = max(ceiling-delta, 1)
	}
	return budget
}

func (t *submitPlanTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        submitPlanToolName,
		Description: "Submit the finished plan as structured arguments. The plan is compiled, pre-checked, previewed for human approval, then locked. If it is rejected you receive diagnostics and may resubmit once.",
		Parameters:  json.RawMessage(submitPlanSchema),
	}
}

func (t *submitPlanTool) Effect() agent.Effect {
	// Write|Exec: the tool launches the fixed agentflow CLI (Exec) which writes
	// proof state (Write). The compiled plan preview must be explicitly approved
	// before Invoke may initialize or lock Agentflow state.
	return agent.Effect{Class: agent.Write | agent.Exec, Approval: agent.ApprovalAlways, Scope: agent.Scope{CWD: t.sess.root}}
}

func (t *submitPlanTool) Plan(ctx context.Context, args json.RawMessage) (agent.ToolPlan, error) {
	effect := t.Effect()
	t.sess.previewArgs = nil
	t.sess.previewRecommendation = nil
	t.sess.previewBrief = nil
	var ir agentflow.PlanIR
	if err := json.Unmarshal(args, &ir); err != nil {
		effect.Approval = agent.ApprovalNever
		return agent.ToolPlan{Effect: effect}, nil
	}
	plan := agentflow.Compile(ir)
	if len(authorPlanDiagnostics(ir, plan)) > 0 {
		effect.Approval = agent.ApprovalNever
		return agent.ToolPlan{Effect: effect}, nil
	}
	brief := taskBriefFromIR(ir, plan)
	recommendation, err := t.sess.client.RecommendWorkflow(ctx, brief, t.sess.workflowProfile, t.sess.workflowReason)
	if err != nil {
		effect.Approval = agent.ApprovalNever
		return agent.ToolPlan{Effect: effect}, fmt.Errorf("recommend Agentflow workflow: %w", err)
	}
	preview := renderPlanPreview(plan) + "\n" + renderWorkflowPreview(recommendation)
	shown := preview
	if t.sess.lastPreview != "" {
		shown += previewDelta(t.sess.lastPreview, preview)
	}
	t.sess.lastPreview = preview
	t.sess.previewArgs = append([]byte(nil), args...)
	t.sess.previewRecommendation = &recommendation
	t.sess.previewBrief = &brief
	return agent.ToolPlan{Effect: effect, Preview: shown}, nil
}

func (t *submitPlanTool) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	s := t.sess
	s.attempts++
	s.lastDiags = nil

	var ir agentflow.PlanIR
	if err := json.Unmarshal(args, &ir); err != nil {
		return t.reject([]agentflow.Diagnostic{{Code: "invalid_plan_ir", Message: "submit_plan arguments did not parse as a plan: " + err.Error()}})
	}
	plan := agentflow.Compile(ir)
	if diags := authorPlanDiagnostics(ir, plan); len(diags) > 0 {
		return t.reject(diags)
	}
	if s.previewRecommendation == nil || s.previewBrief == nil || !bytes.Equal(s.previewArgs, args) {
		return t.reject([]agentflow.Diagnostic{{Code: "workflow_preview_required", Message: "the exact plan and Agentflow route must be previewed before approval"}})
	}
	recommendation := *s.previewRecommendation
	brief := *s.previewBrief
	s.previewArgs = nil
	s.previewRecommendation = nil
	s.previewBrief = nil
	briefPath, handoffPath, err := t.lock(ctx, plan, recommendation, brief)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			s.cancel()
			return agent.ToolResult{}, context.Canceled
		}
		if term := classifyLockError(err); term != nil {
			s.terminalErr = term
			s.cancel()
			return agent.ToolResult{Content: "planner aborted: " + term.Error(), IsError: true}, nil
		}
		return t.reject(lockErrorDiagnostics(err))
	}
	s.taskBriefPath = briefPath
	s.workflowHandoffPath = handoffPath
	// #209 resolves relative -plan paths against the process cwd, not -root.
	// Preserve an absolute handoff path so an explicit -root works from anywhere.
	s.lockedPath = filepath.Join(s.root, filepath.FromSlash(lockedPlanRel))
	s.cancel()
	return agent.ToolResult{Content: "plan locked: " + s.lockedPath}, nil
}

func authorPlanDiagnostics(ir agentflow.PlanIR, plan agentflow.Plan) []agentflow.Diagnostic {
	diags := agentflow.CheckPlan(plan)
	switch ir.TaskType {
	case "docs", "bugfix", "feature", "refactor":
	default:
		diags = append(diags, agentflow.Diagnostic{Code: "bad_task_type", Message: fmt.Sprintf("task_type %q must be one of: docs, bugfix, feature, refactor", ir.TaskType)})
	}
	if ir.BlastRadius != "" && ir.BlastRadius != "isolated" && ir.BlastRadius != "local" && ir.BlastRadius != "cross_cutting" {
		diags = append(diags, agentflow.Diagnostic{Code: "bad_blast_radius", Message: fmt.Sprintf("blast_radius %q must be one of: isolated, local, cross_cutting", ir.BlastRadius)})
	}
	if ir.DeclaredSize != "" && ir.DeclaredSize != "xs" && ir.DeclaredSize != "s" && ir.DeclaredSize != "m" && ir.DeclaredSize != "l" && ir.DeclaredSize != "xl" {
		diags = append(diags, agentflow.Diagnostic{Code: "bad_declared_size", Message: fmt.Sprintf("declared_size %q must be one of: xs, s, m, l, xl", ir.DeclaredSize)})
	}
	if len(plan.Requirements) == 0 {
		diags = append(diags, agentflow.Diagnostic{Code: "missing_requirements", Message: "authored plans require at least one requirement with acceptance criteria"})
	}
	return diags
}

func taskBriefFromIR(ir agentflow.PlanIR, plan agentflow.Plan) agentflow.TaskBrief {
	brief := agentflow.TaskBriefFromPlan(plan, ir.TaskType)
	if ir.SecuritySensitive != nil {
		value := *ir.SecuritySensitive
		brief.SecuritySensitive = &value
	}
	if ir.BlastRadius != "" {
		value := ir.BlastRadius
		brief.BlastRadius = &value
	}
	if ir.DeclaredSize != "" {
		value := ir.DeclaredSize
		brief.DeclaredSize = &value
	}
	return brief
}

// reject records the diagnostics, ends the loop when the submission budget is
// exhausted, and returns them as a non-terminal error observation for the model.
func (t *submitPlanTool) reject(diags []agentflow.Diagnostic) (agent.ToolResult, error) {
	s := t.sess
	s.lastDiags = diags
	if s.attempts >= maxPlanSubmissions {
		s.cancel()
	}
	return agent.ToolResult{Content: renderDiagnostics(diags), IsError: true}, nil
}

func renderDiagnostics(diags []agentflow.Diagnostic) string {
	var b strings.Builder
	b.WriteString("plan rejected; fix these and resubmit:\n")
	for _, d := range diags {
		fmt.Fprintf(&b, "- [%s] %s\n", d.Code, d.Message)
	}
	return b.String()
}

func renderPlanPreview(plan agentflow.Plan) string {
	var b strings.Builder
	b.WriteString("Plan preview\n\nObjective\n  ")
	b.WriteString(previewText(plan.Objective))
	b.WriteString("\n\nScope\n")
	writePreviewList(&b, plan.Scope)
	b.WriteString("\nRisk\n  ")
	b.WriteString(previewText(plan.RiskLevel))
	b.WriteString("\n\nNon-goals\n")
	writePreviewList(&b, plan.NonGoals)
	b.WriteString("\nInvariants\n")
	writePreviewList(&b, plan.Invariants)
	b.WriteString("\nAllowed files\n")
	writePreviewList(&b, plan.AllowedFiles)
	b.WriteString("\nBlocked files\n")
	writePreviewList(&b, plan.BlockedFiles)
	b.WriteString("\nRollback\n  ")
	b.WriteString(previewText(plan.RollbackPlan))
	b.WriteString("\n\nSchema\n  ")
	b.WriteString(previewText(plan.SchemaVersion))
	b.WriteString("\n\nDrift budget\n")
	fmt.Fprintf(&b, "  unrelated_edits: %d\n  new_dependencies: %d\n  formatting_drift: %s\n  architecture_drift: %s\n",
		plan.DriftBudget.UnrelatedEdits, plan.DriftBudget.NewDependencies,
		previewText(plan.DriftBudget.FormattingDrift), previewText(plan.DriftBudget.ArchitectureDrift))
	b.WriteString("\nRequirements\n")
	if len(plan.Requirements) == 0 {
		b.WriteString("  - none\n")
	}
	for _, requirement := range plan.Requirements {
		fmt.Fprintf(&b, "  %s: %s\n", previewText(requirement.ID), previewText(requirement.Text))
		for _, criterion := range requirement.AcceptanceCriteria {
			fmt.Fprintf(&b, "    %s: %s", previewText(criterion.ID), previewText(criterion.Text))
			if criterion.Review != nil {
				fmt.Fprintf(&b, " [review: %s]", previewText(criterion.Review.MinimumDepth))
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nSteps\n")
	for _, step := range plan.Steps {
		fmt.Fprintf(&b, "  %s: %s\n", previewText(step.ID), previewText(step.Action))
		fmt.Fprintf(&b, "    files: %s\n", previewValues(step.Files))
		fmt.Fprintf(&b, "    depends_on: %s\n", previewValues(step.DependsOn))
		fmt.Fprintf(&b, "    criteria: %s\n", previewValues(step.CriterionIDs))
		fmt.Fprintf(&b, "    expected_diff: %s\n", previewValues(step.ExpectedDiff))
		b.WriteString("    validation:\n")
		for i, gate := range step.Gates {
			label := strings.Join(gate.Run, " ")
			if i < len(step.Validation) {
				label = step.Validation[i]
			}
			fmt.Fprintf(&b, "      - %s\n        argv: %s\n        criteria: %s\n", previewText(label), previewValues(gate.Run), previewValues(gate.CriterionIDs))
		}
	}
	return b.String()
}

func renderWorkflowPreview(recommendation agentflow.WorkflowRecommendation) string {
	contract := recommendation.Contract
	capabilities := make([]string, 0, len(contract.RequiredCapabilities))
	for _, capability := range contract.RequiredCapabilities {
		if capability.Required {
			capabilities = append(capabilities, capability.ID)
		}
	}
	var b strings.Builder
	b.WriteString("Workflow recommendation\n\n")
	fmt.Fprintf(&b, "  recommended: %s / %s\n", previewText(recommendation.Recommended.Pack), previewText(recommendation.Recommended.Profile))
	fmt.Fprintf(&b, "  selected: %s / %s\n", previewText(recommendation.Selected.Pack), previewText(recommendation.Selected.Profile))
	fmt.Fprintf(&b, "  signals: %s\n", previewValues(recommendation.Signals))
	fmt.Fprintf(&b, "  rationale: %s\n", previewText(recommendation.Rationale))
	fmt.Fprintf(&b, "  selection_reason: %s\n", previewText(contract.SelectionReason))
	fmt.Fprintf(&b, "  review_depth: %s\n", previewText(contract.ReviewDepth))
	fmt.Fprintf(&b, "  require_review_run: %t\n", contract.ProofPolicy.RequireReviewRun)
	fmt.Fprintf(&b, "  hunk_attribution: %s\n", previewText(contract.ProofPolicy.HunkAttribution))
	fmt.Fprintf(&b, "  required_capabilities: %s\n", previewValues(capabilities))
	fmt.Fprintf(&b, "  required_gates: %s\n", previewValues(contract.ValidationPolicy.RequiredGates))
	if recommendation.Override == nil {
		b.WriteString("  override: none\n")
	} else {
		fmt.Fprintf(&b, "  override: %s -> %s\n", previewText(recommendation.Override.FromProfile), previewText(recommendation.Override.ToProfile))
		fmt.Fprintf(&b, "  override_reason: %s\n", previewText(recommendation.Override.Reason))
	}
	b.WriteString("  alternatives:\n")
	if len(recommendation.Alternatives) == 0 {
		b.WriteString("    - none\n")
	}
	for _, alternative := range recommendation.Alternatives {
		fmt.Fprintf(&b, "    - profile: %s; relation: %s; reason: %s\n", previewText(alternative.Profile), previewText(alternative.Relation), previewText(alternative.Reason))
	}
	return b.String()
}

func writePreviewList(b *strings.Builder, values []string) {
	if len(values) == 0 {
		b.WriteString("  - none\n")
		return
	}
	for _, value := range values {
		fmt.Fprintf(b, "  - %s\n", previewText(value))
	}
}

// previewValues renders a list with the same QuoteToGraphic escaping as
// previewText. json.Marshal is NOT safe here: it leaves DEL and the C1 range
// (U+0080-U+009F, honored as terminal controls by some emulators) unescaped,
// which would reopen the preview-spoofing hole for model-authored files/argv.
func previewValues(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = previewText(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func previewText(value string) string {
	return strconv.QuoteToGraphic(value)
}

// previewDelta renders a line-level summary of what changed between two
// deterministic previews so a repair resubmission's re-approval prompt
// highlights the difference instead of forcing a full re-read. Both inputs are
// already control-safe, so their lines can be reprinted verbatim.
func previewDelta(prev, cur string) string {
	if prev == cur {
		return "\nResubmission: identical to the previously previewed plan.\n"
	}
	var b strings.Builder
	b.WriteString("\nResubmission changes since the previous preview\n")
	for _, ln := range extraLines(strings.Split(prev, "\n"), strings.Split(cur, "\n")) {
		fmt.Fprintf(&b, "  - %s\n", ln)
	}
	for _, ln := range extraLines(strings.Split(cur, "\n"), strings.Split(prev, "\n")) {
		fmt.Fprintf(&b, "  + %s\n", ln)
	}
	return b.String()
}

// extraLines returns the lines of a that exceed b's multiset, in order.
func extraLines(a, b []string) []string {
	remaining := map[string]int{}
	for _, ln := range b {
		remaining[ln]++
	}
	var out []string
	for _, ln := range a {
		if remaining[ln] > 0 {
			remaining[ln]--
			continue
		}
		out = append(out, ln)
	}
	return out
}

// lock stages the compiled plan through a temp file and the real lock-plan. The
// temp lives outside .agent/ so a failed lock leaves no stray workspace artifact;
// LockPlan itself writes the durable .agent/plan.lock.json.
func (t *submitPlanTool) lock(ctx context.Context, plan agentflow.Plan, recommendation agentflow.WorkflowRecommendation, brief agentflow.TaskBrief) (string, string, error) {
	if err := t.sess.client.Init(ctx); err != nil {
		return "", "", &terminalError{fmt.Errorf("agentflow init: %w", err)}
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", "", &terminalError{fmt.Errorf("marshal compiled plan: %w", err)}
	}
	tmp, err := os.CreateTemp("", "golem-plan-*.json")
	if err != nil {
		return "", "", &terminalError{fmt.Errorf("create staged plan: %w", err)}
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return "", "", &terminalError{fmt.Errorf("write staged plan %s: %w", tmp.Name(), err)}
	}
	if err := tmp.Close(); err != nil {
		return "", "", &terminalError{fmt.Errorf("close staged plan %s: %w", tmp.Name(), err)}
	}
	if err := t.sess.client.LockPlan(ctx, tmp.Name()); err != nil {
		return "", "", err
	}
	if err := t.sess.client.MaterializeWorkflowContract(ctx, recommendation); err != nil {
		return "", "", &terminalError{fmt.Errorf("materialize Agentflow workflow contract after plan lock: %w", err)}
	}
	briefPath, err := saveApprovedTaskBrief(t.sess.root, brief)
	if err != nil {
		return "", "", &terminalError{fmt.Errorf("save approved Agentflow task brief after plan lock: %w", err)}
	}
	handoffPath, err := saveApprovedWorkflowHandoff(t.sess.root, plan, brief, recommendation)
	if err != nil {
		_ = os.Remove(briefPath)
		return "", "", &terminalError{fmt.Errorf("save approved Agentflow workflow handoff after plan lock: %w", err)}
	}
	return briefPath, handoffPath, nil
}

func saveApprovedTaskBrief(root string, brief agentflow.TaskBrief) (string, error) {
	return saveApprovedJSON(root, "golem-task-brief-*.json", brief)
}

func saveApprovedWorkflowHandoff(root string, plan agentflow.Plan, brief agentflow.TaskBrief, recommendation agentflow.WorkflowRecommendation) (string, error) {
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal approved plan: %w", err)
	}
	planSHA256, err := canonicalPlanJSONSHA256(planJSON)
	if err != nil {
		return "", fmt.Errorf("digest approved plan: %w", err)
	}
	taskBriefSHA256, err := canonicalJSONSHA256(brief)
	if err != nil {
		return "", fmt.Errorf("digest approved task brief: %w", err)
	}
	handoff := approvedWorkflowHandoff{
		SchemaVersion:   approvedWorkflowHandoffSchemaVersion,
		PlanSHA256:      planSHA256,
		TaskBriefSHA256: taskBriefSHA256,
		Recommendation:  recommendation,
	}
	return saveApprovedJSON(root, "golem-workflow-handoff-*.json", handoff)
}

func saveApprovedJSON(root, pattern string, value any) (string, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, ".agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(f.Name())
		}
	}()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	remove = false
	return f.Name(), nil
}

func canonicalJSONSHA256(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// agentflowCanonicalJSONSHA256 matches Python's json.dumps defaults used by
// Agentflow's plan_binding_sha256: sorted keys, compact separators, and
// ensure_ascii=true. It is intentionally separate from Golem's own canonical
// JSON contract above.
func agentflowCanonicalJSONSHA256(value any) (string, error) {
	var encoded strings.Builder
	if err := writeAgentflowCanonicalJSON(&encoded, value); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(encoded.String()))
	return fmt.Sprintf("%x", sum), nil
}

func writeAgentflowCanonicalJSON(dst *strings.Builder, value any) error {
	switch value := value.(type) {
	case nil:
		dst.WriteString("null")
	case bool:
		dst.WriteString(strconv.FormatBool(value))
	case string:
		writeAgentflowJSONString(dst, value)
	case agentflowJSONString:
		writeAgentflowJSONRunes(dst, value)
	case json.Number:
		number, err := agentflowJSONNumber(value)
		if err != nil {
			return err
		}
		dst.WriteString(number)
	case agentflowJSONConstant:
		dst.WriteString(string(value))
	case []any:
		dst.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := writeAgentflowCanonicalJSON(dst, item); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			writeAgentflowJSONString(dst, key)
			dst.WriteByte(':')
			if err := writeAgentflowCanonicalJSON(dst, value[key]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	case agentflowJSONObject:
		members := append(agentflowJSONObject(nil), value...)
		sort.Slice(members, func(i, j int) bool {
			return compareAgentflowJSONStrings(members[i].key, members[j].key) < 0
		})
		dst.WriteByte('{')
		for i, member := range members {
			if i > 0 {
				dst.WriteByte(',')
			}
			writeAgentflowJSONRunes(dst, member.key)
			dst.WriteByte(':')
			if err := writeAgentflowCanonicalJSON(dst, member.value); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	default:
		return fmt.Errorf("unsupported Agentflow canonical JSON value %T", value)
	}
	return nil
}

func writeAgentflowJSONString(dst *strings.Builder, value string) {
	writeAgentflowJSONRunes(dst, []rune(value))
}

func writeAgentflowJSONRunes(dst *strings.Builder, value []rune) {
	dst.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			dst.WriteByte('\\')
			dst.WriteRune(r)
		case '\b':
			dst.WriteString(`\b`)
		case '\f':
			dst.WriteString(`\f`)
		case '\n':
			dst.WriteString(`\n`)
		case '\r':
			dst.WriteString(`\r`)
		case '\t':
			dst.WriteString(`\t`)
		default:
			switch {
			case r < 0x20 || r == 0x7f:
				_, _ = fmt.Fprintf(dst, `\u%04x`, r)
			case r < utf8RuneSelf:
				dst.WriteRune(r)
			case r <= 0xffff:
				_, _ = fmt.Fprintf(dst, `\u%04x`, r)
			default:
				high, low := utf16.EncodeRune(r)
				_, _ = fmt.Fprintf(dst, `\u%04x\u%04x`, high, low)
			}
		}
	}
	dst.WriteByte('"')
}

type agentflowJSONString []rune

type agentflowJSONConstant string

type agentflowJSONMember struct {
	key   agentflowJSONString
	value any
}

type agentflowJSONObject []agentflowJSONMember

func compareAgentflowJSONStrings(left, right agentflowJSONString) int {
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	default:
		return 0
	}
}

const utf8RuneSelf = 0x80

func agentflowJSONNumber(value json.Number) (string, error) {
	raw := value.String()
	if !strings.ContainsAny(raw, ".eE") {
		integer, ok := new(big.Int).SetString(raw, 10)
		if !ok {
			return "", fmt.Errorf("invalid Agentflow JSON integer %q", raw)
		}
		return integer.String(), nil
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return "", fmt.Errorf("invalid Agentflow JSON float %q", raw)
	}
	if math.IsInf(parsed, 1) {
		return "Infinity", nil
	}
	if math.IsInf(parsed, -1) {
		return "-Infinity", nil
	}
	if math.IsNaN(parsed) {
		return "NaN", nil
	}
	if parsed == 0 {
		if math.Signbit(parsed) {
			return "-0.0", nil
		}
		return "0.0", nil
	}

	scientific := strconv.FormatFloat(parsed, 'e', -1, 64)
	mantissa, exponentText, ok := strings.Cut(scientific, "e")
	if !ok {
		return "", fmt.Errorf("format Agentflow JSON float %q", raw)
	}
	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		return "", fmt.Errorf("format Agentflow JSON float %q: %w", raw, err)
	}
	if exponent < -4 || exponent >= 16 {
		sign := "+"
		if exponent < 0 {
			sign = "-"
			exponent = -exponent
		}
		return fmt.Sprintf("%se%s%02d", mantissa, sign, exponent), nil
	}

	negative := strings.HasPrefix(mantissa, "-")
	digits := strings.ReplaceAll(strings.TrimPrefix(mantissa, "-"), ".", "")
	point := exponent + 1
	var fixed strings.Builder
	if negative {
		fixed.WriteByte('-')
	}
	switch {
	case point <= 0:
		fixed.WriteString("0.")
		fixed.WriteString(strings.Repeat("0", -point))
		fixed.WriteString(digits)
	case point >= len(digits):
		fixed.WriteString(digits)
		fixed.WriteString(strings.Repeat("0", point-len(digits)))
		fixed.WriteString(".0")
	default:
		fixed.WriteString(digits[:point])
		fixed.WriteByte('.')
		fixed.WriteString(digits[point:])
	}
	return fixed.String(), nil
}

// canonicalPlanJSONSHA256 binds every semantic Agentflow plan field while
// excluding only lock-plan's restamped bookkeeping, matching Agentflow's plan
// binding contract. The narrow parser preserves lone UTF-16 surrogate escapes:
// Python's json module retains them, while encoding/json replaces them with
// U+FFFD before a canonical encoder can see them.
func canonicalPlanJSONSHA256(data []byte) (string, error) {
	value, err := parseAgentflowJSON(data)
	if err != nil {
		return "", err
	}
	plan, ok := value.(agentflowJSONObject)
	if !ok {
		return "", errors.New("plan must be a JSON object")
	}
	filtered := make(agentflowJSONObject, 0, len(plan))
	for _, member := range plan {
		if agentflowJSONStringEqualASCII(member.key, "locked") || agentflowJSONStringEqualASCII(member.key, "locked_at") {
			continue
		}
		filtered = append(filtered, member)
	}
	return agentflowCanonicalJSONSHA256(filtered)
}

func agentflowJSONStringEqualASCII(value agentflowJSONString, want string) bool {
	if len(value) != len(want) {
		return false
	}
	for i, r := range value {
		if r != rune(want[i]) {
			return false
		}
	}
	return true
}

type agentflowJSONParser struct {
	data   []byte
	offset int
}

func parseAgentflowJSON(data []byte) (any, error) {
	parser := agentflowJSONParser{data: data}
	value, err := parser.value()
	if err != nil {
		return nil, err
	}
	parser.skipSpace()
	if parser.offset != len(parser.data) {
		return nil, parser.errorf("trailing JSON")
	}
	return value, nil
}

func (p *agentflowJSONParser) value() (any, error) {
	p.skipSpace()
	if p.offset >= len(p.data) {
		return nil, p.errorf("unexpected end of JSON")
	}
	switch p.data[p.offset] {
	case '{':
		return p.object()
	case '[':
		return p.array()
	case '"':
		return p.string()
	case 't':
		return p.literal("true", true)
	case 'f':
		return p.literal("false", false)
	case 'n':
		return p.literal("null", nil)
	case 'N':
		return p.constant("NaN")
	case 'I':
		return p.constant("Infinity")
	default:
		if p.data[p.offset] == '-' && len(p.data)-p.offset >= len("-Infinity") && string(p.data[p.offset:p.offset+len("-Infinity")]) == "-Infinity" {
			return p.constant("-Infinity")
		}
		return p.number()
	}
}

func (p *agentflowJSONParser) object() (agentflowJSONObject, error) {
	p.offset++
	p.skipSpace()
	if p.take('}') {
		return agentflowJSONObject{}, nil
	}
	members := agentflowJSONObject{}
	indexes := map[string]int{}
	for {
		p.skipSpace()
		if p.offset >= len(p.data) || p.data[p.offset] != '"' {
			return nil, p.errorf("object key must be a string")
		}
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if !p.take(':') {
			return nil, p.errorf("expected colon after object key")
		}
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		identity := agentflowJSONStringIdentity(key)
		if index, exists := indexes[identity]; exists {
			members[index].value = value
		} else {
			indexes[identity] = len(members)
			members = append(members, agentflowJSONMember{key: key, value: value})
		}
		p.skipSpace()
		if p.take('}') {
			return members, nil
		}
		if !p.take(',') {
			return nil, p.errorf("expected comma or closing brace")
		}
	}
}

func (p *agentflowJSONParser) array() ([]any, error) {
	p.offset++
	p.skipSpace()
	if p.take(']') {
		return []any{}, nil
	}
	values := []any{}
	for {
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipSpace()
		if p.take(']') {
			return values, nil
		}
		if !p.take(',') {
			return nil, p.errorf("expected comma or closing bracket")
		}
	}
}

func (p *agentflowJSONParser) string() (agentflowJSONString, error) {
	p.offset++
	value := agentflowJSONString{}
	for p.offset < len(p.data) {
		current := p.data[p.offset]
		switch {
		case current == '"':
			p.offset++
			return value, nil
		case current == '\\':
			p.offset++
			escaped, err := p.escape()
			if err != nil {
				return nil, err
			}
			value = append(value, escaped...)
		case current < 0x20:
			return nil, p.errorf("unescaped control character in string")
		case current < utf8.RuneSelf:
			value = append(value, rune(current))
			p.offset++
		default:
			r, size := utf8.DecodeRune(p.data[p.offset:])
			if r == utf8.RuneError && size == 1 {
				return nil, p.errorf("invalid UTF-8 in string")
			}
			value = append(value, r)
			p.offset += size
		}
	}
	return nil, p.errorf("unterminated string")
}

func (p *agentflowJSONParser) escape() ([]rune, error) {
	if p.offset >= len(p.data) {
		return nil, p.errorf("unterminated string escape")
	}
	escape := p.data[p.offset]
	p.offset++
	switch escape {
	case '"', '\\', '/':
		return []rune{rune(escape)}, nil
	case 'b':
		return []rune{'\b'}, nil
	case 'f':
		return []rune{'\f'}, nil
	case 'n':
		return []rune{'\n'}, nil
	case 'r':
		return []rune{'\r'}, nil
	case 't':
		return []rune{'\t'}, nil
	case 'u':
		first, err := p.hexRune()
		if err != nil {
			return nil, err
		}
		if first >= 0xd800 && first <= 0xdbff && p.offset+6 <= len(p.data) && p.data[p.offset] == '\\' && p.data[p.offset+1] == 'u' {
			second, ok := decodeAgentflowHexRune(p.data[p.offset+2 : p.offset+6])
			if ok && second >= 0xdc00 && second <= 0xdfff {
				p.offset += 6
				return []rune{utf16.DecodeRune(first, second)}, nil
			}
		}
		return []rune{first}, nil
	default:
		return nil, p.errorf("invalid string escape")
	}
}

func (p *agentflowJSONParser) hexRune() (rune, error) {
	if p.offset+4 > len(p.data) {
		return 0, p.errorf("short Unicode escape")
	}
	r, ok := decodeAgentflowHexRune(p.data[p.offset : p.offset+4])
	if !ok {
		return 0, p.errorf("invalid Unicode escape")
	}
	p.offset += 4
	return r, nil
}

func decodeAgentflowHexRune(data []byte) (rune, bool) {
	if len(data) != 4 {
		return 0, false
	}
	var value rune
	for _, digit := range data {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += rune(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += rune(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += rune(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func (p *agentflowJSONParser) number() (json.Number, error) {
	start := p.offset
	p.take('-')
	if p.offset >= len(p.data) {
		return "", p.errorf("invalid JSON number")
	}
	if p.take('0') {
		if p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
			return "", p.errorf("leading zero in JSON number")
		}
	} else {
		if p.data[p.offset] < '1' || p.data[p.offset] > '9' {
			return "", p.errorf("invalid JSON value")
		}
		for p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
			p.offset++
		}
	}
	if p.take('.') {
		fraction := p.offset
		for p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
			p.offset++
		}
		if p.offset == fraction {
			return "", p.errorf("JSON fraction has no digits")
		}
	}
	if p.offset < len(p.data) && (p.data[p.offset] == 'e' || p.data[p.offset] == 'E') {
		p.offset++
		if p.offset < len(p.data) && (p.data[p.offset] == '+' || p.data[p.offset] == '-') {
			p.offset++
		}
		exponent := p.offset
		for p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
			p.offset++
		}
		if p.offset == exponent {
			return "", p.errorf("JSON exponent has no digits")
		}
	}
	return json.Number(string(p.data[start:p.offset])), nil
}

func (p *agentflowJSONParser) literal(text string, value any) (any, error) {
	if p.offset+len(text) > len(p.data) || string(p.data[p.offset:p.offset+len(text)]) != text {
		return nil, p.errorf("invalid JSON value")
	}
	p.offset += len(text)
	return value, nil
}

func (p *agentflowJSONParser) constant(text string) (agentflowJSONConstant, error) {
	if p.offset+len(text) > len(p.data) || string(p.data[p.offset:p.offset+len(text)]) != text {
		return "", p.errorf("invalid JSON value")
	}
	p.offset += len(text)
	return agentflowJSONConstant(text), nil
}

func (p *agentflowJSONParser) skipSpace() {
	for p.offset < len(p.data) {
		switch p.data[p.offset] {
		case ' ', '\t', '\n', '\r':
			p.offset++
		default:
			return
		}
	}
}

func (p *agentflowJSONParser) take(want byte) bool {
	if p.offset >= len(p.data) || p.data[p.offset] != want {
		return false
	}
	p.offset++
	return true
}

func (p *agentflowJSONParser) errorf(format string, args ...any) error {
	return fmt.Errorf("parse Agentflow canonical JSON at byte %d: %s", p.offset, fmt.Sprintf(format, args...))
}

func agentflowJSONStringIdentity(value agentflowJSONString) string {
	var identity strings.Builder
	for _, r := range value {
		_, _ = fmt.Fprintf(&identity, "%08x", r)
	}
	return identity.String()
}

// decodeAgentflowPlanJSON projects the Python JSON dialect into Golem's typed
// plan. Python may persist unknown non-finite future fields as NaN/Infinity;
// they are replaced with null only for this narrow typed projection while the
// canonical digest above still binds their exact Python value.
func decodeAgentflowPlanJSON(data []byte, plan *agentflow.Plan) error {
	value, err := parseAgentflowJSON(data)
	if err != nil {
		return err
	}
	if _, ok := value.(agentflowJSONObject); !ok {
		return errors.New("plan must be a JSON object")
	}
	var projected strings.Builder
	if err := writeAgentflowPlanProjectionJSON(&projected, value); err != nil {
		return err
	}
	return json.Unmarshal([]byte(projected.String()), plan)
}

func writeAgentflowPlanProjectionJSON(dst *strings.Builder, value any) error {
	switch value := value.(type) {
	case nil:
		dst.WriteString("null")
	case bool:
		dst.WriteString(strconv.FormatBool(value))
	case agentflowJSONString:
		if err := writeAgentflowPlanJSONString(dst, value); err != nil {
			return err
		}
	case json.Number:
		dst.WriteString(value.String())
	case agentflowJSONConstant:
		dst.WriteString("null")
	case []any:
		dst.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := writeAgentflowPlanProjectionJSON(dst, item); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	case agentflowJSONObject:
		dst.WriteByte('{')
		for i, member := range value {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := writeAgentflowPlanJSONString(dst, member.key); err != nil {
				return err
			}
			dst.WriteByte(':')
			if err := writeAgentflowPlanProjectionJSON(dst, member.value); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	default:
		return fmt.Errorf("unsupported Agentflow plan projection JSON value %T", value)
	}
	return nil
}

// writeAgentflowPlanJSONString rejects lone UTF-16 surrogates before the typed
// projection reaches encoding/json, which would silently replace them with
// U+FFFD. Canonical hashing still preserves them exactly, but Golem must never
// execute argv different from the locked Agentflow plan.
func writeAgentflowPlanJSONString(dst *strings.Builder, value agentflowJSONString) error {
	for _, r := range value {
		if r >= 0xd800 && r <= 0xdfff {
			return errors.New("agentflow plan contains a lone UTF-16 surrogate that cannot be executed faithfully")
		}
	}
	writeAgentflowJSONRunes(dst, value)
	return nil
}

// terminalError marks a fail-fast condition that no model repair can fix.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

// These validation_error message tokens identify fields emitted by the compiler,
// failure is in a field the compiler emits (not the model IR): the IR cannot
// repair it, so it is terminal. Treating schema_version / drift_budget /
// "missing required field" as terminal is SAFE because CheckPlan is a superset of
// validate_plan's model-field checks, so any such error that survives CheckPlan to
// reach lock is compiler-owned, not model-repairable. Kept narrow and test-pinned.
// classifyLockError returns a non-nil terminal error when the lock failure cannot
// be repaired by the model, else nil (the caller treats it as repairable).
func classifyLockError(err error) error {
	var te *terminalError
	if errors.As(err, &te) {
		return te
	}
	var ce *agentflow.CommandError
	if !errors.As(err, &ce) {
		return err // runner spawn / unexpected: terminal
	}
	if len(ce.Errors) == 0 {
		return err // opaque nonzero exit: terminal
	}
	for _, se := range ce.Errors {
		if se.Code != "validation_error" {
			return err // only ordinary content validation errors are repairable
		}
		if strings.Contains(se.Message, "schema_version") || strings.Contains(se.Message, "drift_budget") ||
			strings.Contains(se.Message, "missing required field") {
			return err
		}
	}
	return nil // ordinary content validation_error(s): repairable
}

// lockErrorDiagnostics maps a repairable lock error's structured entries into
// diagnostics for the model.
func lockErrorDiagnostics(err error) []agentflow.Diagnostic {
	var ce *agentflow.CommandError
	if !errors.As(err, &ce) || len(ce.Errors) == 0 {
		return []agentflow.Diagnostic{{Code: "lock_rejected", Message: err.Error()}}
	}
	out := make([]agentflow.Diagnostic, 0, len(ce.Errors))
	for _, se := range ce.Errors {
		// StructuredError and Diagnostic are field-identical ({Code, Message
		// string}); a direct conversion is what staticcheck (S1016) expects.
		out = append(out, agentflow.Diagnostic(se))
	}
	return out
}

// submitPlanSchema is the model-facing JSON Schema for PlanIR. Hand-written to
// avoid a schema-generation dependency; TestSubmitPlanSchema keeps it aligned with
// the IR structs by pinning required keys and asserting every PlanIR,
// RequirementIR, CriterionIR, StepIR, and GateIR field appears under the
// matching properties (so a json-tag rename cannot silently mislead the model).
const submitPlanSchema = `{
  "type": "object",
  "required": ["task_type", "objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "requirements", "steps"],
  "properties": {
    "task_type":     {"type": "string", "enum": ["docs", "bugfix", "feature", "refactor"], "description": "the kind of task being planned"},
    "objective":     {"type": "string", "description": "one-sentence goal of the whole plan"},
    "scope":         {"type": "array", "items": {"type": "string"}, "description": "areas the plan touches"},
    "invariants":    {"type": "array", "items": {"type": "string"}, "description": "properties that must hold after the plan"},
    "risk_level":    {"type": "string", "enum": ["low", "medium", "high"]},
    "rollback_plan": {"type": "string", "description": "how to undo the whole plan, e.g. git checkout -- ."},
    "allowed_files": {"type": "array", "items": {"type": "string"}, "description": "glob patterns of files the plan may change; must cover every step file"},
    "blocked_files": {"type": "array", "items": {"type": "string"}, "description": "optional glob patterns that must never change"},
    "non_goals":     {"type": "array", "items": {"type": "string"}, "description": "optional explicit exclusions"},
    "security_sensitive": {"type": "boolean", "description": "optional; set only when the task explicitly affects security-sensitive behavior"},
    "blast_radius": {"type": "string", "enum": ["isolated", "local", "cross_cutting"], "description": "optional explicit blast radius; omit when unknown"},
    "declared_size": {"type": "string", "enum": ["xs", "s", "m", "l", "xl"], "description": "optional explicit task size; omit when unknown"},
    "requirements": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["id", "text", "acceptance_criteria"],
        "properties": {
          "id":   {"type": "string", "pattern": "^[A-Za-z][A-Za-z0-9._-]{0,127}$", "description": "stable requirement id, unique within the plan"},
          "text": {"type": "string", "minLength": 1, "description": "required behavior or constraint"},
          "acceptance_criteria": {
            "type": "array",
            "minItems": 1,
            "items": {
              "type": "object",
              "required": ["id", "text"],
              "properties": {
                "id":   {"type": "string", "pattern": "^[A-Za-z][A-Za-z0-9._-]{0,127}$", "description": "stable criterion id, unique across the entire plan; never reuse across requirements"},
                "text": {"type": "string", "minLength": 1, "description": "observable acceptance condition"},
                "review": {
                  "type": "object",
                  "required": ["minimum_depth"],
                  "properties": {"minimum_depth": {"type": "string", "enum": ["spec_quality", "deep"]}}
                }
              }
            }
          }
        }
      }
    },
    "steps": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "action", "files", "expected_diff", "validations"],
        "properties": {
          "id":            {"type": "string"},
          "action":        {"type": "string"},
          "files":         {"type": "array", "items": {"type": "string"}},
          "expected_diff": {"type": "array", "items": {"type": "string"}},
          "depends_on":    {"type": "array", "items": {"type": "string"}, "description": "ids of steps that must complete first"},
          "criterion_ids": {"type": "array", "minItems": 1, "uniqueItems": true, "items": {"type": "string"}, "description": "acceptance criteria implemented by this step"},
          "validations": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["argv"],
              "properties": {
                "label": {"type": "string"},
                "argv":  {"type": "array", "minItems": 1, "items": {"type": "string"}, "description": "executable command, e.g. [\"go\",\"test\",\"./...\"]"},
                "criterion_ids": {"type": "array", "minItems": 1, "uniqueItems": true, "items": {"type": "string"}, "description": "criteria this command proves; must be assigned to the parent step"}
              }
            }
          }
        }
      }
    }
  }
}`

const plannerBasePrompt = "You are Golem's planner. From the user's goal, author a durable execution plan for the AgentFlow contract. " +
	"First inspect the repository with the read-only tools (read_file, search, glob, list). Then design a minimal directed-acyclic set of steps. " +
	"Choose the required task_type from docs, bugfix, feature, or refactor. Set security_sensitive, blast_radius, or declared_size only when repository evidence or the user's request makes that signal explicit; otherwise omit it rather than guessing. " +
	"Author stable requirement and acceptance criterion IDs. Criterion IDs share one plan-wide namespace: never reuse an acceptance criterion id across requirements. Map every criterion into at least one step criterion_ids list and either a proving validation criterion_ids list or a spec_quality/deep review floor. " +
	"Each step needs: concrete workspace-relative files, at least one expected_diff sentence, and at least one executable validation as an argv array (for example [\"go\",\"test\",\"./...\"]). " +
	"Provide allowed_files globs that cover every step file, at least one invariant, and a rollback_plan. Use depends_on to order steps; do not create cycles. " +
	"Planning is read-only: you may not edit files or run commands here. When ready, call submit_plan. " +
	"If that submission is rejected you will receive diagnostics; fix them and make one final resubmission."

var (
	errPlannerRejected               = errors.New("planner could not produce a lockable plan within the submission budget")
	errPlannerNoSubmission           = errors.New("the planner did not submit a plan")
	errPlannerInterrupted            = errors.New("planning interrupted before a plan was locked")
	errPlannerApprovalDenied         = errors.New("plan approval denied; plan was not locked")
	errAgentflowAuthoringUnsupported = errors.New("golem: Agentflow plan authoring is unsupported on this platform")
)

// guardExistingPlan refuses to proceed when locking would discard durable state.
// An absent lock or AgentFlow's pristine unlocked scaffold (blank objective, no
// steps) may proceed; a locked plan, a non-empty unlocked draft, or an
// unreadable/malformed plan is refused. There is no planner-level override.
func guardExistingPlan(root string) error {
	path := filepath.Join(root, ".agent", "plan.lock.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("refusing to plan: cannot read existing %s: %w", path, err)
	}
	var existing struct {
		SchemaVersion *string            `json:"schema_version"`
		Objective     *string            `json:"objective"`
		Steps         *[]json.RawMessage `json:"steps"`
		Locked        *bool              `json:"locked"`
	}
	if err := json.Unmarshal(b, &existing); err != nil {
		return fmt.Errorf("refusing to plan: %s exists but is not valid plan JSON: %w; resolve it via agentflow before planning", path, err)
	}
	if existing.SchemaVersion == nil || strings.TrimSpace(*existing.SchemaVersion) == "" ||
		existing.Objective == nil || existing.Steps == nil || existing.Locked == nil {
		return fmt.Errorf("refusing to plan: %s is not the recognized AgentFlow scaffold; resolve it before planning", path)
	}
	if *existing.Locked {
		return fmt.Errorf("refusing to plan: %s is already locked; use agentflow to reset the run before re-planning", path)
	}
	if strings.TrimSpace(*existing.Objective) != "" || len(*existing.Steps) > 0 {
		return fmt.Errorf("refusing to plan: %s holds an unlocked draft; resolve it via agentflow before planning", path)
	}
	return nil
}

// runAgentflowAuthor is the -goal flow: guard, probe, run a read-only authoring
// loop, then initialize and lock only after submit_plan's preview is approved.
func runAgentflowAuthor(ctx context.Context, src lineSource, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string) error {
	var runner agentflow.Runner
	if f.agentflowSrc != "" {
		runner = agentflow.NewSrcExecRunner(root, f.agentflowSrc)
	} else {
		runner = agentflow.NewExecRunner(root)
	}
	// Only -approve-plan-lock may arrive without a source: its approver never
	// reads stdin. Any other nil is an internal wiring error, and opening a
	// second reader here would defeat the single-source invariant.
	var approver agent.Approver
	if f.approvePlanLock {
		approver = &autoPlanApprover{out: stdout}
	} else {
		if src == nil {
			return errors.New("golem: interactive plan approval requires a line source")
		}
		approver = newReplApprover(src, stdout, sess.color)
	}
	return runAgentflowAuthorWithClient(ctx, stdout, stderr, interrupts, sess, f, root, agentflow.NewClient(runner, root), approver)
}

func runAgentflowAuthorWithClient(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string, client afLocker, approver agent.Approver) error {
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	if interrupts != nil {
		go func() {
			select {
			case <-interrupts:
				cancel()
			case <-done:
			}
		}()
	}

	release, err := acquireAuthorLock(loopCtx, root)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errPlannerInterrupted
		}
		if errors.Is(err, errAgentflowAuthoringUnsupported) {
			return err
		}
		return fmt.Errorf("acquire planner lock: %w", err)
	}
	defer release()

	// Keep the guard inside the injected seam so tests exercise the same safety
	// boundary as production.
	if err := guardExistingPlan(root); err != nil {
		return err
	}

	// Fail before any model work when AgentFlow is unavailable.
	if err := client.Probe(loopCtx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(loopCtx.Err(), context.Canceled) {
			return errPlannerInterrupted
		}
		return fmt.Errorf("agentflow unavailable: %w", err)
	}
	if err := client.ProbeWorkflow(loopCtx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(loopCtx.Err(), context.Canceled) {
			return errPlannerInterrupted
		}
		return fmt.Errorf("agentflow workflow routing unavailable: %w", err)
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return fmt.Errorf("planner workspace: %w", err)
	}
	ws.SetScopeGuard(func(rel string, _ bool) error { return denyProofState(rel) })

	as := &authorSession{
		client: client, root: root, cancel: cancel,
		workflowProfile: f.workflowProfile, workflowReason: f.workflowReason,
	}

	planTools := append(agenttools.NewFileToolsForWorkspace(ws), newSubmitPlanTool(as))

	system := plannerBasePrompt
	if sess.projectContextBlock != "" {
		system = system + "\n\n" + sess.projectContextBlock
	}

	plannerOpts := plannerModelOptions(sess.modelOptions)
	req := agent.Request{
		Goal:     f.goal,
		System:   system,
		Tools:    planTools,
		MaxSteps: sess.maxSteps,
		Budget:   plannerBudget(sess.budget, plannerOpts, config.UseCasePlanning),
		Options:  plannerOpts,
		Approver: &authorPlanApprover{delegate: approver, sess: as},
	}
	_, runErr := sess.orch.Run(loopCtx, req, agent.Observer(newRenderer(stderr, false, sess.maxSteps, sess.clock, sess.mixed)))
	budgetExhausted := as.attempts >= maxPlanSubmissions

	switch {
	case as.lockedPath != "":
		_, _ = fmt.Fprintf(stdout, "locked approved plan: %s\n", as.lockedPath)
		_, _ = fmt.Fprintln(stdout, "execute separately:")
		// Include -root: #209 resolves proof state against the process cwd, so an
		// operator running this from another directory would otherwise write to the
		// wrong tree (the cwd-mismatch class fixed in 45838e7). root is absolute.
		_, _ = fmt.Fprintf(stdout, "  golem -plan %s -root %s -task-brief %s -workflow-handoff %s",
			shellQuote(as.lockedPath), shellQuote(root), shellQuote(as.taskBriefPath), shellQuote(as.workflowHandoffPath))
		if f.agentflowSrc != "" {
			_, _ = fmt.Fprintf(stdout, " -agentflow-src %s", shellQuote(f.agentflowSrc))
		}
		_, _ = fmt.Fprintln(stdout, " -approve-plan-edits -approve-plan-gates")
		return nil
	case as.approvalDenied:
		if as.deniedPlanSaveErr != nil {
			return fmt.Errorf("%w (failed to save the denied plan for reference: %v)", errPlannerApprovalDenied, as.deniedPlanSaveErr)
		}
		if as.deniedPlanPath != "" {
			return fmt.Errorf("%w; denied plan saved for reference: %s", errPlannerApprovalDenied, as.deniedPlanPath)
		}
		return errPlannerApprovalDenied
	case as.terminalErr != nil:
		return as.terminalErr
	case budgetExhausted && len(as.lastDiags) > 0:
		_, _ = fmt.Fprint(stderr, renderDiagnostics(as.lastDiags))
		return errPlannerRejected
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		return runErr
	case errors.Is(runErr, context.Canceled):
		// The loop was cancelled with no locked plan, diagnostics, or terminal
		// error recorded: an operator interrupt (a successful/exhausted submit is
		// handled by the cases above, which set those fields before cancelling).
		return errPlannerInterrupted
	default:
		return errPlannerNoSubmission
	}
}

type authorPlanApprover struct {
	delegate agent.Approver
	sess     *authorSession
}

func (a *authorPlanApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	approved := false
	var err error
	if a.delegate != nil {
		approved, err = a.delegate.Approve(ctx, call, preview)
	}
	if err == nil && !approved && call.Function.Name == submitPlanToolName {
		a.sess.approvalDenied = true
		a.sess.deniedPlanPath, a.sess.deniedPlanSaveErr = saveDeniedPlan(call.Function.Arguments)
		a.sess.cancel()
	}
	return approved, err
}

// autoPlanApprover implements -approve-plan-lock: the non-interactive -goal
// path. It prints the same preview an operator would see, then approves the
// lock. Anything other than submit_plan stays denied (fail-safe: the planner's
// other tools are read-only and never reach approval).
type autoPlanApprover struct{ out io.Writer }

func (a *autoPlanApprover) Approve(_ context.Context, call provider.ToolCall, preview string) (bool, error) {
	if call.Function.Name != submitPlanToolName {
		return false, nil
	}
	_, _ = fmt.Fprintln(a.out, strings.TrimRight(preview, "\n"))
	_, _ = fmt.Fprintln(a.out, "plan lock auto-approved via -approve-plan-lock")
	return true, nil
}

// saveDeniedPlan writes the compiled plan a denied approval rejected to a temp
// file so the operator can inspect or adapt it without re-running the whole
// planning loop.
func saveDeniedPlan(args json.RawMessage) (string, error) {
	var ir agentflow.PlanIR
	if err := json.Unmarshal(args, &ir); err != nil {
		return "", fmt.Errorf("parse denied plan: %w", err)
	}
	b, err := json.MarshalIndent(agentflow.Compile(ir), "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal denied plan: %w", err)
	}
	f, err := os.CreateTemp("", "golem-denied-plan-*.json")
	if err != nil {
		return "", fmt.Errorf("create denied plan file: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write denied plan file %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("close denied plan file %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
