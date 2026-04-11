// router_errors.go defines sentinel errors, error classification, and
// circuit-breaker state types used by the routing layer. These allow callers
// to distinguish infrastructure failures (network errors, 5xx, rate limiting)
// from client-side or application errors.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// ErrNoViableCandidate is returned when the router cannot find any
// provider/model that satisfies the request's capability requirements.
var ErrNoViableCandidate = errors.New("router: no viable candidate for request")

// ErrAllBreakersOpen is returned when every candidate provider has its
// circuit breaker in the open state.
var ErrAllBreakersOpen = errors.New("router: all circuit breakers are open")

// ErrBudgetAdaptationRequired is returned when the request's token budget
// exceeds what any available model can handle and needs to be adapted.
var ErrBudgetAdaptationRequired = errors.New("router: budget adaptation required")

// ErrBudgetExceeded is returned when a request would exceed the configured
// token budget and cannot be routed.
var ErrBudgetExceeded = errors.New("router: budget exceeded")

// ErrRouterClosed is returned when Route is called on a shut-down router.
var ErrRouterClosed = errors.New("router: router is closed")

// ---------------------------------------------------------------------------
// BreakerState
// ---------------------------------------------------------------------------

// BreakerState represents the current state of a circuit breaker.
type BreakerState int

const (
	// BreakerClosed means the circuit is healthy and requests flow normally.
	BreakerClosed BreakerState = iota
	// BreakerOpen means the circuit has tripped and requests are rejected.
	BreakerOpen
	// BreakerHalfOpen means the circuit is testing whether the backend has recovered.
	BreakerHalfOpen
)

// String returns the human-readable name of the breaker state.
func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// BreakerInfo
// ---------------------------------------------------------------------------

// BreakerInfo holds the current status of a circuit breaker for a particular
// provider or model endpoint.
type BreakerInfo struct {
	// State is the current circuit breaker state.
	State BreakerState
	// Failures is the count of consecutive failures.
	Failures int
	// LastFailure is the timestamp of the most recent failure.
	LastFailure time.Time
	// LastError is the most recent error that triggered a failure count.
	LastError error
	// RecoverAt is the earliest time the breaker will transition to half-open.
	RecoverAt time.Time
}

// ---------------------------------------------------------------------------
// HTTPStatusError
// ---------------------------------------------------------------------------

// HTTPStatusError represents an HTTP-level error from a provider backend.
// It carries the status code and status text so callers can classify the
// failure (e.g. distinguishing 429 rate limiting from 500 server errors).
type HTTPStatusError struct {
	// StatusCode is the HTTP status code (e.g. 500, 429).
	StatusCode int
	// Status is the HTTP status text (e.g. "500 Internal Server Error").
	Status string
}

// Error implements the error interface.
func (e *HTTPStatusError) Error() string {
	if e.Status != "" {
		return e.Status
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// ---------------------------------------------------------------------------
// Infrastructure error classification
// ---------------------------------------------------------------------------

// IsInfrastructureError reports whether err represents a transient
// infrastructure failure that warrants retry or circuit-breaker action.
//
// Infrastructure errors include:
//   - Network errors (net.OpError, net.DNSError)
//   - HTTP 5xx server errors
//   - HTTP 429 rate limiting
//
// The following are NOT infrastructure errors:
//   - nil
//   - context.Canceled and context.DeadlineExceeded (caller-initiated)
//   - HTTP 4xx client errors (except 429)
//   - Generic/unknown errors
func IsInfrastructureError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation and deadline are caller-initiated, not infrastructure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Network-level errors are always infrastructure failures.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	// HTTP status errors: 5xx and 429 are infrastructure, other 4xx are not.
	var httpErr *HTTPStatusError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode >= 500 {
			return true
		}
		if httpErr.StatusCode == 429 {
			return true
		}
		return false
	}

	return false
}
