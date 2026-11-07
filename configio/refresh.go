package configio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

type refreshOptions struct {
	store fingerprint.EvidenceWriter
	cfg   *config.Config
}

// RefreshOption configures RefreshInventory.
type RefreshOption func(*refreshOptions)

// WithPublish makes a successful refresh publish each listing, its members and
// the facts the provider reported to store, in one transaction after the last
// provider returns. cfg supplies each provider's endpoint, api format and key;
// a provider with no entry in cfg, an invalid base URL or an unresolved ${...}
// key is listed as usual and not published. Cancellation publishes nothing.
// With WithPublish, models may be nil: the result then carries the listed
// identities and the provider-reported facts, without registry projection.
func WithPublish(store fingerprint.EvidenceWriter, cfg *config.Config) RefreshOption {
	return func(o *refreshOptions) { o.store, o.cfg = store, cfg }
}

// listingReason classifies a failed listing as one of the bounded reasons; an
// error that is none of them is unreachable, so a failure always has a reason. It reads status codes and error types, never text.
func listingReason(err error) string {
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		switch code := status.HTTPStatusCode(); {
		case code == 404 || code == 405:
			return configview.ListingUnsupported
		case code == 401 || code == 403:
			return configview.ListingAuthFailed
		case code == 429:
			return configview.ListingRateLimited
		case code >= 400 && code < 500:
			return configview.ListingRejected
		case code >= 500:
			return configview.ListingServerError
		case code >= 300:
			return configview.ListingMalformedResponse // not a listing: a redirect or other non-2xx
		}
	}
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	var netErr net.Error
	switch {
	case errors.As(err, &syntax), errors.As(err, &typed),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return configview.ListingMalformedResponse
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return configview.ListingTimeout
	case errors.As(err, &netErr):
		return configview.ListingUnreachable
	}
	return configview.ListingUnreachable
}

// publishTarget is where one provider's listing is published.
type publishTarget struct {
	binding provider.EvidenceBinding
	apiKey  string
}

// targetFor resolves a provider's publish target from cfg, and false when it
// cannot be published: no cfg, no entry, an invalid base URL or an unresolved key.
func (o *refreshOptions) targetFor(name string) (publishTarget, bool) {
	if o.cfg == nil {
		return publishTarget{}, false
	}
	pc := o.cfg.Provider(name)
	if pc == nil || strings.Contains(pc.APIKey, "${") {
		return publishTarget{}, false
	}
	binding, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		return publishTarget{}, false
	}
	return publishTarget{binding: binding, apiKey: pc.APIKey}, true
}

// listingPublish builds the publish for one provider's hygiene-filtered listing.
func listingPublish(t publishTarget, started time.Time, infos []provider.ModelInfo) fingerprint.ListingPublish {
	pub := fingerprint.ListingPublish{Endpoint: t.binding.Endpoint, APIKey: t.apiKey, StartedAt: started}
	for _, info := range infos {
		pub.Members = append(pub.Members, fingerprint.ListingMember{Model: info.Name, OwnedBy: info.OwnedBy, Digest: info.Digest})
		if t.binding.Adapter != fingerprint.AdapterOllama {
			continue
		}
		facts := provider.OllamaListingFacts(info)
		names := make([]string, 0, len(facts))
		for name := range facts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			row := fingerprint.EvidenceRow{
				KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: t.binding.Endpoint, Adapter: t.binding.Adapter,
				Model: info.Name, Digest: info.Digest, Capability: name,
				Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: facts[name],
				Method: fingerprint.MethodOllamaShow, MethodVersion: fingerprint.MethodVersion(fingerprint.MethodOllamaShow),
				ObservedAt: started,
			}
			row.FreshUntil, row.ExpiresAt = fingerprint.ExpiryFor(row, 0)
			pub.Facts = append(pub.Facts, row)
		}
	}
	return pub
}

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
//
// With WithPublish, each successful listing is also published to the evidence
// store after the final barrier (see WithPublish); a failed listing publishes
// nothing and reports a bounded Reason. If the store fails while the context is
// live, the populated Inventory is returned together with a non-nil wrapped
// error, so a host can show the models and say they were not saved. If the
// context is cancelled, including during publish, the zero Inventory and the
// context error are returned.
func RefreshInventory(ctx context.Context, providers ProviderLister, models ListedProjector, opts ...RefreshOption) (configview.Inventory, error) {
	var o refreshOptions
	for _, opt := range opts {
		opt(&o)
	}
	var pubs []fingerprint.ListingPublish
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
		started := time.Now()
		infos, err := p.Models(ctx)
		if err != nil {
			inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: false, Reason: listingReason(err)})
			continue
		}
		if infos == nil && o.store != nil {
			// A 200 with no listing in it is not an empty listing: publishing it
			// would wipe the previous snapshot.
			inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: false, Reason: configview.ListingMalformedResponse})
			continue
		}
		infos = append([]provider.ModelInfo(nil), infos...)
		sort.SliceStable(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
		// Listing hygiene: a misbehaving server can return empty or
		// duplicate model ids. Empty names are unaddressable (the probe
		// op rejects them as invalid_argument) and duplicates would
		// diverge between Inventory.Models and configview's by-name maps
		// (last-wins there). Drop empties, keep the first of each name —
		// with the stable sort, that is the first occurrence in provider
		// order, a deterministic function of the listing. Names containing
		// NUL — the store-key separator, never a legitimate id — are
		// likewise dropped.
		kept := infos[:0]
		var prev string
		for _, info := range infos {
			if info.Name == "" || strings.ContainsRune(info.Name, 0) || (len(kept) > 0 && info.Name == prev) {
				continue
			}
			kept = append(kept, info)
			prev = info.Name
		}
		infos = kept
		target, publishable := publishTarget{}, false
		if o.store != nil {
			target, publishable = o.targetFor(name)
		}
		var facts []provider.ListedModelFacts
		if models == nil {
			// ponytail: no registry, so no catalog or config layering; the
			// listing's own facts are all there is.
			for _, info := range infos {
				f := provider.ListedModelFacts{
					Key: provider.ModelKey{Provider: name, Model: info.Name}, Family: info.Family, ContextWindow: info.ContextWindow,
				}
				if publishable && target.binding.Adapter == fingerprint.AdapterOllama {
					f.Caps, f.KnownMask = provider.ListingFactMasks(provider.OllamaListingFacts(info))
				}
				facts = append(facts, f)
			}
		} else {
			facts, err = models.ProjectListedModels(ctx, name, infos)
		}
		if publishable {
			pubs = append(pubs, listingPublish(target, started, infos))
		}
		if err != nil {
			// The projection is total except cancellation; its error IS
			// the abort signal and never marks the provider unreachable.
			return configview.Inventory{}, err
		}
		inv.Providers = append(inv.Providers, configview.InventoryProvider{Name: name, Reachable: true})
		for _, f := range facts {
			inv.Models = append(inv.Models, configview.InventoryModel{
				Key:               f.Key,
				Family:            f.Family,
				Caps:              f.Caps,
				KnownMask:         f.KnownMask,
				ProfileSource:     f.ProfileSource,
				ContextWindow:     f.ContextWindow,
				QualityCtxCeiling: f.QualityCtxCeiling,
			})
		}
	}
	// Final barrier: a cancelled operation publishes nothing, even when
	// cancellation lands after the last provider's work completed.
	if err := ctx.Err(); err != nil {
		return configview.Inventory{}, err
	}
	if o.store != nil && len(pubs) > 0 {
		if err := o.store.PublishListings(ctx, pubs); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return configview.Inventory{}, cerr
			}
			return inv, fmt.Errorf("configio: publish listings: %w", err)
		}
	}
	return inv, nil
}
