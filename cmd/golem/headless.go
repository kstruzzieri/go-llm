package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// outputFormat is the -output-format selection (#352). text is the default and
// is byte-identical to golem's pre-#352 one-shot output; the two machine
// formats put machine-readable objects on stdout and leave every diagnostic
// on stderr.
type outputFormat uint8

const (
	// outputText is the zero value on purpose: an unset -output-format and an
	// explicit "text" are the same mode, so no call site needs a set/unset bit.
	outputText outputFormat = iota
	outputJSON
	outputStreamJSON
)

// parseOutputFormat maps the flag value to a mode. Matching is exact and
// case-sensitive: this is a machine contract, and accepting "JSON" or
// "stream_json" today would make them contract forever.
func parseOutputFormat(v string) (outputFormat, error) {
	switch v {
	case "", "text":
		return outputText, nil
	case "json":
		return outputJSON, nil
	case "stream-json":
		return outputStreamJSON, nil
	}
	return 0, fmt.Errorf("golem: invalid -output-format %q (want text, json, or stream-json)", v)
}

// machine reports whether this format writes machine-readable objects to
// stdout instead of the plain answer.
func (f outputFormat) machine() bool { return f == outputJSON || f == outputStreamJSON }

// allowToolNames is the frozen set of built-in gated tools that -allow-tool can
// authorize (#352). This ticket freezes it as public contract for scripting
// consumers, so adding a name here is a contract change.
//
// Deliberately absent:
//   - verify_command   — a synthetic approval identity, never a registered
//     tool; the model cannot call it (cmd/golem/verify.go).
//   - submit_plan      — planning mode only, and -goal is incompatible with -p.
//   - mcp__*           — excluded by the issue; MCP tools stay mounted under
//     -p and stay denied.
//   - command_status,
//     command_tail     — mounted alongside start_command but never gated, so
//     there is no authorization to grant (see allowToolCompanions).
var allowToolNames = []string{"write_file", "edit_file", "run_command", "start_command", "stop_command"}

// allowToolCompanions names the UNGATED tools a named tool needs to be usable.
// start_command's output is readable only through command_status/command_tail;
// mounting them is a dependency closure, not an authorization expansion,
// because neither ever prompts.
var allowToolCompanions = map[string][]string{
	"start_command": {"command_status", "command_tail"},
}

// allowToolSet is the exact-name authorization set built from -allow-tool. It
// is consulted by name only: the approver never parses a preview and never
// parses or derives from an ApprovalKey, mirroring the #341 discipline that
// scope comes from the tool name and never from the key.
type allowToolSet struct{ names map[string]struct{} }

// newAllowToolSet validates every name against the frozen list and returns the
// authorization set. An unknown or excluded name is a usage error: it must fail
// before the run rather than silently authorizing nothing.
func newAllowToolSet(values []string) (allowToolSet, error) {
	allowed := make(map[string]struct{}, len(allowToolNames))
	for _, n := range allowToolNames {
		allowed[n] = struct{}{}
	}
	set := allowToolSet{names: make(map[string]struct{}, len(values))}
	for _, v := range values {
		if _, ok := allowed[v]; !ok {
			return allowToolSet{}, newUsageError(
				"golem: unknown -allow-tool %q (want one of: %s)", v, strings.Join(allowToolNames, ", "))
		}
		set.names[v] = struct{}{}
	}
	return set, nil
}

// authorized reports whether this exact tool name was named by -allow-tool.
func (s allowToolSet) authorized(name string) bool {
	if s.names == nil {
		return false
	}
	_, ok := s.names[name]
	return ok
}

// empty reports whether -allow-tool authorized nothing.
func (s allowToolSet) empty() bool { return len(s.names) == 0 }

// stdinPromptSentinel is the -p value that means "read the prompt from stdin".
// A literal prompt of "-" is unreachable by design and documented as such.
const stdinPromptSentinel = "-"

// stdinPromptRequested reports whether -p asked for the stdin prompt path.
func stdinPromptRequested(f flags) bool {
	return f.promptSet && f.prompt == stdinPromptSentinel
}

// resolveStdinPrompt reads the one-shot prompt from stdin for "-p -" (#352).
//
// It refuses a terminal outright rather than hanging on a read the caller
// almost certainly did not intend, and bounds the read at maxGoalBytes — the
// same ceiling main.go already installs as the runtime's MaxMessageBytes, so
// the CLI can never admit a prompt the runtime would then reject as an invalid
// request. Every refusal is a usage error, so it exits 2.
//
// The bytes are returned verbatim: trailing newlines are the caller's content,
// and only the emptiness check trims.
func resolveStdinPrompt(stdin io.Reader, isTTY bool) (string, error) {
	if isTTY {
		return "", newUsageError(`golem: -p - reads the prompt from stdin; pipe or redirect input (e.g. echo "..." | golem -p -)`)
	}
	// cap+1 so a prompt exactly at the cap is admitted and the first byte over
	// it is observable.
	buf, err := io.ReadAll(io.LimitReader(stdin, maxGoalBytes+1))
	if err != nil {
		return "", wrapUsageError(fmt.Errorf("golem: read prompt from stdin: %w", err))
	}
	if len(buf) > maxGoalBytes {
		return "", newUsageError("golem: -p - prompt exceeds %d bytes", maxGoalBytes)
	}
	if strings.TrimSpace(string(buf)) == "" {
		return "", newUsageError("golem: -p requires a non-empty prompt")
	}
	return string(buf), nil
}

// filterAllowedTools narrows a built gated-tool set to what -allow-tool named,
// preserving the builder's order (#352).
//
// It admits a named tool plus that tool's UNGATED companions: start_command's
// result is only readable through command_status/command_tail, so mounting
// start_command without them mounts a tool whose output can never be observed.
// Neither companion ever prompts, so this widens usability, not authorization —
// the approver still authorizes by exact name, and stop_command (gated) is
// never a companion.
//
// A named tool that was never built is simply absent: this filters, it never
// fabricates.
func filterAllowedTools(built []agent.Tool, allow allowToolSet) []agent.Tool {
	if allow.empty() {
		return nil
	}
	mount := make(map[string]struct{}, len(built))
	for name := range allow.names {
		mount[name] = struct{}{}
		for _, companion := range allowToolCompanions[name] {
			mount[companion] = struct{}{}
		}
	}
	var out []agent.Tool
	for _, tool := range built {
		if _, ok := mount[tool.Spec().Name]; ok {
			out = append(out, tool)
		}
	}
	return out
}

// headlessApprover is the non-interactive approver -allow-tool installs for
// one-shot runs (#352). It replaces the nil-approver fail-safe (which denies
// everything) with an exact-name allowlist, and nothing else changes: there is
// no prompt, no preview rendering, no grant store, and no session state.
//
// Authorization is by tool name ONLY. The preview is never parsed — it is
// model-influenced text — and the ApprovalKey is never parsed or compared,
// keeping the #341 rule that a colliding key can never transfer authorization
// across tools. The key is accepted and ignored purely to satisfy the
// KeyedApprover signature.
//
// It never records a grant: an approval authorizes exactly the call in front of
// it, and the process exits at the end of the turn.
type headlessApprover struct{ allow allowToolSet }

var (
	_ agent.Approver      = (*headlessApprover)(nil)
	_ agent.KeyedApprover = (*headlessApprover)(nil)
)

func newHeadlessApprover(allow allowToolSet) *headlessApprover {
	return &headlessApprover{allow: allow}
}

// headlessApproverFor returns the -allow-tool approver, or nil when nothing was
// authorized. A nil approver is the pre-#352 behavior: the runtime's fail-safe
// denies every gated call.
func headlessApproverFor(allow allowToolSet) *headlessApprover {
	if allow.empty() {
		return nil
	}
	return newHeadlessApprover(allow)
}

// Approve satisfies the plain contract by delegating; no key, same answer.
func (a *headlessApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	d, err := a.ApproveKeyed(ctx, call, preview, "")
	return d.Approved, err
}

// ApproveKeyed approves iff the exact tool name was named by -allow-tool. A
// canceled context denies and returns the error so the run aborts, matching the
// interactive approver's cancellation semantics.
func (a *headlessApprover) ApproveKeyed(ctx context.Context, call provider.ToolCall, _, _ string) (agent.ApprovalDecision, error) {
	if err := ctx.Err(); err != nil {
		return agent.ApprovalDecision{}, err
	}
	// ViaGrant stays false by construction: this decision came from a flag, not
	// from a session grant, and reporting otherwise would mislabel the run
	// record and the tool-result event.
	return agent.ApprovalDecision{Approved: a.allow.authorized(call.Function.Name)}, nil
}
