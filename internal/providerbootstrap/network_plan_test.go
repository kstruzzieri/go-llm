package providerbootstrap

import (
	"errors"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// planFixtureConfig mirrors the live config's shape: one local provider, one
// remote, an agent role whose fallbacks cross onto the remote, a remote-only
// analysis role (the summarize fallback target), and a local-only embedding
// role.
func planFixtureConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8090"},
			"opencode": {APIFormat: "openai-compat", BaseURL: "https://opencode.ai/zen/go"},
			"unused":   {APIFormat: "openai-compat", BaseURL: "https://unused.example.com"},
		},
		Models: map[string]config.ModelConfig{
			"fast":        {Provider: "llamacpp", Name: "qwen-fast", Type: "dense", Fallbacks: []string{"cloud-coder"}},
			"coding":      {Provider: "llamacpp", Name: "qwen-coder", Type: "dense"},
			"embedding":   {Provider: "llamacpp", Name: "qwen-embed", Type: "embedding"},
			"cloud-pro":   {Provider: "opencode", Name: "deepseek-pro", Type: "moe", Fallbacks: []string{"cloud-flash"}},
			"cloud-flash": {Provider: "opencode", Name: "deepseek-flash", Type: "moe"},
			"cloud-coder": {Provider: "opencode", Name: "kimi-code", Type: "moe"},
		},
		Defaults: map[string]string{
			"agent":     "fast",
			"embedding": "embedding",
			"analysis":  "cloud-pro",
		},
	}
}

func mustEff(t *testing.T, cfg *config.Config) *Effective {
	t.Helper()
	eff, err := Materialize(cfg, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func edgeSet(t *testing.T, plan *NetworkPlan) map[string]provider.DestinationEdge {
	t.Helper()
	out := make(map[string]provider.DestinationEdge, len(plan.Edges))
	for _, e := range plan.Edges {
		out[e.Purpose+"|"+e.Destination.Provider()] = e
	}
	return out
}

// I5, the enumeration test the acceptance criteria require: every provider
// reachable from every planned route has a manifest edge under that route's
// purpose — proven by walking the plan's OWN routes, never a fixed use-case
// list (M5: config.SideTaskUseCases() omits agent, embedding, dispatch,
// delegate, and planning, so an enumerator built on it cannot be evidence).
func TestNetworkPlanEveryPlannedRouteHasEdges(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())

	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	summarize, err := PlanOptionalUseCaseRoute(eff.cfg, config.UseCaseSummarize)
	if err != nil {
		t.Fatal(err)
	}
	embedding, err := PlanRoleRoute(eff.cfg, "embedding", "embedding")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := PlannedRoute{UseCase: "agent", Chain: slices.Clone(agent.Chain)}
	delegate, err := PlanRoleRoute(eff.cfg, "coding", "coding")
	if err != nil {
		t.Fatal(err)
	}
	planning := PlannedRoute{UseCase: "planning", Chain: []string{"opencode/deepseek-pro"}}

	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent, summarize, embedding, dispatch, delegate, planning}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	edges := edgeSet(t, plan)
	if len(plan.Routes) != 6 {
		t.Fatalf("plan carries %d routes, want all 6", len(plan.Routes))
	}
	for _, r := range plan.Routes {
		if r.Recommend {
			for prov := range eff.cfg.Providers {
				if _, ok := edges[r.UseCase+"|"+prov]; !ok {
					t.Errorf("recommend route %q missing edge for provider %q", r.UseCase, prov)
				}
			}
			continue
		}
		for _, sel := range r.Chain {
			prov := selectorProvider(sel)
			if _, ok := edges[r.UseCase+"|"+prov]; !ok {
				t.Errorf("route %q selector %q has no manifest edge", r.UseCase, sel)
			}
		}
	}
	// The edges must assemble into a real manifest.
	if _, err := provider.NewDestinationManifest(plan.Edges...); err != nil {
		t.Fatalf("plan edges do not form a manifest: %v", err)
	}
}

// I3/M3: a local primary with remote fallbacks yields a REMOTE edge marked
// as fallback — reachability, not just the primary, is what gets admitted.
func TestNetworkPlanRemoteFallbackProducesRemoteEdge(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	edges := edgeSet(t, plan)
	remote, ok := edges["agent|opencode"]
	if !ok {
		t.Fatal("agent chain's remote fallback produced no edge")
	}
	if !remote.IsFallback {
		t.Error("remote reachable only as fallback must be marked IsFallback")
	}
	if remote.Destination.IsLocal() {
		t.Error("opencode edge classified local")
	}
	local, ok := edges["agent|llamacpp"]
	if !ok {
		t.Fatal("agent primary produced no edge")
	}
	if local.IsFallback {
		t.Error("primary provider's edge marked as fallback")
	}
}

// I7/M7: a configured provider reachable from NO planned route is inactive:
// absent from ActiveProviders and from every edge, so nothing later may
// refresh, probe, or prompt for it.
func TestNetworkPlanUnusedProviderIsInactive(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(plan.ActiveProviders, "unused") {
		t.Error("provider reachable from no route listed active")
	}
	for _, e := range plan.Edges {
		if e.Destination.Provider() == "unused" {
			t.Errorf("inactive provider has edge %q", e.Purpose)
		}
	}
	// And the active ones are exactly the reachable ones, sorted.
	want := []string{"llamacpp", "opencode"}
	if !slices.Equal(plan.ActiveProviders, want) {
		t.Errorf("ActiveProviders = %v, want %v", plan.ActiveProviders, want)
	}
}

// I8/M8: any recommend route makes EVERY configured provider reachable —
// recommendation may pick any of them, so all must be admitted or denied
// before model enumeration, never silently narrowed to defaults.agent's
// provider.
func TestNetworkPlanRecommendActivatesEveryProvider(t *testing.T) {
	cfg := planFixtureConfig()
	delete(cfg.Defaults, "agent") // recommend mode
	eff := mustEff(t, cfg)

	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !agent.Recommend {
		t.Fatal("agent route without defaults.agent must be a recommend route")
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"llamacpp", "opencode", "unused"}
	if !slices.Equal(plan.ActiveProviders, want) {
		t.Errorf("ActiveProviders = %v, want every configured provider %v", plan.ActiveProviders, want)
	}
}

// F4/current behavior: a config where the summarize use case resolves to
// nothing yields a RECOMMEND summarize route — the runtime summarizer routes
// non-strict with an empty chain, which reaches every configured provider.
func TestNetworkPlanEmptySummarizeIsRecommend(t *testing.T) {
	cfg := planFixtureConfig()
	delete(cfg.Defaults, "analysis") // summarize falls through analysis -> chat -> nothing
	eff := mustEff(t, cfg)

	summarize, err := PlanOptionalUseCaseRoute(eff.cfg, config.UseCaseSummarize)
	if err != nil {
		t.Fatal(err)
	}
	if !summarize.Recommend {
		t.Fatal("unresolvable summarize must be a recommend route, mirroring the non-strict runtime summarizer")
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{summarize}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ActiveProviders) != len(eff.cfg.Providers) {
		t.Errorf("recommend summarize activates %d providers, want all %d",
			len(plan.ActiveProviders), len(eff.cfg.Providers))
	}
}

// A summarize use case that RESOLVES stays strict — the fixture's analysis
// fallback routes it to the remote provider (the original #477 bug path,
// now visible as edges instead of silent egress).
func TestNetworkPlanSummarizeViaAnalysisFallbackIsStrictRemote(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	summarize, err := PlanOptionalUseCaseRoute(eff.cfg, config.UseCaseSummarize)
	if err != nil {
		t.Fatal(err)
	}
	if summarize.Recommend {
		t.Fatal("summarize resolving via the analysis fallback must be strict")
	}
	if summarize.SuppliedByUseCase != "analysis" {
		t.Fatalf("summarize role source = %q, want analysis", summarize.SuppliedByUseCase)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{summarize}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edges := edgeSet(t, plan)
	e, ok := edges["summarize|opencode"]
	if !ok {
		t.Fatal("summarize-via-analysis produced no opencode edge")
	}
	if e.Destination.IsLocal() {
		t.Error("summarize edge classified local")
	}
}

// Disabled features contribute nothing: without the embedding route, the
// embedding-only provider stays out of a plan that only runs the delegate.
func TestNetworkPlanDisabledFeatureAddsNoEdges(t *testing.T) {
	cfg := planFixtureConfig()
	// Give embedding its own provider so its absence is observable.
	cfg.Providers["embedhost"] = config.ProviderConfig{APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:9091"}
	m := cfg.Models["embedding"]
	m.Provider = "embedhost"
	cfg.Models["embedding"] = m
	eff := mustEff(t, cfg)

	delegate, err := PlanRoleRoute(eff.cfg, "coding", "coding")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{delegate}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.ActiveProviders, "embedhost") {
		t.Error("embedding provider active though no embedding route was planned")
	}
}

// Metadata edges: model-refresh for every ACTIVE provider, slot-probe only
// for active providers opted into slot discovery, capability-probe only when
// the mode enables it — and none of them for inactive providers.
func TestNetworkPlanMetadataEdges(t *testing.T) {
	cfg := planFixtureConfig()
	pc := cfg.Providers["llamacpp"]
	pc.SlotDiscovery = true
	cfg.Providers["llamacpp"] = pc
	eff := mustEff(t, cfg)

	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("with capability probes", func(t *testing.T) {
		plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{CapabilityProbes: true})
		if err != nil {
			t.Fatal(err)
		}
		edges := edgeSet(t, plan)
		for _, prov := range []string{"llamacpp", "opencode"} {
			if _, ok := edges[provider.DestinationPurposeModelRefresh+"|"+prov]; !ok {
				t.Errorf("active provider %q missing model-refresh edge", prov)
			}
			if _, ok := edges[provider.DestinationPurposeCapabilityProbe+"|"+prov]; !ok {
				t.Errorf("active provider %q missing capability-probe edge", prov)
			}
		}
		if _, ok := edges[provider.DestinationPurposeSlotProbe+"|llamacpp"]; !ok {
			t.Error("slot-discovery provider missing slot-probe edge")
		}
		if _, ok := edges[provider.DestinationPurposeSlotProbe+"|opencode"]; ok {
			t.Error("slot-probe edge for a provider not opted into slot discovery")
		}
		if _, ok := edges[provider.DestinationPurposeModelRefresh+"|unused"]; ok {
			t.Error("model-refresh edge for an inactive provider")
		}
	})

	t.Run("without capability probes", func(t *testing.T) {
		plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range plan.Edges {
			if e.Purpose == provider.DestinationPurposeCapabilityProbe {
				t.Fatal("capability-probe edge present though probing is disabled")
			}
		}
	})
}

// One purpose fed by two chains (agent + a dispatch role) unions its edges;
// selectors repeating a provider inside one chain collapse to one edge that
// keeps the strongest (primary) marking.
func TestNetworkPlanUnionsChainsPerPurpose(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// dispatch role pins the same purpose to a different chain.
	dispatch, err := PlanRoleRoute(eff.cfg, "cloud-pro", "agent")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent, dispatch}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edges := edgeSet(t, plan)
	// agent purpose covers llamacpp (agent primary) AND opencode (agent
	// fallback + dispatch primary). The dispatch primary marking wins over
	// the fallback marking for the same edge.
	if _, ok := edges["agent|llamacpp"]; !ok {
		t.Fatal("agent chain's own provider lost in the union")
	}
	oc, ok := edges["agent|opencode"]
	if !ok {
		t.Fatal("dispatch chain's provider missing from the union")
	}
	if oc.IsFallback {
		t.Error("opencode is a dispatch PRIMARY; the union must keep the primary marking")
	}
}

// Recommend-route edges come out in sorted provider order, run after run —
// map iteration order must never reach the consent surface. Twenty builds
// make a lucky ordering from unsorted iteration vanishingly improbable.
func TestNetworkPlanRecommendEdgesAreDeterministicallyOrdered(t *testing.T) {
	cfg := planFixtureConfig()
	delete(cfg.Defaults, "agent")
	eff := mustEff(t, cfg)
	want := []string{"llamacpp", "opencode", "unused"}

	for range 20 {
		agent, err := PlanAgentRoute(eff.cfg)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range plan.Edges {
			if e.Purpose == "agent" {
				got = append(got, e.Destination.Provider())
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("recommend edge order = %v, want sorted %v", got, want)
		}
	}
}

// Frozen means frozen: mutating the caller's route slice or chain after the
// build must not change what the plan carries.
func TestNetworkPlanIsImmuneToCallerMutation(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	routes := []PlannedRoute{agent}
	plan, err := BuildNetworkPlan(eff, routes, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	routes[0].UseCase = "hijacked"
	if len(agent.Chain) > 0 {
		agent.Chain[0] = "ghost/replaced"
	}

	if plan.Routes[0].UseCase != "agent" {
		t.Error("plan route mutated through the caller's slice")
	}
	for _, sel := range plan.Routes[0].Chain {
		if sel == "ghost/replaced" {
			t.Error("plan chain mutated through the caller's slice")
		}
	}
}

// A strict selector naming an unknown provider is a wiring error, caught at
// plan construction — never a silent skip that would leave the route
// half-admitted.
func TestNetworkPlanRejectsUnknownSelectorProvider(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	bogus := PlannedRoute{UseCase: "agent", Chain: []string{"ghost/model-x"}}
	if _, err := BuildNetworkPlan(eff, []PlannedRoute{bogus}, PlanOptions{}); err == nil {
		t.Fatal("unknown selector provider accepted")
	}
}

func TestNetworkPlanRejectsEmptyAndConflictingRoutes(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	if _, err := BuildNetworkPlan(eff, []PlannedRoute{{UseCase: ""}}, PlanOptions{}); err == nil {
		t.Error("route with empty use case accepted")
	}
	if _, err := BuildNetworkPlan(eff, []PlannedRoute{{UseCase: "agent"}}, PlanOptions{}); err == nil {
		t.Error("route with neither chain nor recommend accepted")
	}
}

// PlanAgentRoute mirrors resolveAgentChain exactly: the literal
// defaults["agent"] check, not RoleForUseCase.
func TestPlanAgentRouteMirrorsResolveAgentChain(t *testing.T) {
	t.Run("nil config recommends", func(t *testing.T) {
		r, err := PlanAgentRoute(nil)
		if err != nil || !r.Recommend {
			t.Fatalf("PlanAgentRoute(nil) = %+v, %v; want recommend", r, err)
		}
	})
	t.Run("unresolvable agent default is fatal", func(t *testing.T) {
		cfg := planFixtureConfig()
		cfg.Defaults["agent"] = "no-such-role"
		if _, err := PlanAgentRoute(cfg); err == nil {
			t.Fatal("unresolvable defaults.agent must be fatal, mirroring resolveAgentChain")
		}
	})
}

func TestPlanRoleRouteErrors(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	if _, err := PlanRoleRoute(nil, "coding", "coding"); err == nil {
		t.Error("nil config accepted")
	}
	if _, err := PlanRoleRoute(eff.cfg, "no-such-role", "coding"); err == nil {
		t.Error("unknown role accepted")
	}
}

// The install path end to end: a plan whose remote edges are ungranted fails
// Install under the zero policy naming a use case; granting the remote
// destination admits the whole plan.
func TestNetworkPlanEdgesInstallAgainstGate(t *testing.T) {
	eff := mustEff(t, planFixtureConfig())
	agent, err := PlanAgentRoute(eff.cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{agent}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := provider.NewDestinationManifest(plan.Edges...)
	if err != nil {
		t.Fatal(err)
	}
	gate := provider.NewDestinationGate()

	var zero provider.DestinationPolicy
	err = gate.Install(zero, m)
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("Install with ungranted remote = %v, want ErrDestinationDenied", err)
	}

	if err := gate.Install(provider.NewDestinationPolicy(eff.dests["opencode"]), m); err != nil {
		t.Fatalf("Install with the remote granted: %v", err)
	}
}
