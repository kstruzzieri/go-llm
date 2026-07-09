package agentflow

import (
	"fmt"
	"strings"
)

// StructuredError is one entry of an agentflow --json failure `errors` array.
type StructuredError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CommandError is a nonzero-exit or invalid-status agentflow result.
type CommandError struct {
	Cmd    string
	Exit   int
	Errors []StructuredError
	Stderr string
}

func (e *CommandError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "agentflow %s: exit %d", e.Cmd, e.Exit)
	for _, se := range e.Errors {
		fmt.Fprintf(&b, "; [%s] %s", se.Code, se.Message)
	}
	if len(e.Errors) == 0 && strings.TrimSpace(e.Stderr) != "" {
		fmt.Fprintf(&b, ": %s", strings.TrimSpace(e.Stderr))
	}
	return b.String()
}

// statusEnvelope is the common --json failure envelope: {"status":"invalid","errors":[...]}.
type statusEnvelope struct {
	Status string            `json:"status"`
	Errors []StructuredError `json:"errors"`
}
