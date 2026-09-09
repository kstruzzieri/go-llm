package main

import "github.com/kstruzzieri/go-llm/agent"

const secretsBlockedMessage = "run blocked: sensitive content detected"

// runFailureMessage changes only presentation, never the typed run error. Its
// signature also fits the runtime's pre-emission FailureMessage hook.
func runFailureMessage(_ string, err error) string {
	if canaryAborted(err) {
		return "run aborted: canary detected outside system instructions"
	}
	if secretsBlocked(err) {
		return secretsBlockedMessage
	}
	return err.Error()
}

// secretsBlocked examines every branch and finding: an earlier unrelated block
// or higher-verdict cause must not hide a secrets block elsewhere in the tree.
func secretsBlocked(err error) bool {
	return interceptorBlocked(err, "secrets", agent.VerdictBlock)
}

func interceptorBlocked(err error, name string, minimum agent.Verdict) bool {
	if blocked, ok := err.(*agent.BlockedError); ok && blocked != nil {
		for _, finding := range blocked.Findings {
			if finding.Interceptor == name && finding.Verdict >= minimum {
				return true
			}
		}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if interceptorBlocked(child, name, minimum) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return interceptorBlocked(wrapped.Unwrap(), name, minimum)
	}
	return false
}
