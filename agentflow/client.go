package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const agentName = "golem"

var attemptIDPattern = regexp.MustCompile(`^(WT[a-z0-9]{1,16}-)?A[0-9]+$`)

// Client is a host-owned typed wrapper over the agentflow CLI.
type Client struct {
	r     Runner
	root  string
	agent string
}

// NewClient returns a Client that drives agentflow via r, with --root set to the
// workspace root for the subcommands that take it.
func NewClient(r Runner, root string) *Client { return &Client{r: r, root: root} }

// NewOwnedClient returns a Client whose actor-bearing commands use agent.
func NewOwnedClient(r Runner, root, agent string) *Client {
	return &Client{r: r, root: root, agent: agent}
}

func (c *Client) agentName() string {
	if c.agent != "" {
		return c.agent
	}
	return agentName
}

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

// AggregationInput identifies one worker ledger root with its stable source ID.
type AggregationInput struct {
	Root     string
	SourceID string
}

// AggregationResult is Agentflow's stable aggregation envelope. Its nested
// records remain raw because their shapes are intentionally additive.
type AggregationResult struct {
	Status     string            `json:"status"`
	Sources    []json.RawMessage `json:"sources"`
	Collisions []json.RawMessage `json:"collisions,omitempty"`
	Planned    json.RawMessage   `json:"planned,omitempty"`
	Written    json.RawMessage   `json:"written,omitempty"`
}

// AggregationCollisionError preserves Agentflow's collision report (exit 1).
type AggregationCollisionError struct{ Result AggregationResult }

func (e *AggregationCollisionError) Error() string {
	kinds := make([]string, 0, len(e.Result.Collisions))
	for _, raw := range e.Result.Collisions {
		var record struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &record); err != nil || strings.TrimSpace(record.Kind) == "" {
			kinds = append(kinds, "unknown")
			continue
		}
		kinds = append(kinds, record.Kind)
	}
	if len(kinds) == 0 {
		return "agentflow aggregate-ledgers: collision"
	}
	return "agentflow aggregate-ledgers: collision (" + strings.Join(kinds, ", ") + ")"
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

// ClaimStep claims step for this client's agent and returns the new attempt id.
func (c *Client) ClaimStep(ctx context.Context, step string) (string, error) {
	args := append([]string{"claim-step", step}, c.rootArgs("--agent", c.agentName(), "--json")...)
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
		"--agent", c.agentName(), "--reason", reason, "--reason-code", "review_feedback")...)
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
		c.rootArgs("--step", step, "--attempt", attempt, "--path", path, "--agent", c.agentName(), "--json")...)
	_, err := c.call(ctx, "record-file-change", args, true)
	return err
}

// RunGate runs the gate command argv (everything after the `--` separator)
// under agentflow's proof harness for (step, attempt), recording the result.
func (c *Client) RunGate(ctx context.Context, step, attempt, gate string, argv []string) error {
	args := append([]string{"run"},
		c.rootArgs("--step", step, "--attempt", attempt, "--gate", gate, "--agent", c.agentName(), "--confirm-risk", "--")...)
	args = append(args, argv...)
	_, err := c.call(ctx, "run", args, false)
	return err
}

// FinishStep closes the attempt, verifying its gates and file-change receipts;
// a *CommandError surfaces the diagnostics when the step is not yet complete.
func (c *Client) FinishStep(ctx context.Context, step, attempt string) error {
	args := append([]string{"finish-step", step}, c.rootArgs("--attempt", attempt, "--agent", c.agentName(), "--json")...)
	_, err := c.call(ctx, "finish-step", args, true)
	return err
}

// AggregateLedgers combines worker ledgers into this client's root. A valid
// collision report is returned as *AggregationCollisionError rather than being
// collapsed into CommandError, so callers can roll back promotion safely.
func (c *Client) AggregateLedgers(ctx context.Context, inputs []AggregationInput, base string, dryRun bool) (AggregationResult, error) {
	if len(inputs) == 0 {
		return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: at least one input is required")
	}
	if strings.TrimSpace(c.root) == "" || strings.TrimSpace(base) == "" {
		return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: output root and base are required")
	}
	args := []string{"aggregate-ledgers"}
	for _, input := range inputs {
		if strings.TrimSpace(input.Root) == "" || strings.TrimSpace(input.SourceID) == "" {
			return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: input root and source ID are required")
		}
		args = append(args, "--input", input.Root, "--source-id", input.SourceID)
	}
	args = append(args, "--output", c.root, "--base", base)
	if dryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, "--json")

	out, errb, exit, err := c.r.Run(ctx, args, nil)
	if err != nil {
		return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: %w", err)
	}
	result, fields, err := parseAggregationResult(out)
	if err != nil {
		if exit != 0 {
			return AggregationResult{}, &CommandError{Cmd: "aggregate-ledgers", Exit: exit, Stderr: string(errb)}
		}
		return AggregationResult{}, err
	}
	if exit != 0 && exit != 1 {
		return AggregationResult{}, &CommandError{Cmd: "aggregate-ledgers", Exit: exit, Stderr: string(errb)}
	}
	if err := validateAggregationResult(result, fields, dryRun); err != nil {
		return AggregationResult{}, err
	}
	if result.Status == "collision" {
		if exit != 1 {
			return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: collision status with exit %d", exit)
		}
		return result, &AggregationCollisionError{Result: result}
	}
	if exit != 0 {
		if detail := strings.TrimSpace(string(errb)); detail != "" {
			return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: status %q with exit %d: %s",
				result.Status, exit, detail)
		}
		return AggregationResult{}, fmt.Errorf("agentflow aggregate-ledgers: status %q with exit %d", result.Status, exit)
	}
	return result, nil
}

func parseAggregationResult(out []byte) (AggregationResult, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		return AggregationResult{}, nil, fmt.Errorf("agentflow aggregate-ledgers: parse %q: %w", out, err)
	}
	var result AggregationResult
	if err := json.Unmarshal(out, &result); err != nil {
		return AggregationResult{}, nil, fmt.Errorf("agentflow aggregate-ledgers: parse %q: %w", out, err)
	}
	return result, fields, nil
}

func validateAggregationResult(result AggregationResult, fields map[string]json.RawMessage, dryRun bool) error {
	analysis := result.Status == "collision" || dryRun
	if result.Status != "ok" && result.Status != "collision" {
		return fmt.Errorf("agentflow aggregate-ledgers: invalid status %q", result.Status)
	}
	required := []string{"status", "sources"}
	if analysis {
		required = append(required, "collisions", "planned")
		if _, exists := fields["written"]; exists {
			return fmt.Errorf("agentflow aggregate-ledgers: analysis result includes written")
		}
	} else {
		required = append(required, "written")
		if _, exists := fields["collisions"]; exists {
			return fmt.Errorf("agentflow aggregate-ledgers: write result includes collisions")
		}
		if _, exists := fields["planned"]; exists {
			return fmt.Errorf("agentflow aggregate-ledgers: write result includes planned")
		}
	}
	for _, name := range required {
		if _, exists := fields[name]; !exists {
			return fmt.Errorf("agentflow aggregate-ledgers: missing required field %s", name)
		}
	}
	if err := requireAggregationJSONKind(fields, "sources", '['); err != nil {
		return err
	}
	if analysis {
		if err := requireAggregationJSONKind(fields, "collisions", '['); err != nil {
			return err
		}
		return requireAggregationJSONKind(fields, "planned", '{')
	}
	return requireAggregationJSONKind(fields, "written", '{')
}

func requireAggregationJSONKind(fields map[string]json.RawMessage, name string, kind byte) error {
	value := strings.TrimSpace(string(fields[name]))
	if len(value) == 0 || value[0] != kind {
		return fmt.Errorf("agentflow aggregate-ledgers: field %s must be a JSON %s", name, map[byte]string{'[': "array", '{': "object"}[kind])
	}
	return nil
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
		diagnostics := append([]string(nil), r.Diagnostics...)
		if r.StoppedAt == "verify-proof" ||
			(r.StoppedAt == "build-proof" && slices.Contains(r.Diagnostics, "created .agent/proof-pack.json")) {
			diagnostics = append(diagnostics, c.failedProofCheckDiagnostics()...)
		}
		return "", &FinishRunError{StoppedAt: r.StoppedAt, Diagnostics: diagnostics}
	}
	return filepath.Join(c.root, ".agent", "proof-pack.json"), nil
}

// failedProofCheckDiagnostics reads only Agentflow's generated failed checks.
// The caller first confirms this finish-run wrote the pack, so an earlier pack
// cannot supply a stale reason for a pre-write build failure.
func (c *Client) failedProofCheckDiagnostics() []string {
	b, err := os.ReadFile(filepath.Join(c.root, ".agent", "proof-pack.json"))
	if err != nil {
		return nil
	}
	var proof struct {
		Checks []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(b, &proof); err != nil {
		return nil
	}
	var diagnostics []string
	for _, check := range proof.Checks {
		if check.Status != "failed" || strings.TrimSpace(check.Message) == "" {
			continue
		}
		message := check.Message
		if strings.TrimSpace(check.ID) != "" {
			message = check.ID + ": " + message
		}
		diagnostics = append(diagnostics, message)
	}
	return diagnostics
}

// NextActionState is the advisory recovery hint agentflow's next-action reports.
// It is printed, never executed: proof state stays adapter-driven.
type NextActionState struct {
	State        string                  `json:"state"`
	Reason       string                  `json:"reason"`
	Command      string                  `json:"command"`
	Args         []string                `json:"args"`
	Diagnostics  []string                `json:"diagnostics"`
	Resumability *ResumabilityProjection `json:"resumability"`
}

// ResumabilityProjection is the subset of next-action state required to prove
// an owned worktree has not started execution.
type ResumabilityProjection struct {
	Contract    *ResumabilityContract    `json:"contract"`
	AgentID     string                   `json:"agent_id"`
	Step        *ResumabilityStep        `json:"step"`
	Attempt     *ResumabilityAttempt     `json:"attempt"`
	Diagnostics []ResumabilityDiagnostic `json:"diagnostics"`

	attemptPresent     bool
	diagnosticsPresent bool
}

// UnmarshalJSON preserves whether fields required for fresh-worker validation
// were present, since JSON null and an omitted field otherwise decode alike.
func (p *ResumabilityProjection) UnmarshalJSON(data []byte) error {
	type projection ResumabilityProjection
	var decoded projection
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*p = ResumabilityProjection(decoded)
	_, p.attemptPresent = fields["attempt"]
	_, p.diagnosticsPresent = fields["diagnostics"]
	if raw, ok := fields["diagnostics"]; ok && strings.TrimSpace(string(raw)) == "null" {
		return errors.New("resumability diagnostics must be an array")
	}
	return nil
}

// HasAttemptField reports whether Agentflow explicitly projected attempt state.
func (p *ResumabilityProjection) HasAttemptField() bool { return p != nil && p.attemptPresent }

// HasDiagnosticsField reports whether Agentflow explicitly projected diagnostics.
func (p *ResumabilityProjection) HasDiagnosticsField() bool {
	return p != nil && p.diagnosticsPresent
}

// ResumabilityContract is the locked plan/execution contract pairing a
// projection proves its state against.
type ResumabilityContract struct {
	PlanSHA256              string `json:"plan_sha256"`
	Locked                  bool   `json:"locked"`
	ExecutionContractSHA256 string `json:"execution_contract_sha256"`
}

// ResumabilityStep is the projected execution state of one plan step.
// Completed is a pointer so an omitted field is distinguishable from false.
type ResumabilityStep struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Completed *bool  `json:"completed"`
}

// ResumabilityAttempt is a projected open or closed step attempt.
type ResumabilityAttempt struct {
	ID   string `json:"id"`
	Open bool   `json:"open"`
}

// ResumabilityDiagnostic is one projected execution diagnostic record.
type ResumabilityDiagnostic struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Artifact string `json:"artifact"`
}

// NextAction returns agentflow's advisory next-action state (recovery hint).
func (c *Client) NextAction(ctx context.Context) (NextActionState, error) {
	args := append([]string{"next-action"}, c.rootArgs("--json")...)
	if c.agent != "" {
		args = append(args, "--agent", c.agent)
	}
	out, err := c.call(ctx, "next-action", args, true)
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
