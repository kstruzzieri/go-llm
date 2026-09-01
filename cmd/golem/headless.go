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
