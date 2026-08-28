package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	// verifyToolName is the synthetic identity the post-write verification
	// command is approved under (#347). It is NOT a registered tool: the model
	// never sees it and cannot call it. The name exists so the approver can
	// render an action-appropriate prompt and so grantScope can give
	// verification its own grant namespace.
	verifyToolName = "verify_command"
	// verifyObservationCap bounds the captured output the MODEL reads, well
	// below the runner's own 32 KiB per-stream caps. A build or test failure
	// is actionable in its first lines; the rest is tokens the model pays for
	// and does not need.
	verifyObservationCap = 4 * 1024
	// Keep a long command from crowding failure status and diagnostics out of
	// the observation budget.
	verifyCommandCap = verifyObservationCap / 4

	verifyHeader = "\n--- post-batch verification (ran after all calls in this batch) ---\n"
)

// verifyExecutor is the bounded command the policy drives. The interface
// exists so the policy's approval and rendering can be tested without
// spawning a process; *agenttools.VerifyCommand is the production
// implementation.
type verifyExecutor interface {
	Command() string
	Preview() string
	ApprovalKey() string
	Run(ctx context.Context) (agenttools.VerifyResult, error)
}

// verifyRunner is golem's post-write verification policy: approve the exact
// workspace-declared command, run it bounded, and render the outcome for the
// model. It satisfies agent.Verifier.
type verifyRunner struct {
	cmd  verifyExecutor
	call provider.ToolCall
}

var (
	_ agent.Verifier = (*verifyRunner)(nil)
	// The production executor must keep satisfying the seam the policy drives.
	_ verifyExecutor = (*agenttools.VerifyCommand)(nil)
)

func newVerifyRunner(cmd verifyExecutor) *verifyRunner {
	return &verifyRunner{
		cmd: cmd,
		call: provider.ToolCall{
			Type: "function",
			Function: provider.ToolCallFunction{
				Name:      verifyToolName,
				Arguments: json.RawMessage(`{}`),
			},
		},
	}
}

// Verify implements agent.Verifier. Everything the CHECK decides — denial, a
// spawn failure, a non-zero exit, a timeout — is an observation with a nil
// error. Only an approval or control-plane failure and cancellation abort the
// run; see agent.Verifier for why that channel cannot be a ctx.Err() check.
func (v *verifyRunner) Verify(ctx context.Context, approver agent.Approver) (string, error) {
	if approver == nil {
		// Unreachable in golem: with no approver the runtime's fail-safe denies
		// every write, so no batch reaches verification. Another consumer of
		// the seam could still get here with auto-approved writes. Fail closed
		// — never run an unapproved command — but say so rather than going
		// silently missing, which would read as a clean check.
		return v.render("skipped", "reason: no approver available\n"), nil
	}
	d, err := approveVerify(ctx, approver, v.call, v.cmd.Preview(), v.cmd.ApprovalKey())
	if err != nil {
		return "", err
	}
	if !d.Approved {
		return v.render("skipped", "reason: not approved\n"), nil
	}

	res, runErr := v.cmd.Run(ctx)
	if runErr != nil {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) ||
			errors.Is(runErr, context.DeadlineExceeded) {
			return "", runErr
		}
		return v.render("error", "reason: "+sanitizeVerifyLine(runErr.Error())+"\n"), nil
	}
	return v.renderResult(res), nil
}

// approveVerify mirrors the runtime's own approval call: a KeyedApprover
// receives the structural key, a plain Approver does not (and is therefore
// never grantable).
func approveVerify(ctx context.Context, approver agent.Approver, call provider.ToolCall,
	preview, key string) (agent.ApprovalDecision, error) {

	if ka, ok := approver.(agent.KeyedApprover); ok {
		return ka.ApproveKeyed(ctx, call, preview, key)
	}
	ok, err := approver.Approve(ctx, call, preview)
	return agent.ApprovalDecision{Approved: ok}, err
}

// render builds the observation shell shared by every outcome.
func (v *verifyRunner) render(status, tail string) string {
	command, _ := capVerifyOutput(v.cmd.Command(), verifyCommandCap)
	out, _ := capVerifyOutput(
		fmt.Sprintf("%scommand: %s\nstatus: %s\n%s", verifyHeader, command, status, tail),
		verifyObservationCap,
	)
	return out
}

// renderResult renders a completed run. A pass carries status and exit code
// only: its output is noise the model pays for. A failure carries the bounded
// streams, because that is the whole point of the check.
func (v *verifyRunner) renderResult(res agenttools.VerifyResult) string {
	if res.ExitCode == 0 && !res.TimedOut {
		return v.render("passed", "exit_code: 0\n")
	}
	body, capped := verifyOutputBody(res)
	// exit_code is omitted on a timeout: the process was killed, so the
	// platform runner reports -1, and pairing "failed" with a numeric code the
	// command never chose invites the model to reason about it.
	head := fmt.Sprintf("exit_code: %d\n", res.ExitCode)
	if res.TimedOut {
		head = ""
	}
	truncated := res.Truncated || capped
	tail := fmt.Sprintf("%stimed_out: %t\noutput_truncated: %t\n", head, res.TimedOut, truncated)
	body, observationCapped := capVerifyOutput(body, verifyObservationCap-len(v.render("failed", tail)))
	if observationCapped {
		truncated = true
		tail = fmt.Sprintf("%stimed_out: %t\noutput_truncated: %t\n", head, res.TimedOut, truncated)
	}
	return v.render("failed", tail+body)
}

// verifyOutputBody lays out the captured streams, omitting an empty one so a
// failure is not padded with blank section headers.
//
// stderr comes FIRST and each stream is capped independently. Capping the
// joined body head-first would let a chatty-but-irrelevant stdout consume the
// whole budget and cut the stderr section off entirely — and stderr is exactly
// where compilers and test runners put the failure the check exists to
// surface. Splitting the budget only when both streams have content keeps the
// common single-stream case at full size.
func verifyOutputBody(res agenttools.VerifyResult) (string, bool) {
	budget := verifyObservationCap
	if len(res.Stdout) > 0 && len(res.Stderr) > 0 {
		budget /= 2
	}
	var b strings.Builder
	capped := false
	for _, section := range []struct {
		label string
		data  []byte
	}{{"stderr", res.Stderr}, {"stdout", res.Stdout}} {
		if len(section.data) == 0 {
			continue
		}
		text, cut := capVerifyOutput(string(section.data), budget)
		capped = capped || cut
		fmt.Fprintf(&b, "--- %s ---\n", section.label)
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), capped
}

// capVerifyOutput trims one stream to limit from the HEAD: a compiler or test
// runner reports its first failure first, and that is the actionable one. The
// cut lands on a rune boundary so the model never reads a split rune.
func capVerifyOutput(s string, limit int) (string, bool) {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= limit {
		return s, false
	}
	if limit <= 0 {
		return "", true
	}
	const suffix = "\n... [truncated]\n"
	if limit <= len(suffix) {
		return suffix[:limit], true
	}
	end := limit - len(suffix)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + suffix, true
}

// sanitizeVerifyLine keeps a failure reason to one line so it cannot forge
// additional observation fields.
func sanitizeVerifyLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
}

// buildVerifier resolves the workspace's post-write verification command
// (#347). It returns (nil, "") when the workspace declares none — the
// byte-for-byte unchanged path — and (nil, warning) when it declares one that
// cannot be armed, so a malformed or uninstalled verifier costs a startup line
// rather than the session.
//
// Callers must invoke it only while -allow-write is still true, which is AFTER
// mode normalization: applyOneShotMode clears the flag, and task, planning and
// Agentflow modes reject it outright, so that one condition is what keeps every
// non-interactive mode from reading the file at all.
func buildVerifier(root string) (*verifyRunner, string) {
	disabled := func(err error) (*verifyRunner, string) {
		return nil, "verification disabled: " + err.Error()
	}
	spec, err := loadVerifyConfig(root)
	if err != nil {
		return disabled(err)
	}
	if spec == nil {
		return nil, ""
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return disabled(err)
	}
	cmd, err := agenttools.NewVerifyCommand(ws, spec.Argv, spec.Dir, spec.Timeout())
	if err != nil {
		return disabled(err)
	}
	return newVerifyRunner(cmd), ""
}
