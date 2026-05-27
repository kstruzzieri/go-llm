// route_errors.go defines the bounded ErrorClass vocabulary used by the
// RoutingFeedback seam to attribute per-attempt failures. classifyError is
// a pure function over the error types already used by IsInfrastructureError
// (net.OpError, net.DNSError, HTTPStatusError) plus context cancellation,
// returning both the class and the corresponding AttemptStatus.
package provider

import (
	"context"
	"errors"
	"net"
)

// ErrorClass is the bounded vocabulary of routing failure classes.
// RoutingFeedback aggregates pivot by ErrorClass for fail-mode debugging.
type ErrorClass string

const (
	// ErrorClassNetwork covers connection-level failures: refused, reset,
	// DNS lookup failures, unreachable.
	ErrorClassNetwork ErrorClass = "network"

	// ErrorClassTimeout covers context.DeadlineExceeded — the caller's
	// reasonable deadline was missed. Distinct from cancellation
	// (caller-initiated abort) which gets AttemptStatusUnknown and no class.
	ErrorClassTimeout ErrorClass = "timeout"

	// ErrorClass4xx covers HTTP 400-499 except 429, which gets its own
	// class. Typically caller-side issues (bad request, unauthorized,
	// not found).
	ErrorClass4xx ErrorClass = "4xx"

	// ErrorClass5xx covers HTTP 500-599 — server-side failures.
	ErrorClass5xx ErrorClass = "5xx"

	// ErrorClassRateLimit covers HTTP 429. Semantically distinct from 5xx:
	// rate limiting indicates load-shedding, not a server fault.
	ErrorClassRateLimit ErrorClass = "rate_limit"

	// ErrorClassModelUnloaded is reserved for Ollama-specific detection of
	// the "model is not loaded" error. classifyError does not produce this
	// class in PR2 — the producer is added in a later PR. Consumers may
	// still match on the constant without a vocabulary change later.
	ErrorClassModelUnloaded ErrorClass = "model_unloaded"

	// ErrorClassUnknown is the fall-through for errors classifyError can't
	// otherwise identify. Always paired with AttemptStatusFailed.
	ErrorClassUnknown ErrorClass = "unknown"
)

// classifyError returns the ErrorClass and AttemptStatus for err.
//
// Mapping (first match wins):
//
//	nil err                                       → ("",            Succeeded)
//	errors.Is(err, context.Canceled)              → ("",            Unknown)
//	errors.Is(err, context.DeadlineExceeded)      → ("timeout",     Failed)
//	*net.OpError / *net.DNSError                  → ("network",     Failed)
//	*HTTPStatusError 429                          → ("rate_limit",  Failed)
//	*HTTPStatusError 500..599                     → ("5xx",         Failed)
//	*HTTPStatusError 400..499 (not 429)           → ("4xx",         Failed)
//	any other err                                 → ("unknown",     Failed)
//
// model_unloaded is reserved but never returned in PR2.
func classifyError(err error) (ErrorClass, AttemptStatus) {
	if err == nil {
		return "", AttemptStatusSucceeded
	}
	if errors.Is(err, context.Canceled) {
		return "", AttemptStatusUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorClassTimeout, AttemptStatusFailed
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ErrorClassNetwork, AttemptStatusFailed
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ErrorClassNetwork, AttemptStatusFailed
	}

	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 429:
			return ErrorClassRateLimit, AttemptStatusFailed
		case httpErr.StatusCode >= 500 && httpErr.StatusCode <= 599:
			return ErrorClass5xx, AttemptStatusFailed
		case httpErr.StatusCode >= 400 && httpErr.StatusCode <= 499:
			return ErrorClass4xx, AttemptStatusFailed
		}
	}

	return ErrorClassUnknown, AttemptStatusFailed
}
