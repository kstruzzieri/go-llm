package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// These tests pin the PURE half of /model set (#376 M1): from the process's
// frozen effective config plus the user's argument, exactly one PlannedRoute
// under use case "agent". Inventory and capability validation belong to the
// post-admission half; nothing here may touch a registry, a provider, or the
// network.

// modelSelEffective freezes cfg the way bootstrap does, with no URL
// overrides. Tests that care about overrides call Materialize themselves.
func modelSelEffective(t *testing.T, cfg *config.Config) *providerbootstrap.Effective {
	t.Helper()
	eff, err := providerbootstrap.Materialize(cfg, "", "", "")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return eff
}

// modelSelSoleProviderConfig is a vllm-only config: the sole-provider shape
// that makes a bare model id unambiguous.
func modelSelSoleProviderConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm": {BaseURL: "http://127.0.0.1:8000", APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"coder": {Name: "qwen3-coder", Provider: "vllm"},
		},
		Defaults: map[string]string{"agent": "coder"},
	}
}

// modelSelTwoProviderConfig adds a second provider, which is what makes a
// bare model id ambiguous and a qualified one necessary.
func modelSelTwoProviderConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm":   {BaseURL: "http://127.0.0.1:8000", APIFormat: "openai-compat"},
			"ollama": {BaseURL: "http://127.0.0.1:11434", APIFormat: "ollama"},
		},
		Models: map[string]config.ModelConfig{
			"coder": {Name: "qwen3-coder", Provider: "vllm"},
		},
		Defaults: map[string]string{"agent": "coder"},
	}
}

func TestResolveModelSelectionExactRoleBeatsSameNamedModel(t *testing.T) {
	// Rule 1. "coder" is a configured ROLE and also a syntactically valid
	// bare model id for the sole provider. The role wins, so the chain is
	// the role's configured model, not "vllm/coder".
	route, err := resolveModelSelection(modelSelEffective(t, modelSelSoleProviderConfig()), "coder")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	want := []string{"vllm/qwen3-coder"}
	if !reflect.DeepEqual(route.Chain, want) {
		t.Errorf("chain = %v, want %v", route.Chain, want)
	}
	if route.UseCase != "agent" {
		t.Errorf("use case = %q, want %q", route.UseCase, "agent")
	}
	if route.Recommend {
		t.Error("route marked recommend; a selected model is always a strict chain")
	}
	// The CLI plan is derived from that one route, never resolved again.
	plan := chainPlanFor(route)
	if !reflect.DeepEqual(plan.chain, want) {
		t.Errorf("chainPlan.chain = %v, want %v", plan.chain, want)
	}
	if plan.useCase != "agent" || plan.useRecommend {
		t.Errorf("chainPlan = %+v, want use case %q and no recommend", plan, "agent")
	}
}

func TestResolveModelSelectionExactRoleBeatsSameNamedQualifiedSelector(t *testing.T) {
	// Rule 1 outranks rule 2, which is only visible when a role is NAMED
	// like a qualified selector. "vllm/big" is a configured role here, so it
	// resolves to that role's own model -- reading it as provider "vllm"
	// plus model "big" would have selected a different chain entirely.
	cfg := modelSelTwoProviderConfig()
	cfg.Models["vllm/big"] = config.ModelConfig{Name: "qwen3-32b", Provider: "ollama"}
	route, err := resolveModelSelection(modelSelEffective(t, cfg), "vllm/big")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	if want := []string{"ollama/qwen3-32b"}; !reflect.DeepEqual(route.Chain, want) {
		t.Errorf("chain = %v, want %v", route.Chain, want)
	}
}

func TestResolveModelSelectionRoleNamedRecommendIsStillARole(t *testing.T) {
	// A role literally named "recommend" follows rule 1 like any other: it
	// selects its chain, it does not turn the route into the recommend
	// marker.
	cfg := modelSelSoleProviderConfig()
	cfg.Models["recommend"] = config.ModelConfig{Name: "mistral-small", Provider: "vllm"}
	route, err := resolveModelSelection(modelSelEffective(t, cfg), "recommend")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	if want := []string{"vllm/mistral-small"}; !reflect.DeepEqual(route.Chain, want) {
		t.Errorf("chain = %v, want %v", route.Chain, want)
	}
	if route.Recommend {
		t.Error(`route marked recommend; "recommend" is a role name, not the marker`)
	}
}

func TestResolveModelSelectionPreservesCompleteOrderedFallbacks(t *testing.T) {
	// Rule 1 preserves the role's ENTIRE ordered chain, transitive
	// fallbacks included. Nothing filters by availability here: a
	// temporarily unreachable entry stays in the chain, because the
	// resolver has no availability input at all.
	cfg := modelSelTwoProviderConfig()
	cfg.Models = map[string]config.ModelConfig{
		"primary": {Name: "big", Provider: "vllm", Fallbacks: []string{"mid", "backup"}},
		"mid":     {Name: "medium", Provider: "vllm", Fallbacks: []string{"backup"}},
		"backup":  {Name: "small", Provider: "ollama"},
	}
	route, err := resolveModelSelection(modelSelEffective(t, cfg), "primary")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	want := []string{"vllm/big", "vllm/medium", "ollama/small"}
	if !reflect.DeepEqual(route.Chain, want) {
		t.Errorf("chain = %v, want %v", route.Chain, want)
	}
}

func TestResolveModelSelectionQualifiedKeepsSlashedSuffix(t *testing.T) {
	// Rule 2. The FIRST slash separates the provider; the entire remaining
	// suffix is the model id, slashes and all.
	route, err := resolveModelSelection(modelSelEffective(t, modelSelTwoProviderConfig()),
		"vllm/meta-llama/Llama-3.3-70B-Instruct")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	want := []string{"vllm/meta-llama/Llama-3.3-70B-Instruct"}
	if !reflect.DeepEqual(route.Chain, want) {
		t.Errorf("chain = %v, want %v", route.Chain, want)
	}
	if route.UseCase != "agent" {
		t.Errorf("use case = %q, want %q", route.UseCase, "agent")
	}
}

func TestResolveModelSelectionCanonicalizesBeforeParseSelector(t *testing.T) {
	// The shared parser cuts at the first slash. Because the resolver hands
	// it an already-canonical "provider/rest", the slashed model id survives
	// intact -- this is what canonicalizing BEFORE shared parsing buys.
	route, err := resolveModelSelection(modelSelEffective(t, modelSelSoleProviderConfig()),
		"meta-llama/Llama-3.3-70B-Instruct")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	key, ok := parseSelector(route.Chain[0])
	if !ok {
		t.Fatalf("parseSelector(%q) reported no provider", route.Chain[0])
	}
	want := provider.ModelKey{Provider: "vllm", Model: "meta-llama/Llama-3.3-70B-Instruct"}
	if key != want {
		t.Errorf("parsed key = %+v, want %+v", key, want)
	}
}

func TestResolveModelSelectionBareIDWithSoleProvider(t *testing.T) {
	// Rule 3. With exactly one configured provider every bare id qualifies
	// against it -- including one whose first segment merely LOOKS like a
	// provider. An unrecognized prefix is part of the id, never an
	// unknown-provider error.
	eff := modelSelEffective(t, modelSelSoleProviderConfig())
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"bare name", "qwen3-coder-30b", []string{"vllm/qwen3-coder-30b"}},
		{"bare slashed id", "meta-llama/Llama-3.3-70B-Instruct", []string{"vllm/meta-llama/Llama-3.3-70B-Instruct"}},
		{"unrecognized first segment stays in the id", "openai/gpt-4o-mini", []string{"vllm/openai/gpt-4o-mini"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := resolveModelSelection(eff, tt.input)
			if err != nil {
				t.Fatalf("resolveModelSelection(%q): %v", tt.input, err)
			}
			if !reflect.DeepEqual(route.Chain, tt.want) {
				t.Errorf("chain = %v, want %v", route.Chain, tt.want)
			}
		})
	}
}

func TestResolveModelSelectionErrors(t *testing.T) {
	cyclic := modelSelSoleProviderConfig()
	cyclic.Models = map[string]config.ModelConfig{
		"loop": {Name: "a", Provider: "vllm", Fallbacks: []string{"back"}},
		"back": {Name: "b", Provider: "vllm", Fallbacks: []string{"loop"}},
	}
	cyclic.Defaults = map[string]string{}
	tests := []struct {
		name  string
		cfg   *config.Config
		input string
		want  string
	}{
		{"empty input", modelSelSoleProviderConfig(), "",
			"golem: /model set: empty selector"},
		{"whitespace only", modelSelSoleProviderConfig(), "   ",
			"golem: /model set: empty selector"},
		{"qualified with an empty suffix", modelSelSoleProviderConfig(), "vllm/",
			`golem: /model set "vllm/": empty model id after provider "vllm"; use provider/model`},
		{"qualified with a whitespace suffix", modelSelSoleProviderConfig(), "vllm/ ",
			`golem: /model set "vllm/": empty model id after provider "vllm"; use provider/model`},
		{"ambiguous bare id", modelSelTwoProviderConfig(), "meta-llama/Llama-3.3-70B-Instruct",
			`golem: /model set "meta-llama/Llama-3.3-70B-Instruct": ambiguous with 2 providers configured (ollama, vllm); use provider/model`},
		{"ambiguous bare name", modelSelTwoProviderConfig(), "qwen3-coder",
			`golem: /model set "qwen3-coder": ambiguous with 2 providers configured (ollama, vllm); use provider/model`},
		{"cyclic role", cyclic, "loop",
			`golem: /model set: providerbootstrap: role "loop": config: circular fallback at role "loop"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := resolveModelSelection(modelSelEffective(t, tt.cfg), tt.input)
			if err == nil {
				t.Fatalf("resolveModelSelection(%q) = %+v, want an error", tt.input, route)
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
			if route.Chain != nil || route.UseCase != "" || route.Recommend {
				t.Errorf("failed resolution returned %+v, want the zero route", route)
			}
		})
	}
}

func TestResolveModelSelectionNilEffective(t *testing.T) {
	// A resolver with no frozen config must say so, not panic.
	if _, err := resolveModelSelection(nil, "coder"); err == nil {
		t.Fatal("resolveModelSelection(nil) = nil error, want a failure")
	} else if want := "golem: /model set: no effective configuration"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestResolveModelSelectionPlansThroughFrozenDestinations(t *testing.T) {
	// The route is planned against the STARTUP-frozen effective config, so
	// the destinations it reaches are the overridden URLs the live clients
	// dial -- not the ones models.json names.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"vllm":   {BaseURL: "http://127.0.0.1:8000", APIFormat: "openai-compat"},
			"ollama": {BaseURL: "http://127.0.0.1:11434", APIFormat: "ollama"},
		},
		Models: map[string]config.ModelConfig{
			"primary": {Name: "big", Provider: "vllm", Fallbacks: []string{"backup"}},
			"backup":  {Name: "small", Provider: "ollama"},
		},
	}
	eff, err := providerbootstrap.Materialize(cfg, "http://127.0.0.1:19434", "vllm", "http://127.0.0.1:18000")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	route, err := resolveModelSelection(eff, "primary")
	if err != nil {
		t.Fatalf("resolveModelSelection: %v", err)
	}
	plan, err := providerbootstrap.BuildNetworkPlan(eff, []providerbootstrap.PlannedRoute{route}, providerbootstrap.PlanOptions{})
	if err != nil {
		t.Fatalf("BuildNetworkPlan: %v", err)
	}
	got := map[string]string{}
	for _, edge := range plan.Edges {
		if edge.Purpose == "agent" {
			got[edge.Destination.Provider()] = edge.Destination.BaseURL()
		}
	}
	want := map[string]string{
		"vllm":   "http://127.0.0.1:18000",
		"ollama": "http://127.0.0.1:19434",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("agent destinations = %v, want %v", got, want)
	}
}

func TestResolveModelSelectionPerformsNoProviderIO(t *testing.T) {
	// Structural first: the resolver is handed ONLY the frozen effective
	// config, so there is no registry or client to call. This pins the
	// behavioral half -- a provider whose every endpoint counts requests
	// sees none across a full sweep of selector shapes.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := modelSelSoleProviderConfig()
	cfg.Providers["vllm"] = config.ProviderConfig{BaseURL: srv.URL, APIFormat: "openai-compat"}
	eff := modelSelEffective(t, cfg)
	for _, input := range []string{"coder", "vllm/qwen3-coder", "meta-llama/Llama-3.3-70B-Instruct", "vllm/", "", "nope"} {
		_, _ = resolveModelSelection(eff, input)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("provider received %d requests during resolution, want 0", got)
	}
}
