package agentflow

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const agentName = "golem"

var attemptIDPattern = regexp.MustCompile(`^(WT[a-z0-9]{1,16}-)?A[0-9]+$`)

// Client is a host-owned typed wrapper over the agentflow CLI.
type Client struct {
	r    Runner
	root string
}

// NewClient returns a Client that drives agentflow via r, with --root set to the
// workspace root for the subcommands that take it.
func NewClient(r Runner, root string) *Client { return &Client{r: r, root: root} }

// call runs a subcommand and returns stdout, mapping exit!=0 (or a --json
// invalid envelope) to *CommandError. Pass wantJSON=true for --json commands.
func (c *Client) call(ctx context.Context, name string, args []string, wantJSON bool) ([]byte, error) {
	return c.callInput(ctx, name, args, wantJSON, nil)
}

func (c *Client) callInput(ctx context.Context, name string, args []string, wantJSON bool, stdin []byte) ([]byte, error) {
	out, errb, exit, err := c.r.Run(ctx, args, stdin)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		ce := &CommandError{Cmd: name, Exit: exit, Stderr: string(errb)}
		if wantJSON {
			var env statusEnvelope
			if json.Unmarshal(out, &env) == nil {
				ce.Errors = append(ce.Errors, env.Errors...)
				for _, fnd := range env.Findings {
					ce.Errors = append(ce.Errors, StructuredError{Code: fnd.Severity, Message: fnd.Message})
				}
				for _, d := range env.Diagnostics {
					ce.Errors = append(ce.Errors, StructuredError{Code: "diagnostic", Message: d})
				}
			}
		}
		return nil, ce
	}
	return out, nil
}

func (c *Client) rootArgs(extra ...string) []string {
	return append([]string{"--root", c.root}, extra...)
}

// --- AgentFlow 0.4.0 payload structs ---

type claimResult struct {
	AttemptID string `json:"attempt_id"`
}
type nextStepResult struct {
	ID string `json:"id"`
}
type finishRunResult struct {
	OK          bool     `json:"ok"`
	StoppedAt   string   `json:"stopped_at"`
	Diagnostics []string `json:"diagnostics"`
}

// ReviewRun is Agentflow's authoritative record-review --json projection.
type ReviewRun struct {
	ReviewRunID    string         `json:"review_run_id"`
	GateStatus     string         `json:"gate_status"`
	ActiveBlocking []string       `json:"active_blocking"`
	AmendmentReady bool           `json:"amendment_ready"`
	Findings       ReviewFindings `json:"findings"`
}

type ReviewFindings struct {
	Index []ReviewFinding `json:"index"`
}

type ReviewFinding struct {
	FindingID    string          `json:"finding_id"`
	Severity     string          `json:"severity"`
	Status       string          `json:"status"`
	OwningStep   string          `json:"owning_step"`
	Claim        string          `json:"claim"`
	Location     *ReviewLocation `json:"location,omitempty"`
	SuggestedFix string          `json:"suggested_fix"`
}

type ReviewLocation struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	LineEnd int    `json:"line_end,omitempty"`
}

type reviewRunProjection struct {
	ReviewRunID    *string                   `json:"review_run_id"`
	GateStatus     *string                   `json:"gate_status"`
	ActiveBlocking *[]string                 `json:"active_blocking"`
	AmendmentReady *bool                     `json:"amendment_ready"`
	Findings       *reviewFindingsProjection `json:"findings"`
}

type reviewFindingsProjection struct {
	Index *[]ReviewFinding `json:"index"`
}

type amendStepResult struct {
	Event     string `json:"event"`
	StepID    string `json:"step_id"`
	AttemptID string `json:"attempt_id"`
}

// FinishRunError reports a finish-run that stopped before completion (nonzero
// exit or ok=false in the --json envelope).
type FinishRunError struct {
	StoppedAt   string
	Diagnostics []string
}

func (e *FinishRunError) Error() string {
	msg := "agentflow finish-run stopped"
	if e.StoppedAt != "" {
		msg += " at " + e.StoppedAt
	}
	if len(e.Diagnostics) > 0 {
		msg += ": " + strings.Join(e.Diagnostics, "; ")
	}
	return msg
}

// EvidenceEntry is one optional sidecar entry recorded before the plan is
// locked. The CLI requires ID, Claim, and Source.
type EvidenceEntry struct {
	ID         string   `json:"id"`
	Claim      string   `json:"claim"`
	Source     string   `json:"source"`
	Confidence string   `json:"confidence,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Supports   []string `json:"supports,omitempty"`
}

// LockPlan validates and locks the plan JSON at planPath, opening the P0 run.
// It passes no --root: agentflow locates .agent/ by the runner's Cmd.Dir.
func (c *Client) LockPlan(ctx context.Context, planPath string) error {
	_, err := c.call(ctx, "lock-plan",
		[]string{"lock-plan", "--from-json", planPath, "--json"}, true) // no --root
	return err
}

// Init creates the .agent/ planning scaffold under the workspace root.
func (c *Client) Init(ctx context.Context) error {
	_, err := c.call(ctx, "init", append([]string{"init"}, c.rootArgs()...), false)
	return err
}

// InitExecution creates the execution contract that the step loop runs against.
func (c *Client) InitExecution(ctx context.Context) error {
	_, err := c.call(ctx, "init-execution", append([]string{"init-execution"}, c.rootArgs()...), false)
	return err
}

// Doctor checks .agent/ integrity and returns a *CommandError carrying any
// findings when the contract is missing or inconsistent.
func (c *Client) Doctor(ctx context.Context) error {
	_, err := c.call(ctx, "doctor", append([]string{"doctor"}, c.rootArgs("--json")...), true)
	return err
}

// NextStep returns the next eligible step id, or "" when none remain.
func (c *Client) NextStep(ctx context.Context) (string, error) {
	out, err := c.call(ctx, "next-step", append([]string{"next-step"}, c.rootArgs("--json")...), true)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(out)) == "null" {
		return "", nil
	}
	var r nextStepResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("agentflow next-step: parse %q: %w", out, err)
	}
	return r.ID, nil
}

// ClaimStep claims step for the golem agent and returns the new attempt id.
func (c *Client) ClaimStep(ctx context.Context, step string) (string, error) {
	args := append([]string{"claim-step", step}, c.rootArgs("--agent", agentName, "--json")...)
	out, err := c.call(ctx, "claim-step", args, true)
	if err != nil {
		return "", err
	}
	var r claimResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("agentflow claim-step: parse %q: %w", out, err)
	}
	return r.AttemptID, nil
}

// RecordReview validates and records a review manifest through Agentflow,
// returning its amendment projection without reading review source artifacts.
func (c *Client) RecordReview(ctx context.Context, manifestPath string) (ReviewRun, error) {
	args := append([]string{"record-review"}, c.rootArgs("--manifest", manifestPath, "--json")...)
	out, err := c.call(ctx, "record-review", args, true)
	if err != nil {
		return ReviewRun{}, err
	}
	var projection reviewRunProjection
	if err := json.Unmarshal(out, &projection); err != nil {
		return ReviewRun{}, fmt.Errorf("agentflow record-review: parse %q: %w", out, err)
	}
	missing := ""
	switch {
	case projection.ReviewRunID == nil:
		missing = "review_run_id"
	case projection.GateStatus == nil:
		missing = "gate_status"
	case projection.ActiveBlocking == nil:
		missing = "active_blocking"
	case projection.AmendmentReady == nil:
		missing = "amendment_ready"
	case projection.Findings == nil:
		missing = "findings"
	}
	if missing == "" && *projection.AmendmentReady && projection.Findings.Index == nil {
		missing = "findings.index"
	}
	if missing != "" {
		return ReviewRun{}, fmt.Errorf("agentflow record-review: missing required field %s", missing)
	}
	if *projection.GateStatus != "pass" && *projection.GateStatus != "warn" && *projection.GateStatus != "fail" {
		return ReviewRun{}, fmt.Errorf("agentflow record-review: invalid gate_status %q", *projection.GateStatus)
	}
	findings := []ReviewFinding{}
	if projection.Findings.Index != nil {
		findings = *projection.Findings.Index
	}
	return ReviewRun{
		ReviewRunID:    *projection.ReviewRunID,
		GateStatus:     *projection.GateStatus,
		ActiveBlocking: *projection.ActiveBlocking,
		AmendmentReady: *projection.AmendmentReady,
		Findings:       ReviewFindings{Index: findings},
	}, nil
}

// AmendStep opens one review-feedback amendment linked to every supplied
// canonical finding reference and returns the new attempt id.
func (c *Client) AmendStep(ctx context.Context, step string, findingRefs []string) (string, error) {
	reason := "address review findings " + strings.Join(findingRefs, ", ")
	args := append([]string{"amend-step", step}, c.rootArgs(
		"--agent", agentName, "--reason", reason, "--reason-code", "review_feedback")...)
	for _, ref := range findingRefs {
		args = append(args, "--finding", ref)
	}
	args = append(args, "--json")
	out, err := c.call(ctx, "amend-step", args, true)
	if err != nil {
		return "", err
	}
	var r amendStepResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("agentflow amend-step: parse %q: %w", out, err)
	}
	if r.Event != "amendment_started" || r.StepID != step || !attemptIDPattern.MatchString(r.AttemptID) {
		return "", fmt.Errorf("agentflow amend-step: invalid success projection (event=%q step_id=%q attempt_id valid=%t)",
			r.Event, r.StepID, attemptIDPattern.MatchString(r.AttemptID))
	}
	return r.AttemptID, nil
}

// RecordFileChange records an edit to path against (step, attempt) so the
// journal can later reconcile it; this is the receipt golem cannot forge.
func (c *Client) RecordFileChange(ctx context.Context, step, attempt, path string) error {
	args := append([]string{"record-file-change"},
		c.rootArgs("--step", step, "--attempt", attempt, "--path", path, "--agent", agentName, "--json")...)
	_, err := c.call(ctx, "record-file-change", args, true)
	return err
}

// RunGate runs the gate command argv (everything after the `--` separator)
// under agentflow's proof harness for (step, attempt), recording the result.
func (c *Client) RunGate(ctx context.Context, step, attempt, gate string, argv []string) error {
	args := append([]string{"run"},
		c.rootArgs("--step", step, "--attempt", attempt, "--gate", gate, "--agent", agentName, "--confirm-risk", "--")...)
	args = append(args, argv...)
	_, err := c.call(ctx, "run", args, false)
	return err
}

// FinishStep closes the attempt, verifying its gates and file-change receipts;
// a *CommandError surfaces the diagnostics when the step is not yet complete.
func (c *Client) FinishStep(ctx context.Context, step, attempt string) error {
	args := append([]string{"finish-step", step}, c.rootArgs("--attempt", attempt, "--agent", agentName, "--json")...)
	_, err := c.call(ctx, "finish-step", args, true)
	return err
}

// FinishRun runs the terminal gates. On success it returns the deterministic
// proof-pack path; AgentFlow's finish-run JSON reports ok/stopped_at/gates, not
// the path. On a stop, it returns *FinishRunError with stopped_at/diagnostics
// parsed from stdout so recovery output is not lost.
func (c *Client) FinishRun(ctx context.Context) (string, error) {
	args := append([]string{"finish-run"}, c.rootArgs("--json")...)
	out, errb, exit, err := c.r.Run(ctx, args, nil)
	if err != nil {
		return "", err
	}
	var r finishRunResult
	if err := json.Unmarshal(out, &r); err != nil {
		if exit != 0 {
			return "", &CommandError{Cmd: "finish-run", Exit: exit, Stderr: string(errb)}
		}
		return "", fmt.Errorf("agentflow finish-run: parse %q: %w", out, err)
	}
	if exit != 0 || !r.OK {
		return "", &FinishRunError{StoppedAt: r.StoppedAt, Diagnostics: r.Diagnostics}
	}
	return filepath.Join(c.root, ".agent", "proof-pack.json"), nil
}

// NextActionState is the advisory recovery hint agentflow's next-action reports.
// It is printed, never executed: proof state stays adapter-driven.
type NextActionState struct {
	State       string   `json:"state"`
	Reason      string   `json:"reason"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Diagnostics []string `json:"diagnostics"`
}

// NextAction returns agentflow's advisory next-action state (recovery hint).
func (c *Client) NextAction(ctx context.Context) (NextActionState, error) {
	out, err := c.call(ctx, "next-action", append([]string{"next-action"}, c.rootArgs("--json")...), true)
	if err != nil {
		return NextActionState{}, err
	}
	var st NextActionState
	if err := json.Unmarshal(out, &st); err != nil {
		return NextActionState{}, fmt.Errorf("agentflow next-action: parse %q: %w", out, err)
	}
	return st, nil
}

// Status returns agentflow's status output verbatim (printed on recovery).
func (c *Client) Status(ctx context.Context) ([]byte, error) {
	return c.call(ctx, "status", append([]string{"status"}, c.rootArgs()...), false)
}

func (c *Client) RecordEvidence(ctx context.Context, e EvidenceEntry) error {
	args := append([]string{"record-evidence"}, c.rootArgs("--id", e.ID, "--claim", e.Claim, "--source", e.Source)...)
	if e.Confidence != "" {
		args = append(args, "--confidence", e.Confidence)
	}
	if e.Kind != "" {
		args = append(args, "--kind", e.Kind)
	}
	for _, support := range e.Supports {
		args = append(args, "--supports", support)
	}
	_, err := c.call(ctx, "record-evidence", args, false)
	return err
}
