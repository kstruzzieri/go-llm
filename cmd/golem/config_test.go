package main

import (
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func TestResolveAgentChain_Present(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"ollama": {}},
		Models: map[string]config.ModelConfig{
			"primary": {Name: "m1", Provider: "ollama", Fallbacks: []string{"backup"}},
			"backup":  {Name: "m2", Provider: "ollama"},
		},
		Defaults: map[string]string{"agent": "primary"},
	}
	plan, err := resolveAgentChain(cfg)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if plan.useRecommend {
		t.Error("useRecommend = true, want chain")
	}
	want := []string{"ollama/m1", "ollama/m2"}
	if len(plan.chain) != 2 || plan.chain[0] != want[0] || plan.chain[1] != want[1] {
		t.Errorf("chain = %v, want %v", plan.chain, want)
	}
}

func TestResolveAgentChain_AbsentUsesRecommend(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama"}},
		Defaults: map[string]string{},
	}
	plan, err := resolveAgentChain(cfg)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !plan.useRecommend || len(plan.chain) != 0 {
		t.Errorf("plan = %+v, want useRecommend with empty chain", plan)
	}
}

func TestResolveAgentChain_NilConfigUsesRecommend(t *testing.T) {
	plan, err := resolveAgentChain(nil)
	if err != nil || !plan.useRecommend {
		t.Errorf("nil cfg: plan=%+v err=%v, want useRecommend", plan, err)
	}
}

func TestResolveAgentChain_MisconfiguredFatal(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama", Fallbacks: []string{"ghost"}}},
		Defaults: map[string]string{"agent": "primary"},
	}
	if _, err := resolveAgentChain(cfg); err == nil {
		t.Fatal("err = nil for unknown fallback role, want fatal")
	}
}

func TestResolveAgentChain_EmptyAgentKeyFatal(t *testing.T) {
	// Key present but mapping to "" is misconfiguration, not "unset": the
	// presence check passes, RoleFallbackChain("agent") then fails on role "".
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama"}},
		Defaults: map[string]string{"agent": ""},
	}
	if _, err := resolveAgentChain(cfg); err == nil {
		t.Fatal("err = nil for empty agent role, want fatal")
	}
}

func TestResolveSummarizeChain_UsesConfiguredFallbackChain(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"light":  {Name: "small", Provider: "ollama", Fallbacks: []string{"hosted"}},
			"hosted": {Name: "large", Provider: "openai"},
		},
		Defaults: map[string]string{"summarize": "light"},
	}
	chain, err := resolveSummarizeChain(cfg)
	if err != nil {
		t.Fatalf("resolveSummarizeChain: %v", err)
	}
	want := []string{"ollama/small", "openai/large"}
	if len(chain) != len(want) || chain[0] != want[0] || chain[1] != want[1] {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
}

func TestResolveSummarizeChain_FallbackOnlyUsesAnalysis(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"light": {Name: "small", Provider: "ollama"}},
		Defaults: map[string]string{"analysis": "light"}, // no "summarize"
	}
	chain, err := resolveSummarizeChain(cfg)
	if err != nil {
		t.Fatalf("resolveSummarizeChain: %v", err)
	}
	want := []string{"ollama/small"}
	if len(chain) != len(want) || chain[0] != want[0] {
		t.Fatalf("chain = %v, want %v (summarize -> analysis fallback)", chain, want)
	}
}

func TestResolveSummarizeChain_NoDefaultsReturnsNil(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"light": {Name: "small", Provider: "ollama"}},
		Defaults: map[string]string{}, // no summarize / analysis / chat
	}
	chain, err := resolveSummarizeChain(cfg)
	if err != nil {
		t.Fatalf("resolveSummarizeChain: %v", err)
	}
	if chain != nil {
		t.Fatalf("chain = %v, want nil (no resolvable summarize)", chain)
	}
}

func TestResolveSummarizeChain_FallbackOnlyUsesChat(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"light": {Name: "small", Provider: "ollama"}},
		Defaults: map[string]string{"chat": "light"}, // no summarize / analysis
	}
	chain, err := resolveSummarizeChain(cfg)
	if err != nil {
		t.Fatalf("resolveSummarizeChain: %v", err)
	}
	want := []string{"ollama/small"}
	if len(chain) != len(want) || chain[0] != want[0] {
		t.Fatalf("chain = %v, want %v (summarize -> chat fallback)", chain, want)
	}
}

func TestLoadConfig_ExplicitBadPathFatal(t *testing.T) {
	if _, err := loadConfig("/nonexistent/path/models.json"); err == nil {
		t.Fatal("err = nil for nonexistent explicit config path, want fatal")
	}
}

func TestResolvePlanningChain_HopsAndSelectors(t *testing.T) {
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
			cfg := &config.Config{Models: models, Defaults: tt.defaults}
			plan, err := resolvePlanningChain(cfg)
			if err != nil {
				t.Fatalf("resolvePlanningChain: %v", err)
			}
			if plan.useRecommend != tt.wantRecommend {
				t.Errorf("useRecommend = %v, want %v", plan.useRecommend, tt.wantRecommend)
			}
			if !reflect.DeepEqual(plan.chain, tt.wantChain) {
				t.Errorf("chain = %v, want %v", plan.chain, tt.wantChain)
			}
			if plan.useCase != config.UseCasePlanning {
				t.Errorf("useCase = %q, want %q", plan.useCase, config.UseCasePlanning)
			}
		})
	}
}

func TestResolvePlanningChain_NilConfigUsesRecommend(t *testing.T) {
	plan, err := resolvePlanningChain(nil)
	if err != nil {
		t.Fatalf("resolvePlanningChain(nil): %v", err)
	}
	if !plan.useRecommend || len(plan.chain) != 0 {
		t.Errorf("plan = %+v, want useRecommend with empty chain", plan)
	}
	// Recommendation mode still routes UNDER the planning use case; the
	// use case is a property of the route, not of having a chain.
	if plan.useCase != config.UseCasePlanning {
		t.Errorf("useCase = %q, want %q", plan.useCase, config.UseCasePlanning)
	}
}

func TestResolvePlanningChain_UnresolvableRoleFatal(t *testing.T) {
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
			if _, err := resolvePlanningChain(tt.cfg); err == nil {
				t.Fatal("err = nil, want a fatal startup error")
			}
		})
	}
}

func TestResolveActiveChain_SelectsRouteByMode(t *testing.T) {
	// The single active route (#476 D3, I5). Goal mode authors plans and is
	// mutually exclusive with every execution mode, so one route per process
	// suffices -- but it must be the RIGHT one at every seam, which is why
	// the use case travels on the value rather than being restated.
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"planner": {Name: "big", Provider: "ollama"},
			"agentic": {Name: "fast", Provider: "ollama"},
		},
		Defaults: map[string]string{"planning": "planner", "agent": "agentic"},
	}
	goal, err := resolveActiveChain(cfg, true)
	if err != nil {
		t.Fatalf("goal mode: %v", err)
	}
	if goal.useCase != config.UseCasePlanning {
		t.Errorf("goal mode useCase = %q, want %q", goal.useCase, config.UseCasePlanning)
	}
	if !reflect.DeepEqual(goal.chain, []string{"ollama/big"}) {
		t.Errorf("goal mode chain = %v, want the planning chain [ollama/big]", goal.chain)
	}

	exec, err := resolveActiveChain(cfg, false)
	if err != nil {
		t.Fatalf("execution mode: %v", err)
	}
	if exec.useCase != agentUseCase {
		t.Errorf("execution useCase = %q, want %q", exec.useCase, agentUseCase)
	}
	if !reflect.DeepEqual(exec.chain, []string{"ollama/fast"}) {
		t.Errorf("execution chain = %v, want the agent chain [ollama/fast]", exec.chain)
	}
}

func TestResolveAgentChain_CarriesAgentUseCase(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"agentic": {Name: "fast", Provider: "ollama"}},
		Defaults: map[string]string{"agent": "agentic"},
	}
	plan, err := resolveAgentChain(cfg)
	if err != nil {
		t.Fatalf("resolveAgentChain: %v", err)
	}
	if plan.useCase != agentUseCase {
		t.Errorf("useCase = %q, want %q", plan.useCase, agentUseCase)
	}
	// Recommendation mode carries it too, for the same reason planning does.
	rec, err := resolveAgentChain(nil)
	if err != nil {
		t.Fatalf("resolveAgentChain(nil): %v", err)
	}
	if rec.useCase != agentUseCase {
		t.Errorf("nil-config useCase = %q, want %q", rec.useCase, agentUseCase)
	}
}
