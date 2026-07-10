package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
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
// repair it, so it is terminal. Kept narrow and test-pinned.
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
// avoid a schema-generation dependency; a test keeps it aligned with the struct.
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
