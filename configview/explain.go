// configview/explain.go
//
// The offline host readers (#736): explain a model's capabilities, or a
// provider's listing, from exactly one evidence-store read and the effective
// config the host passes, with no other I/O. Hosts such as Firn call them
// without sending traffic the user did not ask for.
package configview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/evidencesel"
	"github.com/kstruzzieri/go-llm/provider"
)

// listingCredentialUnresolved is ListingExplanation.Unavailable when the
// provider's key still holds an unexpanded ${...} template.
const listingCredentialUnresolved = "credential_unresolved"

// AccessFact is the newest access claim for a model under the configured
// credential.
type AccessFact struct {
	State  string    `json:"state"` // answered | refused
	At     time.Time `json:"at,omitzero"`
	Status int       `json:"status,omitempty"` // 400-499 only
}

// CapabilityExplanation explains every canonical capability of one model: one
// fact per capability, in canonical order, and the credential's access.
type CapabilityExplanation struct {
	Key          provider.ModelKey         `json:"key"` // untagged inside, as InventoryModel.Key
	Capabilities []provider.CapabilityFact `json:"capabilities"`
	Access       *AccessFact               `json:"access,omitempty"`
}

// Masks reduces the explanation to the capabilities it says yes to, no to, and
// neither (provider.CapabilityMasks).
func (e CapabilityExplanation) Masks() (yes, no, unknown provider.Capability) {
	return provider.CapabilityMasks(e.Capabilities)
}

// ListedModel is one member of a provider's listing.
type ListedModel struct {
	Model   string `json:"model"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ListingExplanation is a provider's current listing for the configured
// credential, and the models the user named for its endpoint. Unavailable is
// "credential_unresolved" when the key was never expanded, so no listing can be
// read for it.
type ListingExplanation struct {
	ListedAt    time.Time     `json:"listed_at,omitzero"`
	HasListing  bool          `json:"has_listing"`
	Models      []ListedModel `json:"models,omitempty"`
	Named       []string      `json:"named,omitempty"`
	Unavailable string        `json:"unavailable,omitempty"` // credential_unresolved
}

// ExplainCapabilities explains every canonical capability of key offline. cfg
// must be the effective config the host's registry was built from (its URL
// overrides applied), so the binding is the one evidence is written under. It
// makes at most one store.ReadSnapshot call, for both Ollama spellings of the
// model and the configured credential exactly as given, and no other I/O. Only
// rows for those spellings at the binding's endpoint count. The digest is that
// of the listing member named exactly key.Model, else of the member under the
// other Ollama spelling, else "" (name-only). Access (answered or refused only)
// and legacy history are the configured spelling's only. A credential still
// holding an unexpanded ${...} has no listing and no access. A nil store still
// explains the explicit list, the catalog (a nil catalog has none) and the
// type. An empty model, an unknown provider, a provider with no binding, an
// invalid capability policy and a failed read are errors; a failed read is
// provider.ErrEvidenceUnreadable, never the store's text. A context done at
// entry, after the read or before returning wins over every other error, and
// is returned as is.
//
// Each call reads the account's whole current listing to find one digest; a
// host explaining many models should reuse results while the store's
// Generation is unchanged.
func ExplainCapabilities(ctx context.Context, cfg config.Config, store fingerprint.EvidenceReader, catalog *provider.Catalog, key provider.ModelKey) (CapabilityExplanation, error) {
	if err := ctx.Err(); err != nil {
		return CapabilityExplanation{}, err
	}
	if key.Model == "" {
		return CapabilityExplanation{}, errors.New("configview: explain capabilities: empty model")
	}
	pc, ok := cfg.Providers[key.Provider]
	if !ok {
		return CapabilityExplanation{}, fmt.Errorf("configview: explain capabilities: unknown provider %q", key.Provider)
	}
	binding, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		return CapabilityExplanation{}, fmt.Errorf("configview: explain capabilities: provider %q: %w", key.Provider, err)
	}
	now := time.Now()
	names := provider.EvidenceModelNames(binding.Adapter, key.Model)
	var snap fingerprint.EvidenceSnapshot
	if store != nil {
		apiKey := pc.APIKey
		var readErr error
		snap, readErr = store.ReadSnapshot(ctx, binding.Endpoint, binding.Adapter, &apiKey, key.Provider, names, now)
		if err := ctx.Err(); err != nil {
			return CapabilityExplanation{}, err
		}
		if readErr != nil {
			return CapabilityExplanation{}, fmt.Errorf("configview: %w", provider.ErrEvidenceUnreadable)
		}
	}
	policy, err := cfg.CapabilityPolicy(key.Provider, key.Model)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return CapabilityExplanation{}, cerr
		}
		return CapabilityExplanation{}, fmt.Errorf("configview: explain capabilities: %w", err)
	}
	// An unexpanded key reads no account data; the store already refuses it,
	// and the reader does not trust a store that answers anyway.
	unresolved := strings.Contains(pc.APIKey, "${")
	digest, rows := evidencesel.Select(snap, binding, key.Model, !unresolved)
	out := CapabilityExplanation{
		Key: key,
		Capabilities: provider.ExplainEvidence(provider.EvidenceInputs{
			Adapter: binding.Adapter, Digest: digest, Now: now,
			Explicit: policy.Explicit, TypeCaps: policy.TypeCaps, TypeNames: policy.TypeNames,
			Catalog: catalog.ProfileFor(key.Model, ""),
			Rows:    rows, Legacy: snap.Legacy[key.Model],
		}),
	}
	a := snap.Access[key.Model]
	if a != nil && !unresolved && (a.State == fingerprint.AccessAnswered || a.State == fingerprint.AccessRefused) {
		out.Access = &AccessFact{State: string(a.State), At: a.ObservedAt}
		if a.HTTPStatus >= 400 && a.HTTPStatus <= 499 {
			out.Access.Status = a.HTTPStatus
		}
	}
	if err := ctx.Err(); err != nil {
		return CapabilityExplanation{}, err
	}
	return out, nil
}

// ExplainListing returns providerName's current listing for the configured
// credential and the models named for its endpoint, offline. cfg must be the
// effective config, as for ExplainCapabilities. It makes at most one
// store.ReadSnapshot call, for the listing and named models only, and no other
// I/O. Members and named models keep the store's order. A credential still
// holding an unexpanded ${...} has no listing, only Unavailable and the named
// models. A nil store has no listing and no named models. An unknown provider,
// a provider with no binding and a failed read are errors; a failed read is
// provider.ErrEvidenceUnreadable, never the store's text. A context done at
// entry, after the read or before returning wins over every other error, and
// is returned as is.
//
// Each call reads the account's whole current listing; a host rendering it
// repeatedly should reuse the result while the store's Generation is
// unchanged.
func ExplainListing(ctx context.Context, cfg config.Config, store fingerprint.EvidenceReader, providerName string) (ListingExplanation, error) {
	if err := ctx.Err(); err != nil {
		return ListingExplanation{}, err
	}
	pc, ok := cfg.Providers[providerName]
	if !ok {
		return ListingExplanation{}, fmt.Errorf("configview: explain listing: unknown provider %q", providerName)
	}
	binding, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		return ListingExplanation{}, fmt.Errorf("configview: explain listing: provider %q: %w", providerName, err)
	}
	now := time.Now()
	var out ListingExplanation
	unresolved := strings.Contains(pc.APIKey, "${")
	if unresolved {
		out.Unavailable = listingCredentialUnresolved
	}
	if store != nil {
		apiKey := pc.APIKey
		snap, readErr := store.ReadSnapshot(ctx, binding.Endpoint, binding.Adapter, &apiKey, "", nil, now)
		if err := ctx.Err(); err != nil {
			return ListingExplanation{}, err
		}
		if readErr != nil {
			return ListingExplanation{}, fmt.Errorf("configview: %w", provider.ErrEvidenceUnreadable)
		}
		if snap.Listing != nil && !unresolved {
			out.HasListing, out.ListedAt = true, snap.Listing.CommittedAt
			for _, m := range snap.Listing.Members {
				out.Models = append(out.Models, ListedModel{Model: m.Model, OwnedBy: m.OwnedBy})
			}
		}
		for _, n := range snap.Named {
			out.Named = append(out.Named, n.Model)
		}
	}
	if err := ctx.Err(); err != nil {
		return ListingExplanation{}, err
	}
	return out, nil
}
