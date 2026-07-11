package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// afLocker is the narrow agentflow surface the planner needs; *agentflow.Client
// satisfies it. Tests inject a stub instead of a second client implementation.
type afLocker interface {
	Probe(ctx context.Context) error
	Init(ctx context.Context) error
	LockPlan(ctx context.Context, planPath string) error
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
}

type submitPlanTool struct {
	sess *authorSession
}

func newSubmitPlanTool(sess *authorSession) *submitPlanTool {
	return &submitPlanTool{sess: sess}
}

const (
	maxPlanSubmissions = 2
	minPlannerOutput   = 3500
	lockedPlanRel      = ".agent/plan.lock.json"
	// submitPlanToolName links the tool spec, the REPL approver's lock prompt,
	// and the author approver's denial handling; they must agree or a rename
	// silently downgrades the approval UX to the generic edit prompt.
	submitPlanToolName = "submit_plan"
)

func plannerModelOptions(options provider.ModelOptions) provider.ModelOptions {
	off := false
	options.NumPredict = max(options.NumPredict, minPlannerOutput)
	options.Think = &off
	options.ThinkEffort = ""
	return options
}

// plannerBudget floors a nonzero OutputReserve at minPlannerOutput: the agent
// layer forwards Budget.OutputReserve over Options.NumPredict, so an
// -output-reserve below the floor would silently undo plannerModelOptions.
// Zero stays zero — no override fires and the NumPredict floor holds.
func plannerBudget(budget agent.Budget) agent.Budget {
	if budget.OutputReserve > 0 {
		budget.OutputReserve = max(budget.OutputReserve, minPlannerOutput)
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

func (t *submitPlanTool) Plan(_ context.Context, args json.RawMessage) (agent.ToolPlan, error) {
	effect := t.Effect()
	var ir agentflow.PlanIR
	if err := json.Unmarshal(args, &ir); err != nil {
		effect.Approval = agent.ApprovalNever
		return agent.ToolPlan{Effect: effect}, nil
	}
	plan := agentflow.Compile(ir)
	if len(authorPlanDiagnostics(plan)) > 0 {
		effect.Approval = agent.ApprovalNever
		return agent.ToolPlan{Effect: effect}, nil
	}
	preview := renderPlanPreview(plan)
	shown := preview
	if t.sess.lastPreview != "" {
		shown += previewDelta(t.sess.lastPreview, preview)
	}
	t.sess.lastPreview = preview
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
	if diags := authorPlanDiagnostics(plan); len(diags) > 0 {
		return t.reject(diags)
	}
	if err := t.lock(ctx, plan); err != nil {
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
	// #209 resolves relative -plan paths against the process cwd, not -root.
	// Preserve an absolute handoff path so an explicit -root works from anywhere.
	s.lockedPath = filepath.Join(s.root, filepath.FromSlash(lockedPlanRel))
	s.cancel()
	return agent.ToolResult{Content: "plan locked: " + s.lockedPath}, nil
}

func authorPlanDiagnostics(plan agentflow.Plan) []agentflow.Diagnostic {
	diags := agentflow.CheckPlan(plan)
	if len(plan.Requirements) == 0 {
		diags = append(diags, agentflow.Diagnostic{Code: "missing_requirements", Message: "authored plans require at least one requirement with acceptance criteria"})
	}
	return diags
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
func (t *submitPlanTool) lock(ctx context.Context, plan agentflow.Plan) error {
	if err := t.sess.client.Init(ctx); err != nil {
		return &terminalError{fmt.Errorf("agentflow init: %w", err)}
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return &terminalError{fmt.Errorf("marshal compiled plan: %w", err)}
	}
	tmp, err := os.CreateTemp("", "golem-plan-*.json")
	if err != nil {
		return &terminalError{fmt.Errorf("create staged plan: %w", err)}
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return &terminalError{fmt.Errorf("write staged plan %s: %w", tmp.Name(), err)}
	}
	if err := tmp.Close(); err != nil {
		return &terminalError{fmt.Errorf("close staged plan %s: %w", tmp.Name(), err)}
	}
	return t.sess.client.LockPlan(ctx, tmp.Name())
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
  "required": ["objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "requirements", "steps"],
  "properties": {
    "objective":     {"type": "string", "description": "one-sentence goal of the whole plan"},
    "scope":         {"type": "array", "items": {"type": "string"}, "description": "areas the plan touches"},
    "invariants":    {"type": "array", "items": {"type": "string"}, "description": "properties that must hold after the plan"},
    "risk_level":    {"type": "string", "enum": ["low", "medium", "high"]},
    "rollback_plan": {"type": "string", "description": "how to undo the whole plan, e.g. git checkout -- ."},
    "allowed_files": {"type": "array", "items": {"type": "string"}, "description": "glob patterns of files the plan may change; must cover every step file"},
    "blocked_files": {"type": "array", "items": {"type": "string"}, "description": "optional glob patterns that must never change"},
    "non_goals":     {"type": "array", "items": {"type": "string"}, "description": "optional explicit exclusions"},
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
	"Author stable requirement and acceptance criterion IDs. Criterion IDs share one plan-wide namespace: never reuse an acceptance criterion id across requirements. Map every criterion into at least one step criterion_ids list and either a proving validation criterion_ids list or a spec_quality/deep review floor. " +
	"Each step needs: concrete workspace-relative files, at least one expected_diff sentence, and at least one executable validation as an argv array (for example [\"go\",\"test\",\"./...\"]). " +
	"Provide allowed_files globs that cover every step file, at least one invariant, and a rollback_plan. Use depends_on to order steps; do not create cycles. " +
	"Planning is read-only: you may not edit files or run commands here. When ready, call submit_plan. " +
	"If that submission is rejected you will receive diagnostics; fix them and make one final resubmission."

var (
	errPlannerRejected       = errors.New("planner could not produce a lockable plan within the submission budget")
	errPlannerNoSubmission   = errors.New("the planner did not submit a plan")
	errPlannerInterrupted    = errors.New("planning interrupted before a plan was locked")
	errPlannerApprovalDenied = errors.New("plan approval denied; plan was not locked")
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
func runAgentflowAuthor(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string) error {
	var runner agentflow.Runner
	if f.agentflowSrc != "" {
		runner = agentflow.NewSrcExecRunner(root, f.agentflowSrc)
	} else {
		runner = agentflow.NewExecRunner(root)
	}
	var approver agent.Approver = newReplApprover(newLineReader(stdin), stdout, sess.color)
	if f.approvePlanLock {
		approver = &autoPlanApprover{out: stdout}
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
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return fmt.Errorf("planner workspace: %w", err)
	}
	ws.SetScopeGuard(func(rel string, _ bool) error { return denyProofState(rel) })

	as := &authorSession{client: client, root: root, cancel: cancel}

	planTools := append(agenttools.NewFileToolsForWorkspace(ws), newSubmitPlanTool(as))

	system := plannerBasePrompt
	if sess.projectContextBlock != "" {
		system = system + "\n\n" + sess.projectContextBlock
	}

	req := agent.Request{
		Goal:     f.goal,
		System:   system,
		Tools:    planTools,
		MaxSteps: sess.maxSteps,
		Budget:   plannerBudget(sess.budget),
		Options:  plannerModelOptions(sess.modelOptions),
		Approver: &authorPlanApprover{delegate: approver, sess: as},
	}
	_, runErr := sess.orch.Run(loopCtx, req, agent.Observer(newRenderer(stderr, false, sess.maxSteps, sess.clock)))
	budgetExhausted := as.attempts >= maxPlanSubmissions

	switch {
	case as.lockedPath != "":
		_, _ = fmt.Fprintf(stdout, "locked approved plan: %s\n", as.lockedPath)
		_, _ = fmt.Fprintln(stdout, "execute separately:")
		// Include -root: #209 resolves proof state against the process cwd, so an
		// operator running this from another directory would otherwise write to the
		// wrong tree (the cwd-mismatch class fixed in 45838e7). root is absolute.
		_, _ = fmt.Fprintf(stdout, "  golem -plan %s -root %s -approve-plan-edits -approve-plan-gates\n", shellQuote(as.lockedPath), shellQuote(root))
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

// acquireAuthorLock serializes -goal authoring across processes for one workspace.
// The stable temp inode is intentionally retained so waiters cannot race an unlink.
func acquireAuthorLock(ctx context.Context, root string) (func(), error) {
	sum := sha256.Sum256([]byte(root))
	lockName := fmt.Sprintf("golem-agentflow-author-%x.lock", sum)
	lockPath := filepath.Join(os.TempDir(), lockName)
	tmpRoot, err := os.OpenRoot(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("open temp directory: %w", err)
	}
	f, err := tmpRoot.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	_ = tmpRoot.Close()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
