package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
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

func TestRouterSummarizerRoutesSummarizeUseCase(t *testing.T) {
	var gotReq provider.RoutingRequest
	s := &routerSummarizer{
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			gotReq = rr
			return fakePlan{}, nil
		},
	}

	got, err := s.Summarize(context.Background(), "PRIOR-SUMMARY",
		[]conversation.Message{{Role: "user", Content: "old turn"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got != "Hello" {
		t.Fatalf("summary = %q, want collected model output", got)
	}
	if gotReq.UseCase != "summarize" {
		t.Fatalf("UseCase = %q, want summarize", gotReq.UseCase)
	}
	if gotReq.RequiredCaps != provider.CapChat|provider.CapStream {
		t.Fatalf("RequiredCaps = %s, want chat|stream", gotReq.RequiredCaps)
	}
	if gotReq.Options.NumPredict != DefaultSummaryOutputReserve {
		t.Fatalf("NumPredict = %d, want %d", gotReq.Options.NumPredict, DefaultSummaryOutputReserve)
	}
	if len(gotReq.Messages) != 2 {
		t.Fatalf("want system+user, got %d messages", len(gotReq.Messages))
	}
	if !strings.Contains(gotReq.Messages[0].Content, "Do not invent facts.") ||
		!strings.Contains(gotReq.Messages[0].Content, "Open tasks:") {
		t.Fatalf("system prompt missing constraints/sections: %q", gotReq.Messages[0].Content)
	}
	if !strings.Contains(gotReq.Messages[1].Content, "PRIOR-SUMMARY") ||
		!strings.Contains(gotReq.Messages[1].Content, "user: old turn") {
		t.Fatalf("user content missing prior/transcript: %q", gotReq.Messages[1].Content)
	}
}

func TestRouterSummarizerUsesStrictPreferredChain(t *testing.T) {
	var gotReq provider.RoutingRequest
	s := &routerSummarizer{
		chain: []string{"ollama/light", "hosted/big"},
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			gotReq = rr
			return fakePlan{}, nil
		},
	}

	if _, err := s.Summarize(context.Background(), "",
		[]conversation.Message{{Role: "user", Content: "old turn"}}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !gotReq.StrictChain {
		t.Fatal("StrictChain = false, want true")
	}
	if len(gotReq.PreferredChain) != 2 || gotReq.PreferredChain[0] != "ollama/light" || gotReq.PreferredChain[1] != "hosted/big" {
		t.Fatalf("PreferredChain = %v, want summarize chain", gotReq.PreferredChain)
	}
}
