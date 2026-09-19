package main

import (
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

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
	chain             []string // strict PreferredChain when non-empty
	useRecommend      bool     // true => empty-Model recommend (no applicable default, or nil cfg)
	useCase           string   // the routing use case this plan was resolved FOR
	suppliedByUseCase string   // Defaults key that supplied the active role
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

// planActiveRoute resolves the SINGLE model route this process runs on (#476
// D3): the planning route in -goal mode, the agent route otherwise.
//
// -goal (AgentFlow plan authoring) is mutually exclusive with every execution
// mode -- task plans, -p, write/exec, RAG, delegate, dispatch, MCP -- and
// exits after locking a plan. One live route per process is therefore
// sufficient, and a second orchestrator or a phase state machine would be
// machinery with nothing to select between.
//
// Resolution is delegated to the providerbootstrap planners because the #477
// network plan is the ONLY place chains may be resolved -- nothing after the
// plan may call RoleFallbackChain independently, or the consumed chain and
// the admitted chain could diverge. The planning route uses the optional
// use-case planner: an absent "planning" key resolves through the ordered
// fallbacks (reasoning, analysis, agent), and only when none of those is
// configured does planning degrade to recommendation, matching the agent
// route's own configless behavior. A resolvable use case whose ROLE is
// invalid or circular is fatal either way: a plan authored by an
// unresolvable route is worse than a startup error.
func planActiveRoute(cfg *config.Config, goalMode bool) (providerbootstrap.PlannedRoute, error) {
	if goalMode {
		return providerbootstrap.PlanOptionalUseCaseRoute(cfg, config.UseCasePlanning)
	}
	return providerbootstrap.PlanAgentRoute(cfg)
}

// recommendNotice renders the startup line for a mode whose active route
// resolved to recommendation. Parameterized by the active use case rather
// than duplicated per mode (#476 D5): the agent wording is byte-identical to
// the pre-#476 line, and goal mode names what was actually absent -- the
// planning key AND every one of its fallbacks, since RoleForUseCase walked
// them all before recommending.
func recommendNotice(useCase string) string {
	if useCase == config.UseCasePlanning {
		return "no defaults.planning configured (and no reasoning, analysis, or agent fallback); using model recommendation (run will route to the recommended model)"
	}
	return "no defaults.agent configured; using model recommendation (run will route to the recommended model)"
}

// chainPlanFor derives the cmd-local chainPlan from the planned active route.
// This is the ONLY chainPlan constructor wiring may use: deriving from the
// PlannedRoute that entered the network plan is what keeps the consumed
// chain, the admitted chain, and the stamped use case one value (#476 I5).
func chainPlanFor(route providerbootstrap.PlannedRoute) chainPlan {
	return chainPlan{
		chain:             route.Chain,
		useRecommend:      route.Recommend,
		useCase:           route.UseCase,
		suppliedByUseCase: route.SuppliedByUseCase,
	}
}
