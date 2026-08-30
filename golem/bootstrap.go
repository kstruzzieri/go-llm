package golem

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// ErrDestinationPolicyIneffective reports a nonzero Options.DestinationPolicy
// combined with a caller-supplied Orchestrator: this package then owns no
// provider transports, so accepting the policy would imply protection that
// does not exist (#477 D9).
var ErrDestinationPolicyIneffective = errors.New(
	"golem: DestinationPolicy has no effect with a caller-supplied Orchestrator; the host owns those transports")

// bootstrapOrchestrator builds the config-driven orchestrator behind the
// destination-admission boundary (#477): the frozen network plan resolves
// every route BEFORE any I/O, the policy is checked against the resulting
// manifest — the zero value denies any reachable remote with a typed error
// while admitting local-only configurations — and the bundle's clients,
// registry, and router are all gated. progressive enables #331 mixed context
// assembly on the returned orchestrator's ContextManager; off leaves the
// legacy assembly path untouched.
func bootstrapOrchestrator(ctx context.Context, configPath string, policy provider.DestinationPolicy, progressive bool, onWarning func(error)) (*agent.Orchestrator, *providerbootstrap.Bundle, conversation.Summarizer, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	agentRoute, err := providerbootstrap.PlanAgentRoute(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("golem: resolve agent chain: %w", err)
	}
	summarizeRoute, err := providerbootstrap.PlanOptionalUseCaseRoute(cfg, config.UseCaseSummarize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("golem: resolve summarize chain: %w", err)
	}
	eff, err := providerbootstrap.Materialize(cfg, "", "", "")
	if err != nil {
		return nil, nil, nil, err
	}
	plan, err := providerbootstrap.BuildNetworkPlan(eff,
		[]providerbootstrap.PlannedRoute{agentRoute, summarizeRoute},
		providerbootstrap.PlanOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	manifest, err := provider.NewDestinationManifest(plan.Edges...)
	if err != nil {
		return nil, nil, nil, err
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(policy, manifest); err != nil {
		// Typed (provider.ErrDestinationDenied) and BEFORE any outbound
		// byte: the denial names the destination and the use case that
		// reaches it.
		return nil, nil, nil, err
	}
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("golem: bootstrap providers: %w", err)
	}
	for _, warning := range bundle.Warnings {
		if onWarning != nil {
			onWarning(warning)
		}
	}
	// #477 D8: the chains come from the frozen routes above, never
	// re-resolved after admission. A recommend route is the nil chain,
	// preserving both callers' non-strict behavior exactly.
	return agent.New(agent.NewRouterModelCallerWithChain(bundle.Router, agentRoute.Chain), agent.ContextManager{Mixed: progressive}),
		bundle, agent.NewRouterSummarizer(bundle.Router, summarizeRoute.Chain), nil
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("golem: load config %q: %w", path, err)
		}
		return cfg, nil
	}
	cfg, err := config.Default()
	if errors.Is(err, config.ErrConfigNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("golem: discover config: %w", err)
	}
	return cfg, nil
}
