package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// verifyApprovalKeyPrefix namespaces post-write verification grants (#347) so
// a verify command's fingerprint can never be mistaken for an exec grant, and
// vice versa. The v1 tag names the recipe; changing it is an explicit
// migration.
//
// It is correct only while verification is host-only. If a non-host sandbox
// runtime is ever wired in here, this key must absorb an immutable digest of
// the sandbox policy first — the same rule bindExecBackend states for exec
// keys, whose non-host keys carry an "sb:<digest>:" component.
const verifyApprovalKeyPrefix = "verify:v1:"

// VerifyResult is one verification run's raw outcome. Rendering it for the
// model — including the model-visible output cap — is the consumer's job.
type VerifyResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Truncated reports that the runner capped EITHER stream. The two are
	// merged because the model-visible observation reports one
	// output_truncated fact; keeping them apart here would only mirror
	// execResult field for field and chain this exported type to an internal
	// wire struct.
	Truncated bool
	// TimedOut reports that the command exceeded the verifier's OWN deadline.
	// It is an outcome, not a failure: Run returns a nil error for it.
	TimedOut bool
}

// VerifyCommand is a prepared, bounded post-write verification command (#347).
//
// It reuses run_command's preparation wholesale — argv validation, cwd
// containment through the Workspace, the fixed environment allowlist,
// executable resolution and identity stamping, the spawn-time re-check, the
// process-group kill and the output caps — but it is NOT a model-visible tool:
// nothing registers it, and the model can neither call it nor see it.
//
// The plan is prepared ONCE, at construction, and frozen. Every Run re-checks
// it, so a cwd escape or a binary swap after the user approved the command
// fails closed instead of running something else.
type VerifyCommand struct {
	ws      *Workspace
	runner  commandRunner
	pending execPending
	argv0   string
}

// NewVerifyCommand prepares a verification command over ws. It resolves the
// executable immediately, so a command that is not installed fails here rather
// than once per batch.
//
// Execution goes through the #440 backend seam at the host runtime. Sandboxing
// is deliberately not a parameter: host-mode verification cannot enforce a
// read-only check, and giving callers a knob would imply otherwise.
func NewVerifyCommand(ws *Workspace, argv []string, dir string, timeout time.Duration) (*VerifyCommand, error) {
	backend, err := newExecBackend(SandboxConfig{})
	if err != nil {
		return nil, fmt.Errorf("tools: verify command backend: %w", err)
	}
	return newVerifyCommand(ws, backend, argv, dir, timeout)
}

// newVerifyCommand is the runner-injecting constructor the tests use.
func newVerifyCommand(ws *Workspace, runner commandRunner, argv []string, dir string,
	timeout time.Duration) (*VerifyCommand, error) {

	if timeout <= 0 || timeout > execMaxTimeout {
		return nil, fmt.Errorf("tools: verify timeout must be positive and at most %s, got %s",
			execMaxTimeout, timeout)
	}
	pending, err := prepareExecPlan(ws, argv, dir, timeout, int(timeout/time.Second), false)
	if err != nil {
		return nil, fmt.Errorf("tools: verify command: %w", err)
	}
	return &VerifyCommand{ws: ws, runner: runner, pending: pending, argv0: argv[0]}, nil
}

// ApprovalKey is the opaque structural identity of this exact command, for
// session-scoped approval grants (#341).
func (v *VerifyCommand) ApprovalKey() string {
	return verifyApprovalKeyPrefix + v.pending.fingerprint
}

// Preview is the human approval rendering: never parsed, never shown to the
// model, and it lists environment NAMES only, never values. The closing note
// is the honest statement of what approval buys — the argv is fixed, but the
// code it executes is whatever the workspace contains at run time.
func (v *VerifyCommand) Preview() string {
	dir := "(workspace root)"
	if v.pending.dirLabel != "" {
		dir = quoteArgForPreview(v.pending.dirLabel)
	}
	names := make([]string, len(v.pending.envNames))
	for i, n := range v.pending.envNames {
		names[i] = n + "(parent)"
	}
	id := v.pending.fingerprint
	if len(id) > fingerprintLen {
		id = id[:fingerprintLen]
	}

	var b strings.Builder
	b.WriteString("post-write verification command:\n")
	fmt.Fprintf(&b, "  argv:    %s\n", renderArgvForPreview(v.pending.argv))
	fmt.Fprintf(&b, "  exe:     %s -> %s\n", quoteArgForPreview(v.argv0), quoteArgForPreview(v.pending.path))
	fmt.Fprintf(&b, "  cwd:     %s\n", dir)
	fmt.Fprintf(&b, "  timeout: %s\n", fmtTimeout(v.pending.timeout))
	fmt.Fprintf(&b, "  env:     %s\n", strings.Join(names, ", "))
	fmt.Fprintf(&b, "  id:      %s\n", id)
	b.WriteString("  note:    declared by this workspace; later edits to files here\n")
	b.WriteString("           may change the code this command runs\n")
	return b.String()
}

// Run re-checks the frozen plan and spawns the command under the verifier's
// own deadline.
//
// The deadline must be applied HERE. A dispatched tool gets one from the
// Orchestrator's invokeCall; nothing wraps a verification run, so without this
// the command would be unbounded.
//
// A non-zero exit and the verifier's own timeout are outcomes and return a nil
// error. Parent cancellation, a parent deadline, and infra failures (spawn
// error, identity re-check failure) return an error for the consumer to
// classify.
func (v *VerifyCommand) Run(ctx context.Context) (VerifyResult, error) {
	spec, err := recheckExecPlan(v.ws, v.pending)
	if err != nil {
		return VerifyResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, v.pending.timeout)
	defer cancel()

	res, runErr := v.runner.Run(cctx, spec)
	out := VerifyResult{
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.StdoutTruncated || res.StderrTruncated,
		TimedOut:  res.TimedOut,
	}
	if runErr == nil {
		return out, nil
	}
	// res.TimedOut with a live parent is OUR deadline; with a dead parent the
	// cancellation is the caller's and must not be reported as a check outcome.
	if res.TimedOut && ctx.Err() == nil {
		return out, nil
	}
	return VerifyResult{}, runErr
}
