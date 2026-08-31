package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

// Chain-resolution semantics (agent literal check, optional use-case
// fallbacks, fatal unresolvable roles) are owned and tested by the
// providerbootstrap planners -- the #477 network plan is the only place
// chains may be resolved. The tests here pin what cmd/golem adds on top:
// which route is ACTIVE for a mode, and that the derived chainPlan carries
// the planned route's chain, recommend marker, and use case unchanged.

func TestPlanActiveRoute_SelectsRouteByMode(t *testing.T) {
	// The single active route (#476 D3, I5). Goal mode authors plans and is
	// mutually exclusive with every execution mode, so one route per process
	// suffices -- but it must be the RIGHT one at every seam, which is why
	// the use case travels on the value rather than being restated.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"ollama": {}},
		Models: map[string]config.ModelConfig{
			"planner": {Name: "big", Provider: "ollama"},
			"agentic": {Name: "fast", Provider: "ollama"},
		},
		Defaults: map[string]string{"planning": "planner", "agent": "agentic"},
	}
	goal, err := planActiveRoute(cfg, true)
	if err != nil {
		t.Fatalf("goal mode: %v", err)
	}
	if goal.UseCase != config.UseCasePlanning {
		t.Errorf("goal mode UseCase = %q, want %q", goal.UseCase, config.UseCasePlanning)
	}
	if !reflect.DeepEqual(goal.Chain, []string{"ollama/big"}) {
		t.Errorf("goal mode chain = %v, want the planning chain [ollama/big]", goal.Chain)
	}

	exec, err := planActiveRoute(cfg, false)
	if err != nil {
		t.Fatalf("execution mode: %v", err)
	}
	if exec.UseCase != agentUseCase {
		t.Errorf("execution UseCase = %q, want %q", exec.UseCase, agentUseCase)
	}
	if !reflect.DeepEqual(exec.Chain, []string{"ollama/fast"}) {
		t.Errorf("execution chain = %v, want the agent chain [ollama/fast]", exec.Chain)
	}
}

func TestPlanActiveRoute_PlanningHopsAndSelectors(t *testing.T) {
	// Chains carry provider-qualified selectors and configured model
	// fallbacks, exactly as the agent chain does: whatever admission,
	// preflight, and the caller consume must be the full reachable set, not
	// the primary alone (#476 I2).
	models := map[string]config.ModelConfig{
		"planner":  {Name: "big", Provider: "ollama", Fallbacks: []string{"backup"}},
		"backup":   {Name: "small", Provider: "ollama"},
		"reasoner": {Name: "think", Provider: "openai"},
		"analyst":  {Name: "mid", Provider: "ollama"},
		"agentic":  {Name: "fast", Provider: "ollama"},
		"chatty":   {Name: "chat", Provider: "ollama"},
	}
	providers := map[string]config.ProviderConfig{"ollama": {}, "openai": {}}
	tests := []struct {
		name          string
		defaults      map[string]string
		wantChain     []string
		wantRecommend bool
	}{
		{"explicit planning wins, with its model fallbacks",
			map[string]string{"planning": "planner", "reasoning": "reasoner", "agent": "agentic"},
			[]string{"ollama/big", "ollama/small"}, false},
		{"reasoning hop",
			map[string]string{"reasoning": "reasoner", "analysis": "analyst", "agent": "agentic"},
			[]string{"openai/think"}, false},
		{"analysis hop",
			map[string]string{"analysis": "analyst", "agent": "agentic"},
			[]string{"ollama/mid"}, false},
		{"agent hop",
			map[string]string{"agent": "agentic"},
			[]string{"ollama/fast"}, false},
		{"no applicable default falls back to recommendation",
			map[string]string{"chat": "chatty"},
			nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Providers: providers, Models: models, Defaults: tt.defaults}
			route, err := planActiveRoute(cfg, true)
			if err != nil {
				t.Fatalf("planActiveRoute: %v", err)
			}
			if route.Recommend != tt.wantRecommend {
				t.Errorf("Recommend = %v, want %v", route.Recommend, tt.wantRecommend)
			}
			if !reflect.DeepEqual(route.Chain, tt.wantChain) {
				t.Errorf("chain = %v, want %v", route.Chain, tt.wantChain)
			}
			if route.UseCase != config.UseCasePlanning {
				t.Errorf("UseCase = %q, want %q", route.UseCase, config.UseCasePlanning)
			}
		})
	}
}

func TestPlanActiveRoute_NilConfigRecommendsUnderTheModeUseCase(t *testing.T) {
	// Recommendation mode still routes UNDER the mode's use case; the use
	// case is a property of the route, not of having a chain.
	goal, err := planActiveRoute(nil, true)
	if err != nil {
		t.Fatalf("goal: %v", err)
	}
	if !goal.Recommend || goal.UseCase != config.UseCasePlanning {
		t.Errorf("goal route = %+v, want recommend under planning", goal)
	}
	exec, err := planActiveRoute(nil, false)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !exec.Recommend || exec.UseCase != agentUseCase {
		t.Errorf("exec route = %+v, want recommend under agent", exec)
	}
}

func TestPlanActiveRoute_UnresolvablePlanningRoleFatal(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{"unknown model fallback", &config.Config{
			Models:   map[string]config.ModelConfig{"planner": {Name: "big", Provider: "ollama", Fallbacks: []string{"ghost"}}},
			Defaults: map[string]string{"planning": "planner"},
		}},
		{"empty role mapping", &config.Config{
			Models:   map[string]config.ModelConfig{"planner": {Name: "big", Provider: "ollama"}},
			Defaults: map[string]string{"planning": ""},
		}},
		{"circular fallbacks", &config.Config{
			Models: map[string]config.ModelConfig{
				"planner": {Name: "big", Provider: "ollama", Fallbacks: []string{"other"}},
				"other":   {Name: "mid", Provider: "ollama", Fallbacks: []string{"planner"}},
			},
			Defaults: map[string]string{"planning": "planner"},
		}},
		{"role reached via a fallback hop is unresolvable", &config.Config{
			Models:   map[string]config.ModelConfig{"reasoner": {Name: "think", Provider: "ollama", Fallbacks: []string{"ghost"}}},
			Defaults: map[string]string{"reasoning": "reasoner"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := planActiveRoute(tt.cfg, true); err == nil {
				t.Fatal("err = nil, want a fatal startup error")
			}
		})
	}
}

func TestChainPlanFor_CarriesTheRouteUnchanged(t *testing.T) {
	// chainPlanFor is the only chainPlan constructor wiring may use: the
	// consumed chain, the admitted chain, and the stamped use case must be
	// one value (#476 I5). Dropping any field here silently detaches a seam
	// from the admitted plan.
	strict := providerbootstrap.PlannedRoute{
		UseCase: config.UseCasePlanning,
		Chain:   []string{"ollama/big", "ollama/small"},
	}
	got := chainPlanFor(strict)
	if !reflect.DeepEqual(got.chain, strict.Chain) || got.useRecommend || got.useCase != config.UseCasePlanning {
		t.Errorf("chainPlanFor(strict) = %+v, want chain/useCase carried and no recommend", got)
	}

	rec := providerbootstrap.PlannedRoute{UseCase: agentUseCase, Recommend: true}
	got = chainPlanFor(rec)
	if got.chain != nil || !got.useRecommend || got.useCase != agentUseCase {
		t.Errorf("chainPlanFor(recommend) = %+v, want recommend under agent with nil chain", got)
	}
}

func TestRecommendNotice_NamesTheActiveUseCase(t *testing.T) {
	// The agent wording is pinned byte-identical to the pre-#476 line (I9);
	// goal mode names what was actually absent -- the planning key AND every
	// fallback RoleForUseCase walked before recommending.
	agentWant := "no defaults.agent configured; using model recommendation (run will route to the recommended model)"
	if got := recommendNotice(agentUseCase); got != agentWant {
		t.Errorf("recommendNotice(agent) = %q, want the pre-#476 line %q", got, agentWant)
	}
	got := recommendNotice(config.UseCasePlanning)
	for _, want := range []string{"defaults.planning", "reasoning", "analysis", "agent", "model recommendation"} {
		if !strings.Contains(got, want) {
			t.Errorf("recommendNotice(planning) = %q, missing %q", got, want)
		}
	}
	if got == agentWant {
		t.Error("planning notice must not claim defaults.agent was the absent key")
	}
}

func TestLoadConfig_ExplicitBadPathFatal(t *testing.T) {
	if _, err := loadConfig("/nonexistent/path/models.json"); err == nil {
		t.Fatal("err = nil for nonexistent explicit config path, want fatal")
	}
}
