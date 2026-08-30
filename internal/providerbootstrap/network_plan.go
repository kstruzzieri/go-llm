package providerbootstrap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// PlannedRoute is one enabled route in a mode's frozen network plan: the
// RoutingRequest.UseCase the runtime will bind, plus either the exact strict
// selector chain or the recommend marker. Exactly one of Chain/Recommend is
// meaningful; buildNetworkPlan rejects a route carrying neither or both
// halves empty.
//
// One purpose may be fed by several routes — golem's dispatch tool binds
// UseCase "agent" with a chain that can differ from the agent chain — and
// the plan unions their reachability under that purpose.
type PlannedRoute struct {
	UseCase   string
	Chain     []string // strict provider/model selectors, nil when Recommend
	Recommend bool     // non-strict: reaches every configured provider
}

// planOpts carries the mode toggles that add metadata reachability.
type planOpts struct {
	// CapabilityProbes adds a capability-probe edge per active provider —
	// set when the mode may run live tool-capability probes (cap store
	// present and probing not disabled).
	CapabilityProbes bool
}

// NetworkPlan is the frozen reachability of one process mode, resolved once
// before any I/O. Routes carries the exact chains later wiring consumes —
// nothing after the plan may call RoleFallbackChain independently, or the
// consumed chain and the admitted chain could diverge. ActiveProviders is
// the sorted set reachable from any route; only these may be refreshed or
// probed (I7). Edges is the reachability graph the destination manifest is
// built from.
type NetworkPlan struct {
	Routes          []PlannedRoute
	ActiveProviders []string
	Edges           []provider.DestinationEdge
}

// planAgentRoute mirrors cmd/golem's resolveAgentChain exactly: the LITERAL
// defaults["agent"] check (not RoleForUseCase), absent or nil config means
// recommend, present-but-unresolvable is fatal.
func planAgentRoute(cfg *config.Config) (PlannedRoute, error) {
	if cfg == nil {
		return PlannedRoute{UseCase: "agent", Recommend: true}, nil
	}
	if _, ok := cfg.Defaults["agent"]; !ok {
		return PlannedRoute{UseCase: "agent", Recommend: true}, nil
	}
	chain, err := cfg.RoleFallbackChain("agent")
	if err != nil {
		return PlannedRoute{}, fmt.Errorf("providerbootstrap: resolve agent chain: %w", err)
	}
	return PlannedRoute{UseCase: "agent", Chain: chain}, nil
}

// planOptionalUseCaseRoute resolves an optional side-task use case the way
// the runtime consumes it: a resolvable use case is a strict chain, and an
// absent one is a RECOMMEND route — not "disabled" — because the runtime
// caller (agent.NewRouterSummarizer with an empty chain) routes non-strict
// across every configured provider. Modeling absence as no-route would hide
// exactly the reachability #477 exists to surface.
func planOptionalUseCaseRoute(cfg *config.Config, useCase string) (PlannedRoute, error) {
	if cfg == nil {
		return PlannedRoute{UseCase: useCase, Recommend: true}, nil
	}
	if _, ok := cfg.RoleForUseCase(useCase); !ok {
		return PlannedRoute{UseCase: useCase, Recommend: true}, nil
	}
	chain, err := cfg.RoleFallbackChain(useCase)
	if err != nil {
		return PlannedRoute{}, fmt.Errorf("providerbootstrap: resolve %s chain: %w", useCase, err)
	}
	return PlannedRoute{UseCase: useCase, Chain: chain}, nil
}

// planRoleRoute resolves a role-pinned route (delegate, dispatch-role,
// embedding): the role's chain served under the given use case. Mirrors the
// existing flag semantics — a named role that does not resolve is fatal, and
// an empty chain is fatal, because an explicitly requested feature must not
// silently no-op.
func planRoleRoute(cfg *config.Config, role, useCase string) (PlannedRoute, error) {
	if cfg == nil {
		return PlannedRoute{}, fmt.Errorf("providerbootstrap: role %q requires a config; none found", role)
	}
	chain, err := cfg.RoleChain(role)
	if err != nil {
		return PlannedRoute{}, fmt.Errorf("providerbootstrap: role %q: %w", role, err)
	}
	if len(chain) == 0 {
		return PlannedRoute{}, fmt.Errorf("providerbootstrap: role %q resolved to an empty chain", role)
	}
	return PlannedRoute{UseCase: useCase, Chain: chain}, nil
}

// selectorProvider extracts the provider key from a "provider/model"
// selector. Chains from config resolution are always provider-qualified.
func selectorProvider(selector string) string {
	prov, _, _ := strings.Cut(selector, "/")
	return prov
}

// buildNetworkPlan freezes a mode's reachability: which providers its routes
// can reach, and the destination edges the admission manifest is built from.
// Pure — the only inputs are the materialized effective config and the
// already-resolved routes.
//
// Route edges: one per (purpose, provider), first-seen ordering inside each
// chain deciding primary vs fallback; a provider reached as a primary by ANY
// contributing chain keeps the primary marking, because reachable-as-primary
// is the stronger statement. A recommend route contributes an edge for every
// configured provider (I8) — recommendation may pick any of them, and
// admitting fewer would recreate the enumeration hole.
//
// Metadata edges are added for ACTIVE providers only (I7): model-refresh
// always, slot-probe when the provider opted into slot discovery, and
// capability-probe when the mode enables probing.
func buildNetworkPlan(eff *effectiveProviders, routes []PlannedRoute, opts planOpts) (*NetworkPlan, error) {
	type edgeKey struct{ purpose, provider string }
	edgeIdx := make(map[edgeKey]int)
	var edges []provider.DestinationEdge
	active := make(map[string]bool)

	addEdge := func(purpose, prov string, fallback bool) error {
		dest, ok := eff.dests[prov]
		if !ok {
			return fmt.Errorf("providerbootstrap: route %q reaches unknown provider %q", purpose, prov)
		}
		active[prov] = true
		key := edgeKey{purpose: purpose, provider: prov}
		if i, dup := edgeIdx[key]; dup {
			if !fallback {
				edges[i].IsFallback = false
			}
			return nil
		}
		edgeIdx[key] = len(edges)
		edges = append(edges, provider.DestinationEdge{
			Purpose:     purpose,
			Destination: dest,
			IsFallback:  fallback,
		})
		return nil
	}

	for _, r := range routes {
		if r.UseCase == "" {
			return nil, fmt.Errorf("providerbootstrap: planned route with empty use case")
		}
		switch {
		case r.Recommend:
			if len(r.Chain) != 0 {
				return nil, fmt.Errorf("providerbootstrap: route %q carries both a chain and the recommend marker", r.UseCase)
			}
			for prov := range eff.cfg.Providers {
				if err := addEdge(r.UseCase, prov, false); err != nil {
					return nil, err
				}
			}
		case len(r.Chain) > 0:
			for i, sel := range r.Chain {
				if err := addEdge(r.UseCase, selectorProvider(sel), i > 0); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("providerbootstrap: route %q has neither a chain nor the recommend marker", r.UseCase)
		}
	}

	activeSorted := make([]string, 0, len(active))
	for prov := range active {
		activeSorted = append(activeSorted, prov)
	}
	sort.Strings(activeSorted)

	for _, prov := range activeSorted {
		if err := addEdge(provider.DestinationPurposeModelRefresh, prov, false); err != nil {
			return nil, err
		}
		if eff.cfg.Providers[prov].SlotDiscovery {
			if err := addEdge(provider.DestinationPurposeSlotProbe, prov, false); err != nil {
				return nil, err
			}
		}
		if opts.CapabilityProbes {
			if err := addEdge(provider.DestinationPurposeCapabilityProbe, prov, false); err != nil {
				return nil, err
			}
		}
	}

	return &NetworkPlan{
		Routes:          routes,
		ActiveProviders: activeSorted,
		Edges:           edges,
	}, nil
}
