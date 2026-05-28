package provider

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// capturingLogger implements feedbackLogger and records the *rendered*
// message (format applied to args) so tests can assert that args actually
// flow through the format and aren't silently dropped by a future refactor.
type capturingLogger struct {
	mu       sync.Mutex
	messages []string
}

func (c *capturingLogger) Warnf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, fmt.Sprintf(format, args...))
}

func (c *capturingLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.messages))
	copy(out, c.messages)
	return out
}

func TestFeedbackReadOnceFiresOnce(t *testing.T) {
	cap := &capturingLogger{}
	state := newFeedbackWarningState()

	// First call uses a sentinel error so we can prove later calls' args
	// are NOT recorded — locks the once-contract against a future refactor
	// that accidentally records every attempt.
	firstErr := errors.New("first-call-sentinel")
	state.warnFeedbackReadOnce(cap, FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}, firstErr)

	const iterations = 1000
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.warnFeedbackReadOnce(cap, FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}, errors.New("later-call-should-be-dropped"))
		}()
	}
	wg.Wait()

	msgs := cap.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("warnFeedbackReadOnce fired %d times, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0], "feedback") {
		t.Errorf("warning message %q does not mention feedback", msgs[0])
	}
	if !strings.Contains(msgs[0], "first-call-sentinel") {
		t.Errorf("warning message %q does not include first call's error; format/args may not flow", msgs[0])
	}
	if strings.Contains(msgs[0], "later-call-should-be-dropped") {
		t.Errorf("warning message %q includes a later call's error; once-contract violated", msgs[0])
	}
}

func TestFeedbackWriteOnceFiresOnce(t *testing.T) {
	cap := &capturingLogger{}
	state := newFeedbackWarningState()
	for i := 0; i < 50; i++ {
		state.warnFeedbackWriteOnce(cap, nil)
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("warnFeedbackWriteOnce fired %d times, want 1", got)
	}
}

func TestRouteIDRandOnceFiresOnce(t *testing.T) {
	cap := &capturingLogger{}
	state := newFeedbackWarningState()
	for i := 0; i < 50; i++ {
		state.warnRouteIDRandOnce(cap, nil)
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Fatalf("warnRouteIDRandOnce fired %d times, want 1", got)
	}
}

func TestFeedbackLoggerNilSafe(t *testing.T) {
	// A nil logger must not panic.
	state := newFeedbackWarningState()
	state.warnFeedbackReadOnce(nil, FeedbackKey{}, nil)
	state.warnFeedbackWriteOnce(nil, nil)
	state.warnRouteIDRandOnce(nil, nil)
}

// TestFeedbackLoggerNilDoesNotConsumeOnce locks in the contract that
// invoking a warn-once emitter with a nil logger leaves the sync.Once
// armed, so a subsequent call with a real logger still emits. Caught a
// real footgun: the previous shape ran the nil check INSIDE Once.Do,
// which silently burned the Once whenever a caller raced ahead of
// logger attachment.
func TestFeedbackLoggerNilDoesNotConsumeOnce(t *testing.T) {
	state := newFeedbackWarningState()
	// Burn each emitter with nil logger first.
	state.warnFeedbackReadOnce(nil, FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}, errors.New("dropped"))
	state.warnFeedbackWriteOnce(nil, errors.New("dropped"))
	state.warnRouteIDRandOnce(nil, errors.New("dropped"))

	// Now attach a real logger and invoke each emitter. All three must emit.
	cap := &capturingLogger{}
	state.warnFeedbackReadOnce(cap, FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}, errors.New("read-emitted"))
	state.warnFeedbackWriteOnce(cap, errors.New("write-emitted"))
	state.warnRouteIDRandOnce(cap, errors.New("rng-emitted"))

	msgs := cap.snapshot()
	if len(msgs) != 3 {
		t.Fatalf("after nil-then-real, got %d messages, want 3", len(msgs))
	}
	joined := strings.Join(msgs, "|")
	for _, want := range []string{"read-emitted", "write-emitted", "rng-emitted"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rendered messages %q missing %q; once was consumed during nil-logger call", joined, want)
		}
	}
}

func TestFeedbackLoggerDirectInjection(t *testing.T) {
	cap := &capturingLogger{}
	router, _ := setupTestRouter(t)
	router.feedbackLogger = cap // direct field set; no exported option
	if router.feedbackLogger != cap {
		t.Errorf("feedbackLogger direct injection failed")
	}
}

func TestDefaultFeedbackLoggerIsNonNil(t *testing.T) {
	router, _ := setupTestRouter(t)
	if router.feedbackLogger == nil {
		t.Errorf("feedbackLogger default is nil; expected defaultFeedbackLogger")
	}
}
