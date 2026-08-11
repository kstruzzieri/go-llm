package agent

import (
	"context"
	"slices"
	"time"

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
	Latency      time.Duration
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
// as unknown tool / malformed args / planning failure / approval denial). For
// invoked tools it is the capped canonical fallback immediately after execution,
// before Context is cloned onto State or mixed assembly. Under mixed assembly
// Result is not necessarily byte-identical to the model's later input.
type ToolResultEvent struct {
	Step    int
	Call    provider.ToolCall
	Effect  Effect // normalized effect when known; zero value for unknown-tool / bad-JSON
	Result  ToolResult
	Denied  bool
	Invoked bool
	Latency time.Duration
}

// ToolResultObserver is an OPTIONAL extension of Observer. When an Observer also
// implements it, the Orchestrator calls OnToolResult for every ToolResult
// produced by dispatch — including synthetic failures — but NOT for hard
// dispatch aborts (context cancellation, approver error) that return before a
// ToolResult exists. It observes the canonical fallback, not a promise about
// the final model input. Callbacks run serially in loop order in the calling
// goroutine; a returned error aborts Run, like the other observer callbacks.
type ToolResultObserver interface {
	OnToolResult(ctx context.Context, e ToolResultEvent) error
}

// PressureObserver is an OPTIONAL extension of Observer. When an Observer also
// implements it, the Orchestrator calls OnPressure once per step immediately
// after assembling the turn's context and BEFORE the model call — including the
// path where assembly fails with ErrContextExhausted (so the most important
// pressure event is never invisible). A returned error aborts Run before the
// model call, like the other observer callbacks.
type PressureObserver interface {
	OnPressure(ctx context.Context, e PressureEvent) error
}

// PressureEvent reports the per-turn context pressure computed during assembly.
type PressureEvent struct {
	Step     int
	Pressure Pressure
}

// ThinkingEvent reports a streamed reasoning delta from the model, separated
// from answer content by the provider layer.
type ThinkingEvent struct {
	Step    int
	Content string
}

// ThinkingObserver is an OPTIONAL extension of Observer. When an Observer
// also implements it, the Orchestrator calls OnThinking for every reasoning
// delta before any OnToken for the same chunk. Observers that do not
// implement it keep today's behavior (thinking is dropped). A returned error
// aborts Run, like the other observer callbacks.
type ThinkingObserver interface {
	OnThinking(ctx context.Context, e ThinkingEvent) error
}

// ContextAssemblyEvent is one mixed assembly's content-free trace, tagged with
// the step whose model call it assembled.
type ContextAssemblyEvent struct {
	Step  int
	Trace ContextAssemblyTrace
}

// ContextAssemblyObserver is an OPTIONAL extension of Observer. When an Observer
// also implements it, the Orchestrator calls OnContextAssembly after every MIXED
// assembly, before the step's model call and — for the same step — after
// OnPressure. Legacy and no-anchor assemblies emit NOTHING (they return a zero
// trace), and so do assembly errors. A returned error aborts Run, like the other
// observer callbacks.
type ContextAssemblyObserver interface {
	OnContextAssembly(ctx context.Context, e ContextAssemblyEvent) error
}

// RetrievalPresentationEvent reports retrieval attribution in a tool message
// that survived final context assembly and was presented to the model. It
// carries no message content; Attribution.Sources is owned by the event.
type RetrievalPresentationEvent struct {
	Step        int
	ToolCallID  string
	Attribution RetrievalAttribution
}

// RetrievalPresentationObserver is an OPTIONAL extension of Observer. When an
// Observer also implements it, the Orchestrator calls OnRetrievalPresentation
// once for each attributed tool message in the final assembled prompt, after
// OnPressure and OnContextAssembly and before the step's model call. A returned
// error aborts Run before the model call, like the other observer callbacks.
type RetrievalPresentationObserver interface {
	OnRetrievalPresentation(ctx context.Context, e RetrievalPresentationEvent) error
}

func retrievalPresentationEvent(step int, msg Message) RetrievalPresentationEvent {
	return RetrievalPresentationEvent{
		Step:       step,
		ToolCallID: msg.ToolCallID,
		Attribution: RetrievalAttribution{
			Sources: slices.Clone(msg.Attrib.Sources),
		},
	}
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
