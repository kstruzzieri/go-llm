package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
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

// noToolModel returns a single final answer and advances the clock by d on each
// Chat call so the orchestrator's measured model latency is exactly d.
type noToolModel struct {
	clk *fakeClock
	d   time.Duration
}

func (m noToolModel) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
	m.clk.advance(m.d)
	return ModelResult{Response: provider.ChatResponse{Content: "done"}}, nil
}

// stepCapture records StepEvents to assert on the live observer projection.
type stepCapture struct{ steps []StepEvent }

func (c *stepCapture) OnStep(_ context.Context, e StepEvent) error {
	c.steps = append(c.steps, e)
	return nil
}
func (c *stepCapture) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (c *stepCapture) OnToken(context.Context, TokenEvent) error       { return nil }

func TestRun_RecordsModelStepLatency(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	o := New(noToolModel{clk: clk, d: 250 * time.Millisecond}, ContextManager{}, WithClock(clk.now))

	cap := &stepCapture{}
	res, err := o.Run(context.Background(), Request{Goal: "hi"}, cap)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(res.Steps))
	}
	if res.Steps[0].Latency != 250*time.Millisecond {
		t.Fatalf("StepRecord.Latency = %v, want 250ms", res.Steps[0].Latency)
	}
	if len(cap.steps) != 1 || cap.steps[0].Latency != 250*time.Millisecond {
		t.Fatalf("StepEvent.Latency = %v, want 250ms", cap.steps)
	}
}
