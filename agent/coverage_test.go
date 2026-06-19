package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// bigOutputTool returns content larger than its OutputCap so the runtime must truncate.
type bigOutputTool struct{}

func (bigOutputTool) Spec() ToolSpec { return ToolSpec{Name: "big", Parameters: json.RawMessage(`{}`)} }
func (bigOutputTool) Effect() Effect { return Effect{Class: Read, OutputCap: 8} }
func (bigOutputTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: strings.Repeat("A", 100)}, nil
}

func TestDispatchTruncatesOutputCap(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "big", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{bigOutputTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed {
		t.Fatalf("stop = %v", res.StopReason)
	}
}

// blockingTool blocks until its context is cancelled, proving the runtime enforces the timeout.
type blockingTool struct{}

func (blockingTool) Spec() ToolSpec {
	return ToolSpec{Name: "block", Parameters: json.RawMessage(`{}`)}
}
func (blockingTool) Effect() Effect { return Effect{Class: Read, Timeout: 20 * time.Millisecond} }
func (blockingTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	<-ctx.Done()
	return ToolResult{}, ctx.Err()
}

func TestDispatchEnforcesTimeout(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "block", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	// The blocking tool's Invoke returns ctx.Err() (a tool error) once the per-call
	// timeout fires; that becomes an IsError observation and the loop continues.
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{blockingTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].IsError {
		t.Fatalf("timeout must surface as an IsError observation, got %+v", res.ToolCalls)
	}
}

// routeOutcomeObserver captures the RouteOutcome delivered to OnStep.
type routeOutcomeObserver struct{ got *provider.RouteOutcome }

func (o *routeOutcomeObserver) OnStep(_ context.Context, e StepEvent) error {
	o.got = e.RouteOutcome
	return nil
}
func (*routeOutcomeObserver) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (*routeOutcomeObserver) OnToken(context.Context, TokenEvent) error       { return nil }

func TestStepEventCarriesRouteOutcome(t *testing.T) {
	outcome := &provider.RouteOutcome{}
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "x", Done: true}, RouteOutcome: outcome},
	}}
	o := newTestOrchestrator(mc)
	obs := &routeOutcomeObserver{}
	if _, err := o.Run(context.Background(), Request{Goal: "q"}, obs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if obs.got != outcome {
		t.Fatal("StepEvent.RouteOutcome must carry the captured route outcome")
	}
}
