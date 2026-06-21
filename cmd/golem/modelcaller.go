package main

import (
	"context"
	"fmt"
	"strings"

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

// capChecker is the narrow subset of *provider.ModelRegistry the preflight needs.
type capChecker interface {
	Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error)
	LookupAny(ctx context.Context, model string) ([]*provider.ModelProfile, error)
	Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error)
}

const toolRouteCaps = provider.CapChat | provider.CapStream | provider.CapToolCall

func profileToolCapable(p *provider.ModelProfile) bool {
	// Capability.Has tests c&flag == flag across all bits, so reusing the
	// toolRouteCaps mask keeps the required-capability set defined in one place.
	return p != nil && p.Caps.Has(toolRouteCaps)
}

func parseSelector(sel string) (provider.ModelKey, bool) {
	pname, model, ok := strings.Cut(sel, "/")
	if !ok {
		return provider.ModelKey{}, false
	}
	return provider.ModelKey{Provider: pname, Model: model}, true
}

// preflightToolCapable verifies the agent chain can route a tool-capable model.
// It returns warnings for each configured entry that cannot (the Router may
// still reach them on fallback), and an error only when NO entry — or, for an
// empty chain, the recommend set — can satisfy chat|stream|tool_call. Pure
// registry lookup; never makes a live model call.
func preflightToolCapable(ctx context.Context, reg capChecker, chain []string) (warnings []string, err error) {
	if len(chain) == 0 {
		profs, rerr := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps})
		if rerr != nil {
			return nil, fmt.Errorf("golem: tool-capability preflight (recommend): %w", rerr)
		}
		if len(profs) == 0 {
			return nil, fmt.Errorf("golem: no tool-capable model available (require chat|stream|tool_call)")
		}
		return nil, nil
	}

	capable := 0
	for _, sel := range chain {
		ok := false
		if key, parsed := parseSelector(sel); parsed {
			if p, lerr := reg.Lookup(ctx, key); lerr == nil && profileToolCapable(p) {
				ok = true
			}
		} else if profs, lerr := reg.LookupAny(ctx, sel); lerr == nil {
			for _, p := range profs {
				if profileToolCapable(p) {
					ok = true
					break
				}
			}
		}
		if ok {
			capable++
		} else {
			warnings = append(warnings, fmt.Sprintf("agent fallback %q is not tool-capable (chat|stream|tool_call)", sel))
		}
	}
	if capable == 0 {
		return warnings, fmt.Errorf("golem: no entry in the agent chain is tool-capable (chat|stream|tool_call): %v", chain)
	}
	return warnings, nil
}
