package interceptor

import (
	"errors"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// Blocked reports whether err carries a BlockedError finding from the named
// interceptor at or above minimum. It examines every branch of the error tree
// and every finding: an earlier unrelated block or higher-verdict cause must
// not hide a match elsewhere in the tree.
func Blocked(err error, name string, minimum agent.Verdict) bool {
	return anyBlocked(err, func(blocked *agent.BlockedError) bool {
		for _, finding := range blocked.Findings {
			if finding.Interceptor == name && finding.Verdict >= minimum {
				return true
			}
		}
		return false
	})
}

// SensitiveBlocked reports whether err holds a block, at any depth, that
// stands for a credential or sensitive value in the run: one the pipeline
// marked Sensitive because an agent.SensitiveTextInterceptor blocked, under
// any name, or one from Secrets or ConfiguredSecrets by name. Secrets does
// not implement that capability, and a BlockedError built outside the
// pipeline carries no mark. An outbound content denial
// (provider.ErrContentDenied, #424) counts too: the refused request carried a
// configured credential. A host uses it to withhold partial output and
// diagnostics after such a block.
func SensitiveBlocked(err error) bool {
	return anyBlocked(err, (*agent.BlockedError).Sensitive) ||
		Blocked(err, Secrets{}.Name(), agent.VerdictBlock) ||
		Blocked(err, ConfiguredSecretsName, agent.VerdictBlock) ||
		errors.Is(err, provider.ErrContentDenied)
}

// anyBlocked reports whether match accepts a non-nil BlockedError anywhere in
// err's tree, examining every branch.
func anyBlocked(err error, match func(*agent.BlockedError) bool) bool {
	if blocked, ok := err.(*agent.BlockedError); ok && blocked != nil && match(blocked) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if anyBlocked(child, match) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return anyBlocked(wrapped.Unwrap(), match)
	}
	return false
}
