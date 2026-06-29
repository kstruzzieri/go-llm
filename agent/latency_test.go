package agent

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic, concurrency-safe clock for latency tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestWithClock_OverridesNow(t *testing.T) {
	fixed := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	o := New(nil, ContextManager{}, WithClock(func() time.Time { return fixed }))
	if got := o.now(); !got.Equal(fixed) {
		t.Fatalf("now = %v, want %v", got, fixed)
	}
}

func TestNew_DefaultClockIsTimeNow(t *testing.T) {
	o := New(nil, ContextManager{})
	if o.now == nil {
		t.Fatal("default clock is nil")
	}
	before := time.Now()
	if got := o.now(); got.Before(before) {
		t.Fatalf("default clock returned %v before %v", got, before)
	}
}
