package interceptor

import "github.com/kstruzzieri/go-llm/agent"

// Blocked reports whether err carries a BlockedError finding from the named
// interceptor at or above minimum. It examines every branch of the error tree
// and every finding: an earlier unrelated block or higher-verdict cause must
// not hide a match elsewhere in the tree.
func Blocked(err error, name string, minimum agent.Verdict) bool {
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
			if Blocked(child, name, minimum) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return Blocked(wrapped.Unwrap(), name, minimum)
	}
	return false
}

// SensitiveBlocked reports whether err holds a block, at any depth, from a
// guard whose finding stands for a credential or sensitive value in the run:
// Secrets or ConfiguredSecrets. A host uses it to withhold partial output and
// diagnostics after such a block.
func SensitiveBlocked(err error) bool {
	return Blocked(err, Secrets{}.Name(), agent.VerdictBlock) ||
		Blocked(err, ConfiguredSecretsName, agent.VerdictBlock)
}
