package main

import (
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
)

// agentUseCase is the routing use case for ordinary execution: REPL and
// one-shot turns, AgentFlow task steps, parallel workers, and dispatch
// children. Named rather than repeated as a literal so the one place that
// chooses between it and planning is greppable.
const agentUseCase = "agent"

// chainPlan is the resolved routing strategy for ONE use case. Exactly one of
// chain or useRecommend is meaningful; both are zero only when the resolver
// also returns an error, so callers must check err before reading them.
//
// useCase travels WITH the chain because a process resolves exactly one active
// route (#476 D3) that then feeds destination admission, tool-capability
// preflight, input-ceiling derivation, and caller construction. Passing the
// chain alone to those seams and restating the use case at each one is what
// lets them silently disagree; carrying both on one value makes the mismatch
// unrepresentable (#476 I5).
type chainPlan struct {
	chain        []string // strict PreferredChain when non-empty
	useRecommend bool     // true => empty-Model recommend (no applicable default, or nil cfg)
	useCase      string   // the routing use case this plan was resolved FOR
}

// loadConfig applies golem's config-discovery rules. An explicit path that
// fails to load is fatal; auto-discovery that finds nothing returns (nil, nil)
// so the caller passes Config:nil to providerbootstrap.New (default Ollama).
func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("golem: load config %q: %w", path, err)
		}
		return cfg, nil
	}
	cfg, err := config.Default()
	if err != nil {
		if errors.Is(err, config.ErrConfigNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("golem: discover config: %w", err)
	}
	return cfg, nil
}

// resolveAgentChain gates strict-chain vs recommend on the EFFECTIVE config
// (bundle.Config). defaults.agent absent (or nil cfg) => recommend; present
// and resolvable => strict chain; present but unresolvable => fatal.
func resolveAgentChain(cfg *config.Config) (chainPlan, error) {
	if cfg == nil {
		return chainPlan{useRecommend: true, useCase: agentUseCase}, nil
	}
	if _, ok := cfg.Defaults[agentUseCase]; !ok {
		return chainPlan{useRecommend: true, useCase: agentUseCase}, nil
	}
	chain, err := cfg.RoleFallbackChain(agentUseCase)
	if err != nil {
		return chainPlan{}, fmt.Errorf("golem: resolve agent chain: %w", err)
	}
	return chainPlan{chain: chain, useCase: agentUseCase}, nil
}

// resolvePlanningChain resolves the AgentFlow plan-authoring route (#476).
//
// It differs from resolveAgentChain in one way that matters: the presence
// check goes through RoleForUseCase, so an absent "planning" key still
// resolves through the ordered fallbacks (reasoning, analysis, agent) instead
// of dropping straight to recommendation. Only when none of those is
// configured does planning fall back to recommendation, matching the agent
// route's own configless behavior.
//
// A resolvable use case whose ROLE is invalid or circular is fatal here, as it
// is for the agent chain: a plan authored by an unresolvable route is worse
// than a startup error.
func resolvePlanningChain(cfg *config.Config) (chainPlan, error) {
	if cfg == nil {
		return chainPlan{useRecommend: true, useCase: config.UseCasePlanning}, nil
	}
	if _, ok := cfg.RoleForUseCase(config.UseCasePlanning); !ok {
		return chainPlan{useRecommend: true, useCase: config.UseCasePlanning}, nil
	}
	chain, err := cfg.RoleFallbackChain(config.UseCasePlanning)
	if err != nil {
		return chainPlan{}, fmt.Errorf("golem: resolve planning chain: %w", err)
	}
	return chainPlan{chain: chain, useCase: config.UseCasePlanning}, nil
}

// resolveActiveChain resolves the SINGLE model route this process runs on.
//
// -goal (AgentFlow plan authoring) is mutually exclusive with every execution
// mode -- task plans, -p, write/exec, RAG, delegate, dispatch, MCP -- and
// exits after locking a plan. One live route per process is therefore
// sufficient, and a second orchestrator or a phase state machine would be
// machinery with nothing to select between.
func resolveActiveChain(cfg *config.Config, goalMode bool) (chainPlan, error) {
	if goalMode {
		return resolvePlanningChain(cfg)
	}
	return resolveAgentChain(cfg)
}

func resolveSummarizeChain(cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	if _, ok := cfg.RoleForUseCase(config.UseCaseSummarize); !ok {
		return nil, nil
	}
	chain, err := cfg.RoleFallbackChain(config.UseCaseSummarize)
	if err != nil {
		return nil, fmt.Errorf("golem: resolve summarize chain: %w", err)
	}
	return chain, nil
}
