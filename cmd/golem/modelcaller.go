package main

import (
	"context"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// chatStreamer is the minimal slice of *provider.RoutePlan the caller needs;
// mirrors agent.planExecutor (which is unexported, so we redeclare it here).
type chatStreamer interface {
	ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error
}

// chainModelCaller is an agent.ModelCaller that routes strictly down a
// configured fallback chain. When chain is non-empty it pins
// RoutingRequest.PreferredChain + StrictChain so the Router never appends a
// global-recommend tail — a coding agent must know which model answered. When
// chain is empty it falls back to the empty-Model recommend behavior of
// agent.NewRouterModelCaller.
type chainModelCaller struct {
	chain []string
	route func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error)
}

// newRouterChainCaller builds a chainModelCaller backed by a live Router.
func newRouterChainCaller(r *provider.Router, chain []string) agent.ModelCaller {
	return &chainModelCaller{
		chain: chain,
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			plan, err := r.Route(ctx, rr)
			if err != nil {
				return nil, err // avoid wrapping a nil *RoutePlan in a non-nil interface
			}
			return plan, nil
		},
	}
}

func (m *chainModelCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	rr := provider.RoutingRequest{
		UseCase:        "agent",
		Messages:       req.Messages,
		Tools:          req.Tools,
		Options:        req.Options,
		ExpectedOutput: req.Options.NumPredict,
		RequiredCaps:   provider.CapChat | provider.CapStream,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps |= provider.CapToolCall
	}
	if len(m.chain) > 0 {
		rr.PreferredChain = m.chain
		rr.StrictChain = true
	}

	plan, err := m.route(ctx, rr)
	if err != nil {
		return agent.ModelResult{}, err
	}

	var outcome *provider.RouteOutcome
	wrapped, getFinal := provider.Collect(func(chunk provider.ChatResponse) error {
		if chunk.RouteOutcome != nil {
			outcome = chunk.RouteOutcome
		}
		if onToken != nil {
			return onToken(chunk)
		}
		return nil
	})
	execErr := plan.ExecuteChatStream(ctx, wrapped)
	final := getFinal()
	final.RouteOutcome = outcome
	return agent.ModelResult{Response: final, RouteOutcome: outcome}, execErr
}
