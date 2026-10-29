package main

import "github.com/kstruzzieri/go-llm/agent/interceptor"

const secretsBlockedMessage = "run blocked: sensitive content detected"

// runFailureMessage changes only presentation, never the typed run error. Its
// signature also fits the runtime's pre-emission FailureMessage hook.
func runFailureMessage(_ string, err error) string {
	if canaryAborted(err) {
		return "run aborted: canary detected outside system instructions"
	}
	if interceptor.SensitiveBlocked(err) {
		return secretsBlockedMessage
	}
	return err.Error()
}
