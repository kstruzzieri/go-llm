// circuit_breaker.go implements a three-state circuit breaker
// (Closed / Open / HalfOpen) for tracking provider health.
//
// Failure count decays: if the time between consecutive failures exceeds the
// cooldown duration, the counter resets to zero before counting the new failure.
// This prevents transient errors from accumulating across long healthy periods.
package provider

import (
	"sync"
	"time"
)

const (
	defaultFailureThreshold = 3
	defaultCooldown         = 30 * time.Second
)

// CircuitBreaker tracks the health of a single provider endpoint.
// It is safe for concurrent use.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            BreakerState
	failures         int
	lastFailure      time.Time
	lastError        error
	failureThreshold int
	cooldown         time.Duration
}

// BreakerOption configures a CircuitBreaker at construction time.
type BreakerOption func(*CircuitBreaker)

// WithFailureThreshold sets the number of consecutive failures required to
// trip the breaker from Closed to Open. Values <= 0 are ignored.
func WithFailureThreshold(n int) BreakerOption {
	return func(cb *CircuitBreaker) {
		if n > 0 {
			cb.failureThreshold = n
		}
	}
}

// WithCooldown sets the duration the breaker remains Open before
// transitioning to HalfOpen. Values <= 0 are ignored.
func WithCooldown(d time.Duration) BreakerOption {
	return func(cb *CircuitBreaker) {
		if d > 0 {
			cb.cooldown = d
		}
	}
}

// NewCircuitBreaker creates a breaker in the Closed state with the given
// options (or defaults).
func NewCircuitBreaker(opts ...BreakerOption) *CircuitBreaker {
	cb := &CircuitBreaker{
		state:            BreakerClosed,
		failureThreshold: defaultFailureThreshold,
		cooldown:         defaultCooldown,
	}
	for _, opt := range opts {
		opt(cb)
	}
	return cb
}

// Allow reports whether a request may proceed.
//
//   - Closed: always true.
//   - Open: false unless the cooldown has elapsed, in which case the breaker
//     transitions to HalfOpen and returns true (one probe request).
//   - HalfOpen: false (only one probe is allowed; the first Allow() call
//     after the cooldown transition already granted it).
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case BreakerClosed:
		return true
	case BreakerOpen:
		if time.Since(cb.lastFailure) >= cb.cooldown {
			cb.state = BreakerHalfOpen
			return true
		}
		return false
	case BreakerHalfOpen:
		// Only one probe is allowed; subsequent calls return false until
		// the probe result is recorded.
		return false
	default:
		return false
	}
}

// RecordSuccess records a successful request. It resets the failure counter
// and clears the last error. If the breaker is HalfOpen, it transitions to
// Closed.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.lastError = nil

	if cb.state == BreakerHalfOpen {
		cb.state = BreakerClosed
	}
}

// RecordFailure records a failed request. If the time since the last failure
// exceeds the cooldown, the failure counter decays to zero before counting
// the new failure. If the breaker is HalfOpen, it immediately transitions
// back to Open. If failures reach the threshold, the breaker trips to Open.
func (cb *CircuitBreaker) RecordFailure(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()

	// Failure count decay: if enough time has passed since the last failure,
	// reset the counter so that old transient failures don't accumulate.
	if !cb.lastFailure.IsZero() && now.Sub(cb.lastFailure) > cb.cooldown {
		cb.failures = 0
	}

	cb.failures++
	cb.lastFailure = now
	cb.lastError = err

	switch cb.state {
	case BreakerHalfOpen:
		// Probe failed — reopen immediately.
		cb.state = BreakerOpen
	case BreakerClosed:
		if cb.failures >= cb.failureThreshold {
			cb.state = BreakerOpen
		}
	}
}

// State returns the current breaker state.
func (cb *CircuitBreaker) State() BreakerState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Info returns a snapshot of the breaker's current status. When the breaker
// is Open, RecoverAt is set to lastFailure + cooldown.
func (cb *CircuitBreaker) Info() BreakerInfo {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	info := BreakerInfo{
		State:       cb.state,
		Failures:    cb.failures,
		LastFailure: cb.lastFailure,
		LastError:   cb.lastError,
	}

	if cb.state == BreakerOpen {
		info.RecoverAt = cb.lastFailure.Add(cb.cooldown)
	}

	return info
}
