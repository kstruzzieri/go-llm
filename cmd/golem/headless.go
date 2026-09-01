package main

import (
	"fmt"
	"io"
	"strings"
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
