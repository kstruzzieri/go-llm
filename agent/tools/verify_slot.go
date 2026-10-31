package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// VerifyToolName is the synthetic identity the post-write verification
// command is approved under (#347). It is NOT a registered tool: the model
// never sees it and cannot call it. The name exists so an approver can render
// an action-appropriate prompt and give verification its own grant namespace.
const VerifyToolName = "verify_command"

const (
	// verifyObservationCap bounds the captured output the MODEL reads, well
	// below the runner's own 32 KiB per-stream caps. A build or test failure
	// is actionable in its first lines; the rest is tokens the model pays for
	// and does not need.
	verifyObservationCap = 4 * 1024
	// Keep a long command or target from crowding failure status and
	// diagnostics out of the observation budget.
	verifyCommandCap = verifyObservationCap / 4

	verifyHeader = "\n--- post-batch verification (ran after all calls in this batch) ---\n"
)

// ErrVerifierBinding reports a verify command built for another workspace
// root or sandbox policy than the slot it was offered to (#576).
var ErrVerifierBinding = errors.New("tools: verify command does not match the session root or sandbox policy")

// verifyExecutor is the bounded command the slot drives. The interface exists
// so approval and rendering can be tested without spawning a process;
// *VerifyCommand is the production implementation.
type verifyExecutor interface {
	Preview() string
	ApprovalKey() string
	Command() string
	TargetLine() string
	Run(ctx context.Context) (VerifyResult, error)
}

var _ verifyExecutor = (*VerifyCommand)(nil)

// VerifierSlot is the session's post-write verification hook (#372, #576): an
// agent.Verifier bound at construction to one canonical workspace root and
// one normalized sandbox policy. It starts absent and is armed only with a
// VerifyCommand built for exactly that root and policy, so a verifier can
// never carry another runtime's authority into the session. It owns the whole
// flow: approval of the exact command, bounded execution, cancellation
// classification and the model-visible observation.
//
// Unarmed (absent or disabled), Verify performs no verification and returns
// "" (agent.verifyBatch appends nothing). Every method is safe on a nil
// receiver, which behaves as an absent slot bound to nothing.
type VerifierSlot struct {
	root   string        // canonical; immutable
	policy SandboxConfig // normalized; immutable

	mu       sync.Mutex
	exec     verifyExecutor
	disabled string
}

var _ agent.Verifier = (*VerifierSlot)(nil)

// NewVerifierSlot returns an absent slot bound to root, canonicalized as a
// Workspace does, and to policy, normalized as an exec backend does. It
// probes no backend: a policy this OS cannot honor surfaces when the command
// is built.
func NewVerifierSlot(root string, policy SandboxConfig) (*VerifierSlot, error) {
	canon, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		return nil, fmt.Errorf("tools: verifier slot: %w", err)
	}
	normalized, err := normalizeSandboxConfig(policy)
	if err != nil {
		return nil, fmt.Errorf("tools: verifier slot: %w: %w", ErrSandboxPolicy, err)
	}
	return &VerifierSlot{root: canon, policy: normalized}, nil
}

// Accepts reports whether cmd is bound to this slot's root and policy. A nil
// cmd (or a nil slot) is accepted as the no-op Set treats it as; a mismatch
// returns ErrVerifierBinding. It never changes the slot.
func (s *VerifierSlot) Accepts(cmd *VerifyCommand) error {
	if s == nil || cmd == nil {
		return nil
	}
	if cmd.Root() != s.root || sandboxFingerprint(cmd.Sandbox()) != sandboxFingerprint(s.policy) {
		return ErrVerifierBinding
	}
	return nil
}

// Set arms the slot with cmd after Accepts, clearing any disabled state. A nil
// cmd (or a nil slot) is a no-op; a refused cmd leaves the slot unchanged.
func (s *VerifierSlot) Set(cmd *VerifyCommand) error {
	if s == nil || cmd == nil {
		return nil
	}
	if err := s.Accepts(cmd); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec, s.disabled = cmd, ""
	return nil
}

// Disable disarms the slot and records the repository-config notice
// ("verification disabled: ...") that explains why. reason must be non-empty:
// an empty reason leaves the slot absent.
func (s *VerifierSlot) Disable(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec, s.disabled = nil, reason
}

// State reports "armed", "disabled" or "absent", and the canonical root every
// verification targets. A nil slot reports ("absent", "").
func (s *VerifierSlot) State() (state, target string) {
	if s == nil {
		return "absent", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.exec != nil:
		return "armed", s.root
	case s.disabled != "":
		return "disabled", s.root
	default:
		return "absent", s.root
	}
}

// BoundTo reports whether the slot was constructed for exactly root, which
// must be canonical as Root and State report it, and policy, compared after
// normalization. A nil slot, or a policy that does not normalize, is bound to
// nothing.
func (s *VerifierSlot) BoundTo(root string, policy SandboxConfig) bool {
	if s == nil {
		return false
	}
	normalized, err := normalizeSandboxConfig(policy)
	if err != nil {
		return false
	}
	return root == s.root && sandboxFingerprint(normalized) == sandboxFingerprint(s.policy)
}

// Verify implements agent.Verifier. Unarmed, it returns "", nil. Armed,
// everything the CHECK decides — denial, a spawn failure, a non-zero exit, a
// timeout — is an observation with a nil error. Only an approval or
// control-plane failure and cancellation abort the run; see agent.Verifier
// for why that channel cannot be a ctx.Err() check.
func (s *VerifierSlot) Verify(ctx context.Context, approver agent.Approver) (string, error) {
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	e := s.exec
	s.mu.Unlock()
	if e == nil {
		return "", nil
	}
	if approver == nil {
		// Unreachable in golem: with no approver the runtime's fail-safe denies
		// every write, so no batch reaches verification. Another consumer of
		// the seam could still get here with auto-approved writes. Fail closed
		// — never run an unapproved command — but say so rather than going
		// silently missing, which would read as a clean check.
		return verifyRender(e, "skipped", "reason: no approver available\n"), nil
	}
	call := provider.ToolCall{
		Type: "function",
		Function: provider.ToolCallFunction{
			Name:      VerifyToolName,
			Arguments: json.RawMessage(`{}`),
		},
	}
	d, err := approveVerify(ctx, approver, call, e.Preview(), e.ApprovalKey())
	if err != nil {
		return "", err
	}
	if !d.Approved {
		return verifyRender(e, "skipped", "reason: not approved\n"), nil
	}

	res, runErr := e.Run(ctx)
	if runErr != nil {
		if ctx.Err() != nil || errors.Is(runErr, context.Canceled) ||
			errors.Is(runErr, context.DeadlineExceeded) {
			return "", runErr
		}
		return verifyRender(e, "error", "reason: "+sanitizeVerifyLine(runErr.Error())+"\n"+renderNotes(res.Notes)), nil
	}
	return verifyRenderResult(e, res), nil
}

// approveVerify mirrors the runtime's own approval call: a KeyedApprover
// receives the structural key, a plain Approver does not (and is therefore
// never grantable). The preview is sanitized here too: the verifier approves
// outside the orchestrator's own approval call, so it does not pass through
// agent.approve.
func approveVerify(ctx context.Context, approver agent.Approver, call provider.ToolCall,
	preview, key string) (agent.ApprovalDecision, error) {

	preview = agent.SanitizePreview(preview)
	if ka, ok := approver.(agent.KeyedApprover); ok {
		return ka.ApproveKeyed(ctx, call, preview, key)
	}
	ok, err := approver.Approve(ctx, call, preview)
	return agent.ApprovalDecision{Approved: ok}, err
}

// verifyRender builds the observation shell shared by every outcome. The
// target line names the canonical workspace the check ran against (#576).
func verifyRender(e verifyExecutor, status, tail string) string {
	command, _ := capVerifyOutput(e.Command(), verifyCommandCap)
	target, _ := capVerifyOutput(e.TargetLine(), verifyCommandCap)
	out, _ := capVerifyOutput(
		fmt.Sprintf("%scommand: %s\n%s\nstatus: %s\n%s", verifyHeader, command, target, status, tail),
		verifyObservationCap,
	)
	return out
}

// verifyRenderResult renders a completed run. A pass carries status, exit
// code and backend notes only: its output is noise the model pays for, but a
// cleanup failure must not hide behind a pass. A failure carries the notes
// and then the bounded streams, because that is the whole point of the check.
func verifyRenderResult(e verifyExecutor, res VerifyResult) string {
	notes := renderNotes(res.Notes)
	if res.ExitCode == 0 && !res.TimedOut {
		return verifyRender(e, "passed", "exit_code: 0\n"+notes)
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
	tail := fmt.Sprintf("%stimed_out: %t\noutput_truncated: %t\n", head, res.TimedOut, truncated) + notes
	body, observationCapped := capVerifyOutput(body, verifyObservationCap-len(verifyRender(e, "failed", tail)))
	if observationCapped {
		truncated = true
		tail = fmt.Sprintf("%stimed_out: %t\noutput_truncated: %t\n", head, res.TimedOut, truncated) + notes
	}
	return verifyRender(e, "failed", tail+body)
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
func verifyOutputBody(res VerifyResult) (string, bool) {
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
