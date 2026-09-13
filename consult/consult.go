package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
	"unicode/utf8"
)

// ContentForm identifies the frozen consult answer bytes (#450 convention).
const ContentForm = "consult-result/v1"

// Receipt is the unsigned, host-authored result of one consultation. It is
// in-process only; nothing in it comes from consultant text except Answer,
// which has passed admission and control-byte sanitization.
type Receipt struct {
	Consultant    string
	Adapter       string
	Version       string // the init's claude_code_version, pinned
	Model         string
	ExitCode      int
	Duration      time.Duration
	ContentForm   string // ContentForm
	ContentSHA256 string // sha256 of Answer at freeze, before any annotation or framing
	Answer        string
	Evidence      Evidence
}

// Evidence is the redacted admission record kept on the receipt: fixed
// literals, counts and booleans only, never vendor text.
type Evidence struct {
	WaitStatus       string
	WaitErrorKind    string
	InitAPIKeySource string
	AgentCount       int
	RateLimitEvents  int
	OverageAttested  bool
	APIRetryCount    int
	UsageComplete    bool
	WebSearchZero    bool
	OpusInputTokens  int64
	OpusOutputTokens int64
}

// Error is a fixed-category consult failure. Code is one of auth, quota,
// billing, tool-activity, protocol, process-exit, timeout, canceled,
// output-limit, drain-incomplete, unsupported-version, target-drift,
// target-invalid, input-invalid, unsupported-platform. Reason is the first
// fixed admission literal, never vendor text.
type Error struct{ Code, Reason string }

// Error renders only the two fixed literals: a wrapped runner error can carry
// a filesystem path and a consultant stream can carry attacker-chosen text,
// so neither is ever formatted into this message.
func (e *Error) Error() string { return "consult: " + e.Code + " (" + e.Reason + ")" }

// runWaitDelay is a test seam: the grace period after cancellation before Wait
// abandons pipe I/O. Zero, the only value in production, means the runner's
// own default; the drain-incomplete test shortens it so a held pipe does not
// cost five seconds.
var runWaitDelay time.Duration

// Run consults c once with prompt on stdin and returns the receipt. It is the
// only entry point that executes a consultant: the adapter owns argv, the
// runner owns the process envelope and the admission catalog owns the verdict.
// On any failure the receipt is zero and the error is an *Error.
func Run(ctx context.Context, c Consultant, prompt string) (Receipt, error) {
	// Consultant is a plain exported struct, so a caller can hand Run a value
	// that never passed Load's validation and therefore carries neither its
	// bounds nor its defaults. Re-check everything the run depends on: a zero
	// timeout or output cap would otherwise surface as an opaque start failure.
	if c.Adapter != claudeAdapter || !claudeModels[c.Model] ||
		c.TimeoutSeconds <= 0 || c.TimeoutSeconds > maxTimeoutSeconds ||
		c.MaxOutputBytes <= 0 || c.MaxOutputBytes > maxOutputBytes {
		return Receipt{}, &Error{Code: "input-invalid", Reason: "consultant"}
	}
	if prompt == "" || len(prompt) > maxStdinBytes || !utf8.ValidString(prompt) {
		return Receipt{}, &Error{Code: "input-invalid", Reason: "prompt"}
	}
	start := time.Now()
	out, err := run(ctx, runSpec{
		command: c.Command, sha256: c.SHA256, args: claudeArgs(c.Model), stdin: prompt,
		timeout: time.Duration(c.TimeoutSeconds) * time.Second, outputCap: c.MaxOutputBytes,
		waitDelay: runWaitDelay,
	})
	if err != nil {
		return Receipt{}, classifyRunError(err)
	}
	switch {
	case out.CapExceeded:
		return Receipt{}, &Error{Code: "output-limit", Reason: "cap"}
	case out.Canceled:
		return Receipt{}, &Error{Code: "canceled", Reason: "caller"}
	case out.TimedOut:
		return Receipt{}, &Error{Code: "timeout", Reason: "deadline"}
	case !out.GroupCleanupOK || !out.CleanupOK:
		return Receipt{}, &Error{Code: "process-exit", Reason: "cleanup"}
	case out.WaitErrorKind == "wait-delay" || out.WaitErrorKind == "other":
		return Receipt{}, &Error{Code: "drain-incomplete", Reason: out.WaitErrorKind}
	case out.ExitCode != 0:
		return Receipt{}, &Error{Code: "process-exit", Reason: out.WaitStatus}
	}
	in, reasons := inspectStream(out.Stdout, prompt, out.Cwds)
	// inspectStream fails an unpinned init itself; this is the same gate held
	// independently of that ordering, so an unsupported version is reported as
	// such even when another literal was recorded first.
	if in.Version != "" && !claudeSupportedVersions[in.Version] {
		reasons = append([]string{"version-mismatch"}, dropReason(reasons, "version-mismatch")...)
	}
	if len(reasons) != 0 {
		return Receipt{}, &Error{Code: codeFor(reasons[0]), Reason: reasons[0]}
	}
	return Receipt{
		Consultant: c.Name, Adapter: c.Adapter, Version: in.Version, Model: c.Model,
		ExitCode: out.ExitCode, Duration: time.Since(start),
		ContentForm: ContentForm, ContentSHA256: digest(in.Answer), Answer: in.Answer,
		Evidence: Evidence{
			WaitStatus: out.WaitStatus, WaitErrorKind: out.WaitErrorKind,
			InitAPIKeySource: in.InitAPIKeySource, AgentCount: in.AgentCount,
			RateLimitEvents: in.RateLimitEvents, OverageAttested: in.OverageAttested,
			APIRetryCount: in.APIRetryCount, UsageComplete: in.UsageComplete,
			WebSearchZero: in.WebSearchZero, OpusInputTokens: in.OpusInputTokens,
			OpusOutputTokens: in.OpusOutputTokens,
		},
	}, nil
}

// dropReason removes one literal so a prepended reason cannot appear twice.
func dropReason(reasons []string, drop string) []string {
	out := reasons[:0:0]
	for _, r := range reasons {
		if r != drop {
			out = append(out, r)
		}
	}
	return out
}

// codeFor maps a fixed admission literal to a fixed error code. Anything
// unmapped is "protocol".
func codeFor(reason string) string {
	switch reason {
	case "version-mismatch":
		return "unsupported-version"
	case "auth-source-invalid", "api-retry-authentication_failed", "api-retry-oauth_org_not_allowed", "api-retry-account_on_hold":
		return "auth"
	case "quota-rejected", "api-retry-excess", "api-retry-rate_limit":
		return "quota"
	case "overage-in-use", "credits-required", "overage-not-rejected", "api-retry-billing_error", "route-invalid":
		return "billing"
	case "tool-activity", "startup-activity", "task-activity", "subagent-activity", "memory-activity",
		"elicitation-activity", "denial-activity", "web-search-activity", "compaction-activity", "fallback-activity":
		return "tool-activity"
	}
	return "protocol"
}

// classifyRunError maps a runner failure to a fixed code. The wrapped error is
// deliberately dropped: it can carry the consultant's filesystem path.
func classifyRunError(err error) *Error {
	switch {
	case errors.Is(err, errTargetDrift):
		return &Error{Code: "target-drift", Reason: "target"}
	case errors.Is(err, errTargetInvalid):
		return &Error{Code: "target-invalid", Reason: "target"}
	case errors.Is(err, errStdinInvalid):
		return &Error{Code: "input-invalid", Reason: "prompt"}
	case errors.Is(err, errors.ErrUnsupported):
		return &Error{Code: "unsupported-platform", Reason: "platform"}
	}
	return &Error{Code: "process-exit", Reason: "start"}
}

// digest is the lowercase hex SHA-256 of s, the form #450 records.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
