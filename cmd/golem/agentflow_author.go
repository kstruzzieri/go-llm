package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
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

	attempts    int
	lockedPath  string
	terminalErr error
	lastDiags   []agentflow.Diagnostic
}

type submitPlanTool struct {
	sess *authorSession
}

func newSubmitPlanTool(sess *authorSession) *submitPlanTool {
	return &submitPlanTool{sess: sess}
}

const (
	maxPlanSubmissions = 2
	lockedPlanRel      = ".agent/plan.lock.json"
)

func (t *submitPlanTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "submit_plan",
		Description: "Submit the finished plan as structured arguments. The plan is compiled, pre-checked, and locked. If it is rejected you receive diagnostics and may resubmit once.",
		Parameters:  json.RawMessage(submitPlanSchema),
	}
}

func (t *submitPlanTool) Effect() agent.Effect {
	// Write|Exec: the tool launches the fixed agentflow CLI (Exec) which writes
	// proof state (Write). ApprovalNever: it cannot edit source or choose a
	// command, and -goal is itself the operator's request to create the lock.
	return agent.Effect{Class: agent.Write | agent.Exec, Approval: agent.ApprovalNever, Scope: agent.Scope{CWD: t.sess.root}}
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
	if diags := agentflow.CheckPlan(plan); len(diags) > 0 {
		return t.reject(diags)
	}
	if err := t.lock(ctx, plan); err != nil {
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

// lock stages the compiled plan through a temp file and the real lock-plan. The
// temp lives outside .agent/ so a failed lock leaves no stray workspace artifact;
// LockPlan itself writes the durable .agent/plan.lock.json.
func (t *submitPlanTool) lock(ctx context.Context, plan agentflow.Plan) error {
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return &terminalError{err} // marshaling our own struct failed: our bug
	}
	tmp, err := os.CreateTemp("", "golem-plan-*.json")
	if err != nil {
		return &terminalError{err}
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return &terminalError{err}
	}
	if err := tmp.Close(); err != nil {
		return &terminalError{err}
	}
	return t.sess.client.LockPlan(ctx, tmp.Name())
}

// terminalError marks a fail-fast condition that no model repair can fix.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

// compilerOwnedTokens are substrings of a validation_error message that mean the
// failure is in a field the compiler emits (not the model IR): the IR cannot
// repair it, so it is terminal. Treating schema_version / drift_budget /
// "missing required field" as terminal is SAFE because CheckPlan is a superset of
// validate_plan's model-field checks, so any such error that survives CheckPlan to
// reach lock is compiler-owned, not model-repairable. Kept narrow and test-pinned.
var compilerOwnedTokens = []string{"schema_version", "drift_budget", "missing required field"}

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
		for _, tok := range compilerOwnedTokens {
			if strings.Contains(se.Message, tok) {
				return err
			}
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
// the IR structs by pinning the top-level, step, and gate required keys and
// asserting every PlanIR/StepIR/GateIR struct field appears under the matching
// properties (so a json-tag rename cannot silently mislead the model).
const submitPlanSchema = `{
  "type": "object",
  "required": ["objective", "scope", "invariants", "risk_level", "rollback_plan", "allowed_files", "steps"],
  "properties": {
    "objective":     {"type": "string", "description": "one-sentence goal of the whole plan"},
    "scope":         {"type": "array", "items": {"type": "string"}, "description": "areas the plan touches"},
    "invariants":    {"type": "array", "items": {"type": "string"}, "description": "properties that must hold after the plan"},
    "risk_level":    {"type": "string", "enum": ["low", "medium", "high"]},
    "rollback_plan": {"type": "string", "description": "how to undo the whole plan, e.g. git checkout -- ."},
    "allowed_files": {"type": "array", "items": {"type": "string"}, "description": "glob patterns of files the plan may change; must cover every step file"},
    "blocked_files": {"type": "array", "items": {"type": "string"}, "description": "optional glob patterns that must never change"},
    "non_goals":     {"type": "array", "items": {"type": "string"}, "description": "optional explicit exclusions"},
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
          "validations": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["argv"],
              "properties": {
                "label": {"type": "string"},
                "argv":  {"type": "array", "items": {"type": "string"}, "description": "executable command, e.g. [\"go\",\"test\",\"./...\"]"}
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
	"Each step needs: concrete workspace-relative files, at least one expected_diff sentence, and at least one executable validation as an argv array (for example [\"go\",\"test\",\"./...\"]). " +
	"Provide allowed_files globs that cover every step file, at least one invariant, and a rollback_plan. Use depends_on to order steps; do not create cycles. " +
	"Planning is read-only: you may not edit files or run commands here. When ready, call submit_plan. " +
	"If that submission is rejected you will receive diagnostics; fix them and make one final resubmission."

var (
	errPlannerRejected     = errors.New("planner could not produce a lockable plan within the submission budget")
	errPlannerNoSubmission = errors.New("the planner did not submit a plan")
	errPlannerInterrupted  = errors.New("planning interrupted before a plan was locked")
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

// runAgentflowAuthor is the -goal flow: guard, probe, init, run a read-only
// authoring loop with submit_plan, and report the locked plan (or diagnostics).
func runAgentflowAuthor(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string) error {
	var runner agentflow.Runner
	if f.agentflowSrc != "" {
		runner = agentflow.NewSrcExecRunner(root, f.agentflowSrc)
	} else {
		runner = agentflow.NewExecRunner(root)
	}
	return runAgentflowAuthorWithClient(ctx, stdout, stderr, interrupts, sess, f, root, agentflow.NewClient(runner, root))
}

func runAgentflowAuthorWithClient(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string, client afLocker) error {
	// Keep the guard inside the injected seam so tests exercise the same safety
	// boundary as production.
	if err := guardExistingPlan(root); err != nil {
		return err
	}

	// Fail before any model work when AgentFlow is unavailable.
	if err := client.Probe(ctx); err != nil {
		return fmt.Errorf("agentflow unavailable: %w", err)
	}
	// Idempotent scaffold (agentflow 0.4 init skips existing artifacts).
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("agentflow init: %w", err)
	}

	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return err
	}
	ws.SetScopeGuard(func(rel string, _ bool) error { return denyProofState(rel) })

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Watch for an interrupt (Ctrl-C) for the duration of the authoring loop so
	// SIGINT cancels the in-flight model call instead of being swallowed, mirroring
	// runOnce (repl.go) and runAgentflowTask's step loop. interrupts is nil in unit
	// tests; the goroutine (which would block forever on a nil channel) is guarded.
	done := make(chan struct{})
	defer close(done)
	if interrupts != nil {
		// Drain a stale interrupt buffered before this run so it cannot spuriously
		// cancel the loop.
		select {
		case <-interrupts:
		default:
		}
		go func() {
			select {
			case <-interrupts:
				cancel()
			case <-done:
			}
		}()
	}

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
		Budget:   sess.budget,
		Options:  sess.modelOptions,
	}
	_, runErr := sess.orch.Run(loopCtx, req, agent.Observer(newRenderer(stderr, false, sess.maxSteps, sess.clock)))
	budgetExhausted := as.attempts >= maxPlanSubmissions

	switch {
	case as.lockedPath != "":
		_, _ = fmt.Fprintf(stdout, "locked plan: %s\n", as.lockedPath)
		_, _ = fmt.Fprintln(stdout, "review the locked plan, especially validation command argv, before approval")
		_, _ = fmt.Fprintln(stdout, "execute separately:")
		// Include -root: #209 resolves proof state against the process cwd, so an
		// operator running this from another directory would otherwise write to the
		// wrong tree (the cwd-mismatch class fixed in 45838e7). root is absolute.
		_, _ = fmt.Fprintf(stdout, "  golem -plan %s -root %s -approve-plan-edits -approve-plan-gates\n", as.lockedPath, root)
		return nil
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
