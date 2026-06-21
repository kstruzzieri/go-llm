package main

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakePlan is a chatStreamer that emits one chunk carrying a RouteOutcome.
type fakePlan struct {
	outcome *provider.RouteOutcome
}

func (p fakePlan) ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error {
	return fn(provider.ChatResponse{Content: "hi", RouteOutcome: p.outcome})
}

func TestChainModelCaller_SetsChainAndCaps(t *testing.T) {
	var captured provider.RoutingRequest
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "ollama", Model: "m1"}}
	mc := &chainModelCaller{
		chain: []string{"ollama/m1", "ollama/m2"},
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{outcome: outcome}, nil
		},
	}

	res, err := mc.Chat(context.Background(),
		provider.ChatRequest{
			Messages: []provider.ChatMessage{{Role: "user", Content: "hello"}},
			Tools:    []provider.Tool{{}},
		}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if captured.UseCase != "agent" {
		t.Errorf("UseCase = %q, want agent", captured.UseCase)
	}
	wantCaps := provider.CapChat | provider.CapStream | provider.CapToolCall
	if captured.RequiredCaps != wantCaps {
		t.Errorf("RequiredCaps = %v, want %v", captured.RequiredCaps, wantCaps)
	}
	if !captured.StrictChain {
		t.Error("StrictChain = false, want true")
	}
	if len(captured.PreferredChain) != 2 || captured.PreferredChain[0] != "ollama/m1" {
		t.Errorf("PreferredChain = %v, want [ollama/m1 ollama/m2]", captured.PreferredChain)
	}
	if res.RouteOutcome == nil || res.RouteOutcome.ActualModel.Model != "m1" {
		t.Errorf("RouteOutcome not captured: %+v", res.RouteOutcome)
	}
}

func TestChainModelCaller_NoTools_DropsToolCallCap(t *testing.T) {
	var captured provider.RoutingRequest
	mc := &chainModelCaller{
		chain: []string{"ollama/m1"},
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{}, nil
		},
	}
	if _, err := mc.Chat(context.Background(), provider.ChatRequest{}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.RequiredCaps != provider.CapChat|provider.CapStream {
		t.Errorf("RequiredCaps = %v, want chat|stream", captured.RequiredCaps)
	}
	if captured.RequiredCaps&provider.CapToolCall != 0 {
		t.Error("CapToolCall set without tools")
	}
}

func TestChainModelCaller_EmptyChain_UsesRecommend(t *testing.T) {
	var captured provider.RoutingRequest
	mc := &chainModelCaller{
		chain: nil,
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{}, nil
		},
	}
	if _, err := mc.Chat(context.Background(), provider.ChatRequest{Tools: []provider.Tool{{}}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.StrictChain {
		t.Error("StrictChain = true with empty chain, want false (recommend)")
	}
	if len(captured.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty", captured.PreferredChain)
	}
}

// Compile-time assertion: chainModelCaller satisfies agent.ModelCaller.
var _ agent.ModelCaller = (*chainModelCaller)(nil)
