package agent

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// fakePlan emits two content deltas then a Done chunk carrying RouteOutcome.
type fakePlan struct {
	outcome *provider.RouteOutcome
}

func (p fakePlan) ExecuteChatStream(_ context.Context, fn func(provider.ChatResponse) error) error {
	if err := fn(provider.ChatResponse{Content: "Hel"}); err != nil {
		return err
	}
	if err := fn(provider.ChatResponse{Content: "lo"}); err != nil {
		return err
	}
	return fn(provider.ChatResponse{Done: true, RouteOutcome: p.outcome})
}

func TestRouterModelCallerCapturesRouteOutcomeAndStreams(t *testing.T) {
	outcome := &provider.RouteOutcome{}
	mc := &routerModelCaller{
		route: func(context.Context, provider.RoutingRequest) (planExecutor, error) {
			return fakePlan{outcome: outcome}, nil
		},
	}
	var streamed string
	res, err := mc.Chat(context.Background(), provider.ChatRequest{}, func(c provider.ChatResponse) error {
		streamed += c.Content
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if streamed != "Hello" {
		t.Fatalf("streamed = %q, want %q", streamed, "Hello")
	}
	if res.Response.Content != "Hello" {
		t.Fatalf("final content = %q, want accumulated 'Hello'", res.Response.Content)
	}
	if res.RouteOutcome != outcome {
		t.Fatal("RouteOutcome must be captured from the Done chunk")
	}
}

func TestRouterModelCallerAddsToolCapWhenToolsPresent(t *testing.T) {
	var gotReq provider.RoutingRequest
	mc := &routerModelCaller{
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			gotReq = rr
			return fakePlan{}, nil
		},
	}
	_, err := mc.Chat(context.Background(),
		provider.ChatRequest{Tools: []provider.Tool{{Type: "function"}}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotReq.UseCase != "agent" {
		t.Fatalf("UseCase = %q, want agent", gotReq.UseCase)
	}
	if gotReq.RequiredCaps&provider.CapToolCall == 0 {
		t.Fatal("CapToolCall must be required when tools are present")
	}
}

func TestRouterModelCallerUsesNumPredictAsExpectedOutput(t *testing.T) {
	var gotReq provider.RoutingRequest
	mc := &routerModelCaller{
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			gotReq = rr
			return fakePlan{}, nil
		},
	}
	_, err := mc.Chat(context.Background(),
		provider.ChatRequest{Options: provider.ModelOptions{NumPredict: 256}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotReq.ExpectedOutput != 256 {
		t.Fatalf("ExpectedOutput = %d, want 256", gotReq.ExpectedOutput)
	}
}
