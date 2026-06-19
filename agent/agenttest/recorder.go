// Package agenttest provides test helpers for the agent runtime.
package agenttest

import (
	"context"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
)

// RecorderObserver records the ordered sequence of observer callbacks so loop
// tests can assert the exact event transcript.
type RecorderObserver struct {
	mu     sync.Mutex
	Kinds  []string // "step" | "tool_call" | "token"
	Steps  []agent.StepEvent
	Calls  []agent.ToolCallEvent
	Tokens []agent.TokenEvent
}

func (r *RecorderObserver) OnStep(_ context.Context, e agent.StepEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Kinds = append(r.Kinds, "step")
	r.Steps = append(r.Steps, e)
	return nil
}

func (r *RecorderObserver) OnToolCall(_ context.Context, e agent.ToolCallEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Kinds = append(r.Kinds, "tool_call")
	r.Calls = append(r.Calls, e)
	return nil
}

func (r *RecorderObserver) OnToken(_ context.Context, e agent.TokenEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Kinds = append(r.Kinds, "token")
	r.Tokens = append(r.Tokens, e)
	return nil
}
