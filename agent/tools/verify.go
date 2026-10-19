package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// verifyApprovalKeyPrefix namespaces post-write verification grants (#347) so
// a verify key can never be mistaken for an exec grant, and vice versa. The v2
// tag (#576) names the recipe for every runtime, host included:
// verifyKeyDigest over the complete normalized sandboxFingerprint and the
// prepared-command fingerprint (canonical root, argv, resolved executable,
// cwd, environment values, timeout). v1 keyed the command fingerprint alone,
// which was sound only while verification was host-only; v1 grants are not
// reusable. Changing the recipe is an explicit migration. The key binds no
// script bytes: an approved command still runs whatever the workspace holds.
const verifyApprovalKeyPrefix = "verify:v2:"

// verifyKeyDigest is the labeled, NUL-terminated digest behind a verify:v2
// key: hex sha256("sandbox\0" + sandboxDigest + "\0command\0" + commandDigest + "\0").
func verifyKeyDigest(sandboxDigest, commandDigest string) string {
	h := sha256.New()
	for _, s := range []string{"sandbox", sandboxDigest, "command", commandDigest} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

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
	// Notes are backend diagnostics for this run (#576): the Seatbelt loader
	// note on a failed run, a refused private-temp cleanup. Run fills them
	// alongside an infra error too; they never change ExitCode or TimedOut.
	Notes []string
}

// VerifyCommand is a prepared, bounded post-write verification command (#347).
//
// It reuses run_command's preparation wholesale — argv validation, cwd
// containment through the Workspace, the fixed environment allowlist,
// executable resolution and identity stamping, the pre-launch re-check, the
// process-group kill and the output caps — but it is NOT a model-visible tool:
// nothing registers it, and the model can neither call it nor see it.
//
// The plan is prepared ONCE, at construction, and frozen. Every Run re-checks
// it before launch, so a cwd escape or a binary swap between approval and
// that check fails closed. A swap after the check, or an in-place rewrite,
// remains the documented launch residual (#484).
type VerifyCommand struct {
	ws      *Workspace
	runner  commandRunner
	sandbox SandboxConfig // normalized; its approval metadata is pending.sandbox
	pending execPending
	argv0   string
}

// NewVerifyCommand prepares a host verification command over ws. It is
// NewSandboxedVerifyCommand with the host policy, SandboxConfig{}.
func NewVerifyCommand(ws *Workspace, argv []string, dir string, timeout time.Duration) (*VerifyCommand, error) {
	return NewSandboxedVerifyCommand(ws, SandboxConfig{}, argv, dir, timeout)
}

// NewSandboxedVerifyCommand prepares a verification command over ws that
// runs under the isolation runtime selected by sandbox (#576), through the
// same backend seam and normalizer as run_command. It resolves the executable
// immediately, so a command that is not installed fails here rather than once
// per batch.
//
// A policy that cannot be honored fails before any preview exists, with an
// error wrapping ErrSandboxPolicy; there is no host fallback. A native
// verifier is not a read-only check: it can write inside the workspace and
// its private temp, outside /undo, and its preview says so.
func NewSandboxedVerifyCommand(ws *Workspace, sandbox SandboxConfig, argv []string, dir string,
	timeout time.Duration) (*VerifyCommand, error) {

	backend, err := newExecBackend(sandbox)
	if err != nil {
		return nil, fmt.Errorf("tools: verify command backend: %w: %w", ErrSandboxPolicy, err)
	}
	return newVerifyCommandWithBackend(ws, backend, argv, dir, timeout)
}

// newVerifyCommandWithBackend is the backend-injecting constructor the tests
// use. backend must come from newExecBackend or bindExecBackend over a
// normalized config.
func newVerifyCommandWithBackend(ws *Workspace, backend resolvedExecBackend, argv []string, dir string,
	timeout time.Duration) (*VerifyCommand, error) {

	if err := backend.approval.validateWorkspace(ws.root); err != nil {
		return nil, fmt.Errorf("tools: verify command: %w: %w", ErrSandboxPolicy, err)
	}
	if timeout <= 0 || timeout > execMaxTimeout {
		return nil, fmt.Errorf("tools: verify timeout must be positive and at most %s, got %s",
			execMaxTimeout, timeout)
	}
	pending, err := prepareExecPlan(ws, argv, dir, timeout, int(timeout/time.Second), false)
	if err != nil {
		return nil, fmt.Errorf("tools: verify command: %w", err)
	}
	pending.sandbox = backend.approval
	return &VerifyCommand{ws: ws, runner: backend, sandbox: backend.sandbox, pending: pending, argv0: argv[0]}, nil
}

// ApprovalKey is the opaque structural identity of this exact command under
// its frozen sandbox policy, for session-scoped approval grants (#341).
func (v *VerifyCommand) ApprovalKey() string {
	return verifyApprovalKeyPrefix + verifyKeyDigest(sandboxFingerprint(v.sandbox), v.pending.fingerprint)
}

// Sandbox returns a copy of the normalized policy this command runs under.
// Mutating the result never reaches the command.
func (v *VerifyCommand) Sandbox() SandboxConfig {
	s := v.sandbox
	s.DropCaps = slices.Clone(s.DropCaps)
	return s
}

// Root is the canonical workspace root the command is bound to and checks.
func (v *VerifyCommand) Root() string { return v.ws.root }

// TargetLine names what a verification run checks, for the approval preview
// and the model-visible result: the canonical workspace, never an unpromoted
// scratch snapshot or artifact.
func (v *VerifyCommand) TargetLine() string {
	return "target:  " + quoteArgForPreview(v.ws.root) +
		" (canonical workspace; unpromoted scratch artifacts are not checked)"
}

// Command renders the argv for the model-visible observation. It is the
// display form, quoted so argument boundaries are unambiguous; it is never
// parsed and never used to spawn anything.
func (v *VerifyCommand) Command() string {
	return renderArgvForPreview(v.pending.argv)
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
	if v.pending.sandbox.preview != "" {
		fmt.Fprintf(&b, "  sandbox: %s\n", v.pending.sandbox.preview)
	}
	fmt.Fprintf(&b, "  %s\n", v.TargetLine())
	fmt.Fprintf(&b, "  id:      %s\n", id)
	if v.pending.sandbox.preview == "" {
		b.WriteString("  warning: unsandboxed; runs with the host user's filesystem and network authority\n")
		b.WriteString("           verifier writes are outside /undo\n")
	} else {
		b.WriteString("  warning: writes inside the workspace and private temp are outside /undo\n")
	}
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
// classify; once the command was launched, the result returned with that
// error still carries its Notes.
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
		Notes:     res.Notes,
	}
	if runErr == nil {
		return out, nil
	}
	// res.TimedOut with a live parent is OUR deadline; with a dead parent the
	// cancellation is the caller's and must not be reported as a check outcome.
	if res.TimedOut && ctx.Err() == nil {
		return out, nil
	}
	return out, runErr
}
