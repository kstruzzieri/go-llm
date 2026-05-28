package provider

import (
	"strings"
	"sync"
	"testing"
)

// capturingLogger implements feedbackLogger and records all messages.
type capturingLogger struct {
	mu       sync.Mutex
	messages []string
}

func (c *capturingLogger) Warnf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, format) // store format; tests assert substrings
	_ = args
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

	const iterations = 1000
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.warnFeedbackReadOnce(cap, FeedbackKey{Provider: "p", Model: "m", UseCase: "chat"}, nil)
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
