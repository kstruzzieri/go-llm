package main

import "fmt"

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
