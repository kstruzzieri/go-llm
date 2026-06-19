package agent

import (
	"context"

	"github.com/kstruzzieri/go-llm/provider"
)

// Observer receives live deltas during Run. Callbacks are invoked serially in
// loop order, in the calling goroutine; returning an error aborts Run and
// propagates. Result is the canonical final state.
type Observer interface {
	OnStep(ctx context.Context, e StepEvent) error
	OnToolCall(ctx context.Context, e ToolCallEvent) error
	OnToken(ctx context.Context, e TokenEvent) error
}

// StepEvent reports a completed model turn.
type StepEvent struct {
	Index        int
	Response     provider.ChatResponse
	RouteOutcome *provider.RouteOutcome
	Pressure     Pressure
}

// ToolCallEvent reports a tool about to be invoked (post-approval).
type ToolCallEvent struct {
	Step    int
	Call    provider.ToolCall
	Effect  Effect
	Preview string
}

// TokenEvent reports a streamed content delta from the model.
type TokenEvent struct {
	Step    int
	Content string
}

type nopObserver struct{}

func (nopObserver) OnStep(context.Context, StepEvent) error         { return nil }
func (nopObserver) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (nopObserver) OnToken(context.Context, TokenEvent) error       { return nil }

func normalizeObserver(o Observer) Observer {
	if o == nil {
		return nopObserver{}
	}
	return o
}
