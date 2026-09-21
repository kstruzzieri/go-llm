package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// ContentForm is the frozen-content tag recorded alongside ContentSHA256: it
// names which bytes the digest covers, so a later verifier reconstructs the
// same input. It is the #450 convention, and v1 is the sanitized answer text
// with nothing appended.
const ContentForm = "consult-result/v1"

// This pin belongs to the App Server wire/profile contract, independently of exec.
const codexAppServerVersion = "0.153.4"

// Receipt is the unsigned, host-authored result of one consultation. It is
// in-process only; nothing in it comes from consultant text except Answer,
// which has passed admission and control-byte sanitization.
type Receipt struct {
	Consultant string // the configured consultant name
	Adapter    string // the adapter that owned argv, "claude" or "codex"
	Version    string // pinned Claude init version or Codex preflight version
	Model      string // the configured model, not one echoed by the consultant
	// ExitCode is always 0 here: a non-zero exit is a process-exit error and
	// never yields a receipt. It is retained so a caller need not assume that.
	ExitCode int
	// Duration is end-to-end wall clock measured in Run: the version probe,
	// both envelopes and target checks, the prompt invocation, process-group
	// cleanup and transcript parsing. It is not the consultant's service time.
	Duration      time.Duration
	ContentForm   string // ContentForm
	ContentSHA256 string // sha256 of Answer at freeze, before any annotation or framing
	Answer        string // the admitted reply, control-sanitized, at most 64 KiB
	Evidence      Evidence
}

// Evidence is the redacted admission record kept on the receipt: fixed
// literals, counts and booleans only, never vendor text. Every field but the
// wait fields, transport and trust acknowledgement is computed from the admitted
// transcript. Claude-only fields are inapplicable for Adapter "codex".
type Evidence struct {
	// TrustedVendorRuntime records the Codex opt-in. It never attests host isolation.
	TrustedVendorRuntime bool
	// CodexTransport is "app-server" for that explicit transport; empty retains
	// the legacy exec/Claude evidence values.
	CodexTransport string
	// CodexAppServerUsagePresent distinguishes validated App Server usage from
	// unavailable usage. Absent usage leaves this false and all counters zero.
	CodexAppServerUsagePresent bool
	// Codex token counts are vendor-reported, not billing or serving-model proof.
	// These fields are zero for Claude; cache-write defaults to zero when omitted.
	CodexInputTokens           int64
	CodexCachedInputTokens     int64
	CodexCacheWriteInputTokens int64
	CodexOutputTokens          int64
	CodexReasoningOutputTokens int64
	// WaitStatus is the leader's fixed termination literal, exited(N) or
	// signaled(NAME); WaitErrorKind is none|exit|wait-delay|other, where only
	// the first two mean the output pipes were drained to EOF.
	WaitStatus    string
	WaitErrorKind string
	// InitAPIKeySource is the init's credential origin bucket
	// (none|env|helper|managed|legacy|missing|other); only none is admitted.
	InitAPIKeySource string
	// AgentCount is how many subagents the init declared, all of which had to
	// be documented built-ins to be admitted.
	AgentCount int
	// RateLimitEvents counts rate_limit_event records; OverageAttested is true
	// only when at least one appeared and every one positively attested that
	// no overage was in use.
	RateLimitEvents int
	OverageAttested bool
	// APIRetryCount counts system/api_retry records; only the transport-level
	// ones (overloaded, server_error, rate_limit) are admitted at all.
	APIRetryCount int
	// UsageComplete is true when every per-model usage entry carried all five
	// token fields; WebSearchZero is true when every entry reported zero web
	// search requests. Both are false when no usage block was present.
	UsageComplete bool
	WebSearchZero bool
	// OpusInputTokens and OpusOutputTokens are the summed per-model totals for
	// opus models, the only models an admitted transcript may report.
	OpusInputTokens  int64
	OpusOutputTokens int64
}

// Error is a fixed-category consult failure. Code is one of auth, quota,
// billing, tool-activity, protocol, process-exit, timeout, canceled,
// output-limit, drain-incomplete, unsupported-version, target-drift,
// target-invalid, input-invalid, internal, unsupported-platform. Only internal
// reports a host defect rather than something about the consultant or its
// answer.
//
// Reason narrows the code and is drawn from closed vocabularies, never
// from consultant text, a filesystem path or any other vendor string:
//
//   - host literals, when Run itself refused, the host failed, or the runner
//     reported a bounded termination: consultant, prompt, cap, caller,
//     deadline, cleanup, envelope, identity, target, platform, start,
//     wait-delay, other;
//   - the first admission literal from the version probe or protocol.go
//     (auth, quota, billing, tool-activity, protocol and unsupported-version)
//     — for example auth-source-invalid, version-mismatch or quota-rejected;
//   - the fixed codex-* reason selected by Codex admission precedence;
//   - for a non-zero exit, the runner's own termination literal, exited(N) or
//     signaled(SIGKILL|SIGTERM|SIGINT|other).
type Error struct{ Code, Reason string }

// Error renders only the two fixed literals: a wrapped runner error can carry
// a filesystem path and a consultant stream can carry attacker-chosen text,
// so neither is ever formatted into this message.
func (e *Error) Error() string { return "consult: " + e.Code + " (" + e.Reason + ")" }

// runWaitDelay is a test seam: the grace period after cancellation before Wait
// abandons pipe I/O. Zero, the only value in production, means the runner's
// own default; the drain-incomplete test shortens it so a held pipe does not
// cost five seconds. It is package-level state, so no test in this package may
// call t.Parallel while it is overridden.
var runWaitDelay time.Duration

// Run consults c once with prompt on stdin and returns the receipt. It is the
// only entry point that executes a consultant: the adapter owns argv, the
// runner owns the process envelope and the admission catalog owns the verdict.
// On any failure the receipt is zero and the error is an *Error.
func Run(ctx context.Context, c Consultant, prompt string) (Receipt, error) {
	// Bound the allocation before snapshotting caller-owned options. Preflight
	// and later launches must use the same names validated at entry.
	if len(c.DisabledMCPServers) > maxDisabledMCPServers {
		return Receipt{}, &Error{Code: "input-invalid", Reason: "consultant"}
	}
	c.DisabledMCPServers = append([]string(nil), c.DisabledMCPServers...)
	// Revalidate exported declarations even when the caller bypasses Load.
	if !nameRE.MatchString(c.Name) || !supportedModel(c.Adapter, c.Model) || !supportedTransport(c.Adapter, c.Transport) ||
		!validDisabledMCPServers(c.Adapter, c.Transport, c.DisabledMCPServers) ||
		c.TimeoutSeconds <= 0 || c.TimeoutSeconds > maxTimeoutSeconds ||
		c.MaxOutputBytes <= 0 || c.MaxOutputBytes > maxOutputBytes ||
		!c.TrustedProcessEgress || (c.SHA256 != "" && !shaRE.MatchString(c.SHA256)) ||
		(c.Adapter == codexAdapter && !c.TrustedVendorRuntime) {
		return Receipt{}, &Error{Code: "input-invalid", Reason: "consultant"}
	}
	if prompt == "" || len(prompt) > maxStdinBytes || !utf8.ValidString(prompt) {
		return Receipt{}, &Error{Code: "input-invalid", Reason: "prompt"}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	spec := runSpec{adapter: c.Adapter, command: c.Command, sha256: c.SHA256, args: []string{"--version"},
		timeout: time.Duration(c.TimeoutSeconds) * time.Second, outputCap: min(c.MaxOutputBytes, 4096), waitDelay: runWaitDelay}
	// One digest-bound version preflight and one prompt share a deadline.
	out, err := checkedRun(ctx, spec)
	if err != nil {
		return Receipt{}, err
	}
	var version string
	var versionOK bool
	switch c.Adapter {
	case codexAdapter:
		version = codexVersion
		if c.Transport == "app-server" {
			version = codexAppServerVersion
		} else {
			spec.args = codexArgs(c.Model)
		}
		versionOK = string(out.Stdout) == "codex-cli "+version+"\n"
	case claudeAdapter:
		version, versionOK = strings.CutSuffix(strings.TrimSpace(string(out.Stdout)), " (Claude Code)")
		versionOK = versionOK && claudeSupportedVersions[version]
		spec.args = claudeArgs(c.Model)
	}
	if !versionOK {
		return Receipt{}, &Error{Code: "unsupported-version", Reason: "version-mismatch"}
	}
	spec.sha256, spec.stdin, spec.outputCap = out.TargetSHA256, prompt, c.MaxOutputBytes
	var answer string
	var evidence Evidence
	if c.Transport == "app-server" {
		var facts appServerResult
		out, facts, err = checkedAppServer(ctx, spec, c.Model, c.DisabledMCPServers...)
		if err == nil {
			answer = facts.Answer
			evidence = Evidence{WaitStatus: out.WaitStatus, WaitErrorKind: out.WaitErrorKind,
				TrustedVendorRuntime: true, CodexTransport: "app-server", CodexAppServerUsagePresent: facts.UsagePresent,
				CodexInputTokens: facts.Usage.Input, CodexCachedInputTokens: facts.Usage.Cached,
				CodexCacheWriteInputTokens: facts.Usage.CacheWrite, CodexOutputTokens: facts.Usage.Output,
				CodexReasoningOutputTokens: facts.Usage.Reasoning}
		}
	} else {
		out, err = checkedRun(ctx, spec)
		if err == nil {
			if c.Adapter == codexAdapter {
				answer, version, evidence, err = admitCodex(out, version)
			} else {
				answer, version, evidence, err = admitClaude(out, prompt)
			}
		}
	}
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Consultant: c.Name, Adapter: c.Adapter, Version: version, Model: c.Model,
		ExitCode: out.ExitCode, Duration: time.Since(start), ContentForm: ContentForm,
		ContentSHA256: digest(answer), Answer: answer, Evidence: evidence}, nil
}

// admitClaude preserves the shipped catalog and receipt evidence after host success.
func admitClaude(out runOutcome, prompt string) (string, string, Evidence, error) {
	in, reasons := inspectStream(out.Stdout, prompt, out.Cwds)
	// inspectStream fails an unpinned init itself; this is the same gate held
	// independently of that ordering, so an unsupported version is reported as
	// such even when another literal was recorded first.
	if in.Version != "" && !claudeSupportedVersions[in.Version] {
		reasons = append([]string{"version-mismatch"}, dropReason(reasons, "version-mismatch")...)
	}
	if len(reasons) != 0 {
		return "", "", Evidence{}, &Error{Code: codeFor(reasons[0]), Reason: reasons[0]}
	}
	return in.Answer, in.Version, Evidence{
		WaitStatus: out.WaitStatus, WaitErrorKind: out.WaitErrorKind,
		InitAPIKeySource: in.InitAPIKeySource, AgentCount: in.AgentCount,
		RateLimitEvents: in.RateLimitEvents, OverageAttested: in.OverageAttested,
		APIRetryCount: in.APIRetryCount, UsageComplete: in.UsageComplete,
		WebSearchZero: in.WebSearchZero, OpusInputTokens: in.OpusInputTokens,
		OpusOutputTokens: in.OpusOutputTokens,
	}, nil
}

// admitCodex admits text only. Version is the digest-bound preflight value;
// the transcript does not attest the serving model or the billing source.
func admitCodex(out runOutcome, version string) (string, string, Evidence, error) {
	in := inspectCodex(out.Stdout)
	code, reason := "protocol", "codex-invalid-protocol"
	switch in.Verdict {
	case "admitted":
		return in.Answer, version, Evidence{WaitStatus: out.WaitStatus, WaitErrorKind: out.WaitErrorKind,
			TrustedVendorRuntime: true, CodexInputTokens: in.Usage.Input, CodexCachedInputTokens: in.Usage.Cached,
			CodexCacheWriteInputTokens: in.Usage.CacheWrite, CodexOutputTokens: in.Usage.Output, CodexReasoningOutputTokens: in.Usage.Reasoning}, nil
	case "visible_action":
		code, reason = "tool-activity", "codex-visible-action"
	case "vendor_error":
		reason = "codex-vendor-error"
	case "terminal_missing":
		reason = "codex-terminal-missing"
	case "no_answer":
		reason = "codex-no-answer"
	}
	return "", "", Evidence{}, &Error{Code: code, Reason: reason}
}

// checkedRun applies the same failure precedence to the version probe and
// prompt invocation. No output from a failed process is admitted by either.
func checkedRun(ctx context.Context, spec runSpec) (runOutcome, error) {
	out, err := run(ctx, spec)
	if err != nil {
		return runOutcome{}, classifyStartError(ctx, err)
	}
	if err := classifyRunOutcome(out); err != nil {
		return runOutcome{}, err
	}
	return out, nil
}

// checkedAppServer must classify launched host facts even when the exchange
// returned an error. A nonempty wait kind proves this was a launched child.
func checkedAppServer(ctx context.Context, spec runSpec, model string, disabledMCPServers ...string) (runOutcome, appServerResult, error) {
	out, facts, err := runCodexAppServer(ctx, spec, model, disabledMCPServers...)
	if out.WaitErrorKind == "" {
		return runOutcome{}, appServerResult{}, classifyStartError(ctx, err)
	}
	if hostErr := classifyRunOutcome(out); hostErr != nil {
		return runOutcome{}, appServerResult{}, hostErr
	}
	if err != nil {
		return runOutcome{}, appServerResult{}, classifyAppServerError(err)
	}
	return out, facts, nil
}

func classifyStartError(ctx context.Context, err error) *Error {
	// CommandContext refuses to start on a dead context. Preserve the caller's
	// withdrawal ahead of a prelaunch host error, just as static checkedRun did.
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return &Error{Code: "canceled", Reason: "caller"}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Code: "timeout", Reason: "deadline"}
	}
	return classifyRunError(err)
}

func classifyRunOutcome(out runOutcome) *Error {
	// Precedence is deliberate and fails closed. The runner already makes the
	// deadline disjoint from the other two aborts: a cap abort and a caller
	// cancellation both leave the run context Canceled rather than
	// DeadlineExceeded, and TimedOut additionally excludes CapExceeded. Only
	// two overlaps are real, and the order resolves them. Cap and cancel can
	// both be set when a caller cancels a run that had already blown the cap;
	// the cap wins, because it is the reason the output is unusable. Deadline
	// and cancel can both be set when the deadline fired and the caller
	// cancelled before Canceled was read from the parent context after Wait;
	// cancel wins, because the caller withdrew the request. Incomplete cleanup
	// and an abandoned pipe drain then outrank a zero exit status, because
	// both mean the retained transcript may be a prefix of what the consultant
	// wrote and a truncated transcript must never be admitted. A cancellation
	// observed after Wait discards a completed answer on purpose: the caller
	// withdrew the request, so no receipt is issued for it.
	switch {
	case out.CapExceeded:
		return &Error{Code: "output-limit", Reason: "cap"}
	case out.Canceled:
		return &Error{Code: "canceled", Reason: "caller"}
	case out.TimedOut:
		return &Error{Code: "timeout", Reason: "deadline"}
	case !out.GroupCleanupOK || !out.CleanupOK:
		return &Error{Code: "process-exit", Reason: "cleanup"}
	case out.WaitErrorKind == "wait-delay" || out.WaitErrorKind == "other":
		return &Error{Code: "drain-incomplete", Reason: out.WaitErrorKind}
	case out.ExitCode != 0:
		return &Error{Code: "process-exit", Reason: out.WaitStatus}
	}
	return nil
}

// classifyAppServerError exposes only local literals, never RPC diagnostics.
func classifyAppServerError(err error) *Error {
	code, reason := "protocol", "codex-app-server-invalid-protocol"
	switch {
	case errors.Is(err, errAppServerProtocolInitialize):
		reason = "codex-app-server-invalid-protocol-initialize"
	case errors.Is(err, errAppServerProtocolDiscovery):
		reason = "codex-app-server-invalid-protocol-discovery"
	case errors.Is(err, errAppServerProtocolThread):
		reason = "codex-app-server-invalid-protocol-thread"
	case errors.Is(err, errAppServerProtocolTurn):
		reason = "codex-app-server-invalid-protocol-turn"
	case errors.Is(err, errAppServerProtocolShutdown):
		reason = "codex-app-server-invalid-protocol-shutdown"
	case errors.Is(err, errAppServerConfigWarning):
		reason = "codex-app-server-config-warning-rejected"
	case errors.Is(err, errAppServerWarning):
		reason = "codex-app-server-warning-rejected"
	case errors.Is(err, errAppServerDeprecationNotice):
		reason = "codex-app-server-deprecation-notice-rejected"
	case errors.Is(err, errAppServerAccountUpdated):
		reason = "codex-app-server-account-updated-rejected"
	case errors.Is(err, errAppServerRequest):
		code, reason = "tool-activity", "codex-app-server-request"
	case errors.Is(err, errAppServerVendor):
		reason = "codex-app-server-vendor-error"
	case errors.Is(err, errAppServerUnavailable):
		reason = "codex-app-server-model-unavailable"
	case errors.Is(err, errAppServerIncomplete), errors.Is(err, errDuplexEOF):
		reason = "codex-app-server-incomplete"
	case errors.Is(err, errAppServerAnswer):
		reason = "codex-app-server-invalid-answer"
	case errors.Is(err, errDuplexRecords):
		reason = "codex-app-server-record-limit"
	case errors.Is(err, errDuplexWrite):
		reason = "codex-app-server-write"
	case errors.Is(err, errDuplexBackpressure):
		reason = "codex-app-server-backpressure"
	}
	// Append only host-authored points, and only to a phase diagnosis. Unknown
	// private values fall back to the existing phase-only reason.
	var fault *appServerFault
	if errors.As(err, &fault) && (errors.Is(err, errAppServerProtocolInitialize) ||
		errors.Is(err, errAppServerProtocolDiscovery) || errors.Is(err, errAppServerProtocolThread) ||
		errors.Is(err, errAppServerProtocolTurn) || errors.Is(err, errAppServerProtocolShutdown)) {
		switch fault.point {
		case "account-login-completed-rejected", "active-correlation", "agent-message-delta-rejected",
			"app-list-updated-rejected", "auto-approval-review-completed-rejected", "auto-approval-review-started-rejected",
			"auto-approval-review-strict-review-required-rejected", "command-exec-output-delta-rejected", "command-execution-output-delta-rejected",
			"command-execution-terminal-interaction-rejected", "error-notification-rejected", "external-agent-config-import-completed-rejected",
			"external-agent-config-import-progress-rejected", "file-change-output-delta-rejected", "file-change-patch-updated-rejected",
			"fs-changed-rejected", "fuzzy-file-search-session-completed-rejected", "fuzzy-file-search-session-updated-rejected",
			"guardian-warning-rejected", "hook-completed-rejected", "hook-started-rejected",
			"initialize-reply", "item-agent-citation",
			"item-agent-questions", "item-agent-shape", "item-bound",
			"item-collab-agent-tool-call-rejected", "item-command-execution-rejected", "item-context-compaction-rejected",
			"item-dynamic-tool-call-rejected", "item-entered-review-mode-rejected", "item-exited-review-mode-rejected",
			"item-file-change-rejected", "item-function-call-output-rejected", "item-hook-prompt-rejected",
			"item-image-generation-rejected", "item-image-view-rejected", "item-lifecycle",
			"item-mcp-tool-call-rejected", "item-plan-rejected", "item-prompt-echo",
			"item-reasoning-content", "item-reasoning-shape", "item-shape",
			"item-sleep-rejected", "item-sub-agent-activity-rejected", "item-timing",
			"item-unknown", "item-user-content", "item-user-elements",
			"item-user-shape", "item-web-search-rejected", "mcp-server-event-stream-notification-rejected",
			"mcp-server-oauth-login-completed-rejected", "mcp-startup-error", "mcp-startup-failed",
			"mcp-startup-failure-reason", "mcp-startup-name", "mcp-startup-shape",
			"mcp-startup-status", "mcp-startup-thread", "mcp-tool-call-progress-rejected",
			"method-shape", "model-cursor", "model-list-bound",
			"model-list-shape", "model-page-bound", "model-provider-auth-recovery-completed-rejected",
			"model-provider-auth-recovery-started-rejected", "model-rerouted-rejected", "model-safety-buffering",
			"model-shape", "model-verification", "notification-before-initialize",
			"notification-envelope", "notification-order", "notification-shape", "notification-unknown",
			"plan-delta-rejected", "process-exited-rejected", "process-output-delta-rejected",
			"project-changed-rejected", "rate-limits-shape", "raw-response-completed-rejected",
			"raw-response-item-completed-rejected", "reasoning-summary-part-added-rejected", "reasoning-summary-text-delta-rejected",
			"reasoning-text-delta-rejected", "record-array-bound", "record-before-start",
			"record-decode", "record-envelope", "record-framing",
			"remote-status", "reply-correlation", "reply-envelope",
			"reply-id", "reply-method", "request-envelope",
			"rpc-error-shape", "server-request-resolved-rejected", "skills-shape",
			"start-state", "system-error-rejected", "terminal-answer-missing",
			"terminal-items", "terminal-items-incomplete", "terminal-summary",
			"terminal-summary-mismatch", "thread-archived-rejected", "thread-compacted-rejected",
			"thread-correlation", "thread-deleted-rejected", "thread-environment-connected-rejected",
			"thread-environment-disconnected-rejected", "thread-goal-cleared-rejected", "thread-goal-updated-rejected",
			"thread-instructions", "thread-name", "thread-project-updated-rejected",
			"thread-queue-changed-rejected", "thread-realtime-closed-rejected", "thread-realtime-error-rejected",
			"thread-realtime-item-added-rejected", "thread-realtime-item-completed-rejected", "thread-realtime-item-started-rejected",
			"thread-realtime-item-transcript-delta-rejected", "thread-realtime-output-audio-delta-rejected", "thread-realtime-sdp-rejected",
			"thread-realtime-started-rejected", "thread-realtime-transcript-delta-rejected", "thread-realtime-transcript-done-rejected",
			"thread-reply-identity", "thread-reply-profile", "thread-reply-shape",
			"thread-reverted-rejected", "thread-settings-updated-rejected", "thread-started-identity",
			"thread-status", "thread-unarchived-rejected", "turn-diff-updated-rejected",
			"turn-error", "turn-id", "turn-items",
			"turn-moderation-metadata-rejected", "turn-order", "turn-plan-updated-rejected",
			"turn-reply-shape", "turn-shape", "turn-status",
			"turn-timing", "turn-view", "usage-counters",
			"usage-monotonic", "usage-shape", "usage-window",
			"windows-sandbox-setup-completed-rejected", "windows-world-writable-warning-rejected":
			reason += "-" + fault.point
		}
	}
	return &Error{Code: code, Reason: reason}
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
	case errors.Is(err, errEnvelope):
		return &Error{Code: "internal", Reason: "envelope"}
	case errors.Is(err, errIdentity):
		return &Error{Code: "internal", Reason: "identity"}
	case errors.Is(err, errTargetDrift):
		return &Error{Code: "target-drift", Reason: "target"}
	case errors.Is(err, errTargetInvalid):
		return &Error{Code: "target-invalid", Reason: "target"}
	// Defensive: Run pre-rejects identical bounds, so the runner cannot reach
	// this today. Kept so any future caller of run cannot mislabel it.
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
