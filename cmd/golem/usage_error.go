package main

import (
	"errors"
	"fmt"
	"strings"
)

// usageError marks a failure that happened BEFORE the agent turn began: flag
// parsing, flag validation, input resolution, configuration, or authorization
// setup. main() maps it to exit 2 (#352). Everything else that reaches main()
// is a failure of a turn that had already started, and exits 1.
//
// The boundary is deliberately structural rather than message-based: a call
// site either wraps its error at a pre-run boundary or it does not. No error
// prose is ever parsed to classify an exit.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// newUsageError builds a pre-run failure from a format string.
func newUsageError(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// wrapUsageError marks an existing error as a pre-run failure, preserving it
// for errors.Is/errors.As.
func wrapUsageError(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// exitCodeFor is the single exit-code decision (#352): 0 success, 2 pre-run
// (usage) failure, 1 everything else.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}

// maybeUsageError applies the #352 exit-2 classification only on the headless
// (-p) surface. Every other mode keeps its pre-#352 exit behavior, including
// -agentflow-status's shipped exit 2/3 semantics, which a binary-wide usage
// taxonomy would collide with.
func maybeUsageError(err error, headless bool) error {
	if !headless {
		return err
	}
	return wrapUsageError(err)
}

// argsRequestOneShot reports one-shot INTENT from raw argv, for the one moment
// promptSet is unknowable: a flag-parse failure. A literal -p token (in any of
// the flag package's accepted spellings) is the signal; a "-p" appearing as
// another flag's value only matters when the parse FAILS, and misclassifying
// that pathological spelling costs nothing — the invocation is broken either
// way.
func argsRequestOneShot(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--p" || strings.HasPrefix(a, "-p=") || strings.HasPrefix(a, "--p=") {
			return true
		}
	}
	return false
}
