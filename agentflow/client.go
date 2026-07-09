package agentflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

const agentName = "golem"

// Client is a host-owned typed wrapper over the agentflow CLI.
type Client struct {
	r    Runner
	root string
}

func NewClient(r Runner, root string) *Client { return &Client{r: r, root: root} }

// call runs a subcommand and returns stdout, mapping exit!=0 (or a --json
// invalid envelope) to *CommandError. Pass wantJSON=true for --json commands.
func (c *Client) call(ctx context.Context, name string, args []string, wantJSON bool) ([]byte, error) {
	out, errb, exit, err := c.r.Run(ctx, args, nil)
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

func (c *Client) LockPlan(ctx context.Context, planPath string) error {
	_, err := c.call(ctx, "lock-plan",
		[]string{"lock-plan", "--from-json", planPath, "--json"}, true) // no --root
	return err
}

func (c *Client) Init(ctx context.Context) error {
	_, err := c.call(ctx, "init", append([]string{"init"}, c.rootArgs()...), false)
	return err
}

func (c *Client) InitExecution(ctx context.Context) error {
	_, err := c.call(ctx, "init-execution", append([]string{"init-execution"}, c.rootArgs()...), false)
	return err
}

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
		return "", err
	}
	return r.ID, nil
}

func (c *Client) ClaimStep(ctx context.Context, step string) (string, error) {
	args := append([]string{"claim-step", step}, c.rootArgs("--agent", agentName, "--json")...)
	out, err := c.call(ctx, "claim-step", args, true)
	if err != nil {
		return "", err
	}
	var r claimResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return r.AttemptID, nil
}

func (c *Client) RecordFileChange(ctx context.Context, step, attempt, path string) error {
	args := append([]string{"record-file-change"},
		c.rootArgs("--step", step, "--attempt", attempt, "--path", path, "--agent", agentName, "--json")...)
	_, err := c.call(ctx, "record-file-change", args, true)
	return err
}

func (c *Client) RunGate(ctx context.Context, step, attempt, gate string, argv []string) error {
	args := append([]string{"run"},
		c.rootArgs("--step", step, "--attempt", attempt, "--gate", gate, "--agent", agentName, "--confirm-risk", "--")...)
	args = append(args, argv...)
	_, err := c.call(ctx, "run", args, false)
	return err
}

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
		return "", err
	}
	if exit != 0 || !r.OK {
		return "", &FinishRunError{StoppedAt: r.StoppedAt, Diagnostics: r.Diagnostics}
	}
	return filepath.Join(c.root, ".agent", "proof-pack.json"), nil
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
