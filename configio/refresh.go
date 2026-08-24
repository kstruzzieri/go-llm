package configio

import (
	"context"
	"sort"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

// RefreshInventory performs the explicit provider model listing (spec read
// tier 2) and returns a NEW configview.Inventory value. It is never
// implicit: callers invoke it only on an explicit, consented user action.
//
// Semantics:
//   - Providers are visited SEQUENTIALLY in sorted name order; each
//     listing is sorted by model name before projection, so the output is
//     deterministic. Wall-clock is bounded by each provider's own client
//     timeout (models.json `timeout`) and the caller's ctx.
//   - A provider whose LISTING fails is recorded as Reachable=false and
//     the refresh proceeds — reachability is part of the value, not an
//     error, and it reports PROVIDER reachability only. A provider that
//     vanishes between Names and Get (racing unregister) is likewise
//     recorded unreachable.
//   - Per-model facts come from the ListedProjector, read-only and TOTAL
//     by contract: exactly one Models call per provider and NO other I/O
//     — no probes (fingerprint or tool-call), no re-queries. The
//     projector's only error is cancellation. Persisted probe verdicts
//     (yes AND no) surface through KnownMask, validated against this
//     listing's identity.
//   - Cancellation publishes nothing: the final barrier guarantees a
//     cancelled call returns the zero Inventory and the context error
//     regardless of where cancellation landed; the loop-entry check
//     additionally stops further provider listings early; and the
//     projection-error immediate return is a third enforcement path (the
//     projector's only error is cancellation). In-branch ctx re-checks
//     elsewhere were deliberately removed as provably redundant.
//   - Listings are hygiene-filtered: empty model names are dropped and
//     duplicate names collapse to the first (deterministic under the
//     sort above); the projection and Inventory never carry unaddressable
//     or colliding keys.
//   - Policy hooks (capability overrides/floors) are snapshotted per
//     provider by the projection, so a concurrent policy change can land
//     between providers; each provider's block is internally coherent.
func RefreshInventory(ctx context.Context, providers ProviderLister, models ListedProjector) (configview.Inventory, error) {
	inv := configview.Inventory{}
	names := append([]string(nil), providers.Names()...)
	sort.Strings(names)

	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return configview.Inventory{}, err
		}
		p, ok := providers.Get(name)
		if !ok {
			// Racing unregister between Names and Get: record as unreachable.
			inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: false})
			continue
		}
		infos, err := p.Models(ctx)
		if err != nil {
			inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: false})
			continue
		}
		infos = append([]provider.ModelInfo(nil), infos...)
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		// Listing hygiene: a misbehaving server can return empty or
		// duplicate model ids. Empty names are unaddressable (the probe
		// op rejects them as invalid_argument) and duplicates would
		// diverge between Inventory.Models and configview's by-name maps
		// (last-wins there). Drop empties, keep the first of each name —
		// deterministic under the sort above.
		kept := infos[:0]
		var prev string
		for _, info := range infos {
			if info.Name == "" || (len(kept) > 0 && info.Name == prev) {
				continue
			}
			kept = append(kept, info)
			prev = info.Name
		}
		infos = kept
		facts, err := models.ProjectListedModels(ctx, name, infos)
		if err != nil {
			// The projection is total except cancellation; its error IS
			// the abort signal and never marks the provider unreachable.
			return configview.Inventory{}, err
		}
		inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: true})
		for _, f := range facts {
			inv.Models = append(inv.Models, configview.InventoryModel{
				Key:           f.Key,
				Family:        f.Family,
				Caps:          f.Caps,
				KnownMask:     f.KnownMask,
				ProfileSource: f.ProfileSource,
				ContextWindow: f.ContextWindow,
			})
		}
	}
	// Final barrier: a cancelled operation publishes nothing, even when
	// cancellation lands after the last provider's work completed.
	if err := ctx.Err(); err != nil {
		return configview.Inventory{}, err
	}
	return inv, nil
}
