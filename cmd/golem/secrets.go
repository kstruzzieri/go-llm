package main

import "github.com/kstruzzieri/go-llm/agent"

const secretsBlockedMessage = "run blocked: sensitive content detected"

// runFailureMessage changes only presentation, never the typed run error. Its
// signature also fits the runtime's pre-emission FailureMessage hook.
func runFailureMessage(_ string, err error) string {
	if secretsBlocked(err) {
		return secretsBlockedMessage
	}
	return err.Error()
}

// secretsBlocked examines every branch and finding: an earlier unrelated block
// or higher-verdict cause must not hide a secrets block elsewhere in the tree.
func secretsBlocked(err error) bool {
	if blocked, ok := err.(*agent.BlockedError); ok && blocked != nil {
		for _, finding := range blocked.Findings {
			if finding.Interceptor == "secrets" && finding.Verdict >= agent.VerdictBlock {
				return true
			}
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if secretsBlocked(child) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return secretsBlocked(wrapped.Unwrap())
	}
	return false
}
