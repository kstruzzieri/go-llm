// provider/project_listed.go
//
// ProjectListedModels: the read-only, list-fed capability projection
// (#456 slice 4). Consumed by the Firn config panel's explicit "refresh
// model list" -- it must never fire probes or re-query the provider.
package provider

import (
	"context"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ListedModelFacts is the read-only projection of one already-listed model:
// merged capability knowledge derivable WITHOUT any active probing or
// provider re-query. KnownMask covers every present Caps bit, plus
// tool_call when a persisted probe row validated against the listing
// identity answers definitively (including "no", which no cap bit can carry).
type ListedModelFacts struct {
	Key       ModelKey
	Family    string
	Caps      Capability
	KnownMask Capability
	// ProfileSource is always "merged" (the projection runs the same
	// layering as buildProfile); it exists to feed
	// configview.InventoryModel.ProfileSource, the slice-4 wire field.
	ProfileSource string
	ContextWindow int
}

// ProjectListedModels merges facts for models ALREADY LISTED by a provider
// (spec slice 4: the explicit-refresh read path). Inputs are the listing's
// own ModelInfo values; output order mirrors input order.
//
// READ-ONLY by construction: it reuses the same merge layering as
// buildProfile but (a) takes the runtime layer FROM THE SUPPLIED LISTING
// instead of re-querying the provider, (b) forces the fingerprint layer
// read-only (never EnsureProfile, so never DetectKind/ProbeChat), and
// (c) never writes the profile cache. Cap-probe rows are validated
// against capProbeDigest(key, &info) -- the digest convention the write
// side uses -- so a row whose identity cannot be re-established by THIS
// listing claims nothing (fail closed). Store read failures degrade to
// no-claim exactly like capProbeCaps: the projection is TOTAL over the
// listing; its only error is context cancellation, which returns nil
// facts (a cancelled operation publishes nothing).
//
// Divergence from buildProfile: on a registry with a fingerprint store but
// NO prober factory, buildProfile returns the stored fingerprint profile
// unconditionally, while this projection (read-only, digest-anchored)
// contributes nothing from that layer -- fingerprint enrichment here
// requires a prober factory to anchor identity, and failing closed on an
// unanchored profile is deliberate.
func (r *ModelRegistry) ProjectListedModels(ctx context.Context, providerName string, infos []ModelInfo) ([]ListedModelFacts, error) {
	// Snapshot policy hooks once, mirroring buildProfile's TOCTOU discipline.
	r.mu.RLock()
	override := r.capOverride
	floor := r.capFloor
	thinkOverride := r.thinkOverride
	contextOverride := r.contextOverride
	rejectionHook := r.rejectionHook
	r.mu.RUnlock()

	now := time.Now()
	out := make([]ListedModelFacts, 0, len(infos))
	for i := range infos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info := infos[i] // value copy; the caller's slice is never mutated
		key := ModelKey{Provider: providerName, Model: info.Name}
		parsed := ParseModelName(info.Name)
		static := r.catalogProfileFor(parsed, &info)
		fp := r.fingerprintProfileMode(ctx, key, &info, true) // read-only: never EnsureProfile

		// Cap-probe layer, validated against THIS listing's identity --
		// byte-symmetric with the write side (capProbeDigest). Missing
		// rows and store failures are no-claim (capProbeRow returns nil).
		digest := capProbeDigest(key, &info)
		var probeYes Capability
		known := Capability(0)
		if row := r.capProbeRow(ctx, key); row != nil && row.Valid(digest, now) {
			switch row.State {
			case fingerprint.CapProbeYes:
				probeYes = CapToolCall
				known |= CapToolCall
			case fingerprint.CapProbeNo:
				known |= CapToolCall
			}
		}

		profile := r.merge(key, &info, static, fp, parsed, override, floor, thinkOverride, contextOverride, rejectionHook, probeYes)
		out = append(out, ListedModelFacts{
			Key:           key,
			Family:        profile.Family,
			Caps:          profile.Caps,
			KnownMask:     profile.Caps | known,
			ProfileSource: profile.Source.String(),
			ContextWindow: profile.ContextWindow,
		})
	}
	// Final barrier: cancellation landing mid-final-iteration (or arriving
	// with an empty listing) must not return a degraded slice with nil error.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
