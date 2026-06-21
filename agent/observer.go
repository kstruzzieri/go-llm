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

// ToolResultEvent reports a tool's result (or a synthetic dispatch failure such
// as unknown tool / malformed args / planning failure / approval denial) after
// capping and BEFORE the runtime appends it to State. Result is byte-identical
// to the observation the model receives.
type ToolResultEvent struct {
	Step   int
	Call   provider.ToolCall
	Result ToolResult
}

// ToolResultObserver is an OPTIONAL extension of Observer. When an Observer also
// implements it, the Orchestrator calls OnToolResult for every ToolResult
// produced by dispatch — including synthetic failures — but NOT for hard
// dispatch aborts (context cancellation, approver error) that return before a
// ToolResult exists. Callbacks run serially in loop order in the calling
// goroutine; a returned error aborts Run, like the other observer callbacks.
type ToolResultObserver interface {
	OnToolResult(ctx context.Context, e ToolResultEvent) error
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
