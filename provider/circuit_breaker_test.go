package provider

import (
	"errors"
	"testing"
	"time"
)

func TestCircuitBreakerInitialState(t *testing.T) {
	cb := NewCircuitBreaker()

	if cb.State() != BreakerClosed {
		t.Fatalf("expected initial state BreakerClosed, got %v", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true for a new breaker")
	}
}

func TestCircuitBreakerTripsAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCooldown(time.Hour),
	)

	err := errors.New("backend down")
	for i := 0; i < 3; i++ {
		cb.RecordFailure(err)
	}

	if cb.State() != BreakerOpen {
		t.Fatalf("expected BreakerOpen after %d failures, got %v", 3, cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() to return false when breaker is open")
	}
}

func TestCircuitBreakerDoesNotTripBelowThreshold(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCooldown(time.Hour),
	)

	err := errors.New("transient error")
	cb.RecordFailure(err)
	cb.RecordFailure(err)

	if cb.State() != BreakerClosed {
		t.Fatalf("expected BreakerClosed with 2 of 3 failures, got %v", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true when below threshold")
	}
}

func TestCircuitBreakerRecoversAfterCooldown(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCooldown(10*time.Millisecond),
	)

	cb.RecordFailure(errors.New("fail"))
	if cb.State() != BreakerOpen {
		t.Fatalf("expected BreakerOpen, got %v", cb.State())
	}

	time.Sleep(20 * time.Millisecond)

	if !cb.Allow() {
		t.Fatal("expected Allow() to return true after cooldown elapsed")
	}
	if cb.State() != BreakerHalfOpen {
		t.Fatalf("expected BreakerHalfOpen after cooldown, got %v", cb.State())
	}
}

func TestCircuitBreakerHalfOpenSuccess(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCooldown(10*time.Millisecond),
	)

	// Trip the breaker.
	cb.RecordFailure(errors.New("fail"))
	if cb.State() != BreakerOpen {
		t.Fatalf("expected BreakerOpen, got %v", cb.State())
	}

	// Wait for cooldown to elapse.
	time.Sleep(20 * time.Millisecond)

	// Allow should transition to HalfOpen.
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true after cooldown")
	}
	if cb.State() != BreakerHalfOpen {
		t.Fatalf("expected BreakerHalfOpen, got %v", cb.State())
	}

	// A success should close the breaker.
	cb.RecordSuccess()
	if cb.State() != BreakerClosed {
		t.Fatalf("expected BreakerClosed after success in half-open, got %v", cb.State())
	}
}

func TestCircuitBreakerHalfOpenFailure(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(1),
		WithCooldown(10*time.Millisecond),
	)

	// Trip the breaker.
	cb.RecordFailure(errors.New("fail"))
	if cb.State() != BreakerOpen {
		t.Fatalf("expected BreakerOpen, got %v", cb.State())
	}

	// Wait for cooldown to elapse.
	time.Sleep(20 * time.Millisecond)

	// Allow should transition to HalfOpen.
	if !cb.Allow() {
		t.Fatal("expected Allow() to return true after cooldown")
	}
	if cb.State() != BreakerHalfOpen {
		t.Fatalf("expected BreakerHalfOpen, got %v", cb.State())
	}

	// A failure should reopen the breaker.
	cb.RecordFailure(errors.New("still failing"))
	if cb.State() != BreakerOpen {
		t.Fatalf("expected BreakerOpen after failure in half-open, got %v", cb.State())
	}
}

func TestCircuitBreakerFailureDecay(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCooldown(10*time.Millisecond),
	)

	err := errors.New("transient")
	cb.RecordFailure(err)
	cb.RecordFailure(err)

	// Wait for cooldown to elapse so that failure count decays.
	time.Sleep(20 * time.Millisecond)

	// This failure should start from 0 after decay, so only 1 failure total.
	cb.RecordFailure(err)

	if cb.State() != BreakerClosed {
		t.Fatalf("expected BreakerClosed after failure decay, got %v", cb.State())
	}

	info := cb.Info()
	if info.Failures != 1 {
		t.Fatalf("expected 1 failure after decay, got %d", info.Failures)
	}
}

func TestCircuitBreakerSuccessResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(3),
		WithCooldown(time.Hour),
	)

	err := errors.New("transient")
	cb.RecordFailure(err)
	cb.RecordFailure(err)

	cb.RecordSuccess()

	info := cb.Info()
	if info.Failures != 0 {
		t.Fatalf("expected 0 failures after RecordSuccess(), got %d", info.Failures)
	}
}

func TestCircuitBreakerInfo(t *testing.T) {
	cb := NewCircuitBreaker(
		WithFailureThreshold(2),
		WithCooldown(time.Hour),
	)

	// Initially, info should reflect a clean state.
	info := cb.Info()
	if info.State != BreakerClosed {
		t.Fatalf("expected State=BreakerClosed, got %v", info.State)
	}
	if info.Failures != 0 {
		t.Fatalf("expected Failures=0, got %d", info.Failures)
	}
	if info.LastError != nil {
		t.Fatalf("expected LastError=nil, got %v", info.LastError)
	}
	if !info.RecoverAt.IsZero() {
		t.Fatalf("expected RecoverAt to be zero for closed breaker, got %v", info.RecoverAt)
	}

	// Trip the breaker.
	testErr := errors.New("server error")
	cb.RecordFailure(testErr)
	cb.RecordFailure(testErr)

	info = cb.Info()
	if info.State != BreakerOpen {
		t.Fatalf("expected State=BreakerOpen, got %v", info.State)
	}
	if info.Failures != 2 {
		t.Fatalf("expected Failures=2, got %d", info.Failures)
	}
	if info.LastError == nil || info.LastError.Error() != testErr.Error() {
		t.Fatalf("expected LastError=%q, got %v", testErr, info.LastError)
	}
	if info.RecoverAt.IsZero() {
		t.Fatal("expected RecoverAt to be set when breaker is open")
	}
	// RecoverAt should be approximately lastFailure + cooldown.
	if info.RecoverAt.Before(info.LastFailure) {
		t.Fatal("expected RecoverAt to be after LastFailure")
	}
}
