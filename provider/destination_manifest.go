package provider

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
)

// Purposes for repository-owned metadata traffic (#477 D5). Route traffic
// uses RoutingRequest.UseCase as its purpose; these constants name the
// non-route operations so bootstrap, discovery, and probes can bind
// capabilities without inventing ad-hoc strings at each call site.
const (
	DestinationPurposeDiscovery       = "discovery"
	DestinationPurposeHealth          = "health"
	DestinationPurposeModelRefresh    = "model-refresh"
	DestinationPurposeCapabilityProbe = "capability-probe"
	DestinationPurposeSlotProbe       = "slot-probe"
	DestinationPurposeWarmthPoll      = "warmth-poll"
)

// DestinationEdge is one reachability fact: this purpose (a routing use case
// or one of the DestinationPurpose* operations) can reach this destination.
// IsFallback is display metadata for the manifest rendering — it does not
// change authority, because a reachable fallback is exactly as reachable as
// a primary.
type DestinationEdge struct {
	Purpose     string
	Destination Destination
	IsFallback  bool
}

// destEdgeKey identifies an edge for lookup: purpose plus provider. The
// destination is not part of the key because a provider has exactly one
// base URL in an effective config — NewDestinationManifest enforces that,
// so (purpose, provider) resolves to at most one destination.
type destEdgeKey struct {
	purpose  string
	provider string
}

// DestinationManifest is the immutable reachability graph for one admission
// generation: every {purpose, destination} edge the resolved network plan can
// reach. It is a value assembled before any I/O and never mutated after
// construction — the gate swaps whole manifests, it never edits one.
type DestinationManifest struct {
	edges  map[destEdgeKey]DestinationEdge
	sorted []DestinationEdge // deterministic rendering order
}

// NewDestinationManifest validates and freezes an edge set. Exact duplicate
// edges collapse; two different destinations under one provider key are a
// construction error, because edge lookup (and the per-provider transport
// binding) would be ambiguous. Every edge needs a purpose and a constructed
// destination — an unconstructed one has no identity to admit.
func NewDestinationManifest(edges ...DestinationEdge) (*DestinationManifest, error) {
	m := &DestinationManifest{edges: make(map[destEdgeKey]DestinationEdge, len(edges))}
	byProvider := make(map[string]Destination)
	for _, e := range edges {
		if e.Purpose == "" {
			return nil, fmt.Errorf("%w: manifest edge with empty purpose", ErrDestinationInvalid)
		}
		if e.Destination.IsZero() {
			return nil, fmt.Errorf("%w: manifest edge %q with zero destination", ErrDestinationInvalid, e.Purpose)
		}
		prov := e.Destination.Provider()
		if prev, ok := byProvider[prov]; ok && prev != e.Destination {
			return nil, fmt.Errorf("%w: provider %q appears with two destinations", ErrDestinationInvalid, prov)
		}
		byProvider[prov] = e.Destination
		key := destEdgeKey{purpose: e.Purpose, provider: prov}
		if prev, dup := m.edges[key]; dup {
			// Same purpose+provider seen twice: identical edges collapse,
			// and the destination cannot differ (byProvider above). Keep a
			// primary marking over a fallback one — reachable-as-primary is
			// the stronger statement about the same edge.
			if !e.IsFallback {
				prev.IsFallback = false
				m.edges[key] = prev
			}
			continue
		}
		m.edges[key] = e
	}
	for _, e := range m.edges {
		m.sorted = append(m.sorted, e)
	}
	sort.Slice(m.sorted, func(i, j int) bool {
		if m.sorted[i].Purpose != m.sorted[j].Purpose {
			return m.sorted[i].Purpose < m.sorted[j].Purpose
		}
		return m.sorted[i].Destination.String() < m.sorted[j].Destination.String()
	})
	return m, nil
}

// Edges returns every edge in deterministic order. The slice is fresh per
// call so callers cannot mutate the manifest through it.
func (m *DestinationManifest) Edges() []DestinationEdge {
	out := make([]DestinationEdge, len(m.sorted))
	copy(out, m.sorted)
	return out
}

// Destinations returns the deduplicated destination set in deterministic
// order — the unit of consent: one prompt line (and one grant) per entry,
// however many edges reach it.
func (m *DestinationManifest) Destinations() []Destination {
	seen := make(map[Destination]struct{})
	var out []Destination
	for _, e := range m.sorted {
		if _, dup := seen[e.Destination]; !dup {
			seen[e.Destination] = struct{}{}
			out = append(out, e.Destination)
		}
	}
	return out
}

// lookup resolves (purpose, provider) to its one destination.
func (m *DestinationManifest) lookup(purpose, provider string) (Destination, bool) {
	e, ok := m.edges[destEdgeKey{purpose: purpose, provider: provider}]
	return e.Destination, ok
}

// destRevocationToken is the private identity of one REVOCATION generation.
// Pointer identity of a token is what authorization compares, so a token must
// never be forgeable or accidentally shared: it is allocated fresh per
// revoking publication and never handed outside this package. It is one byte
// rather than an empty struct because distinct zero-size allocations may
// share an address, which would make two generations indistinguishable.
type destRevocationToken byte

// destinationSnapshot is one immutable admission generation: the manifest,
// the policy that admitted every edge in it, and the revocation token that
// capabilities issued from it carry. Install validates the pair, so holding a
// snapshot IS the proof that each edge was granted — Bind never re-checks the
// policy.
//
// Install and Narrow mint a fresh token, which revokes every outstanding
// capability. Extend keeps the current token, because it only adds edges: the
// preserved ones are still exactly as admitted.
type destinationSnapshot struct {
	manifest *DestinationManifest
	policy   DestinationPolicy
	token    *destRevocationToken
}

// destinationCapability is the opaque value Bind writes into a context. It
// is unexported and keyed by an unexported type, so no other package can
// forge one — a capability exists only because the snapshot that issued it
// verified the edge. The token is the generation check: after Clear, a
// re-Install, or a Narrow the gate holds a different token, and every
// capability issued before it stops authorizing; after an additive Extend the
// token is unchanged, so those capabilities keep working.
type destinationCapability struct {
	token    *destRevocationToken
	purpose  string
	provider string
	dest     Destination
}

type destCapabilityCtxKey struct{}

// capabilityFromContext recovers the capability, or nil. Package-internal:
// the guarded transport (Task 3) is the intended reader.
func capabilityFromContext(ctx context.Context) *destinationCapability {
	cap, _ := ctx.Value(destCapabilityCtxKey{}).(*destinationCapability)
	return cap
}

// DestinationGate is the stable object every guarded transport holds. It
// starts deny-all and swaps immutable snapshots atomically: Install puts a
// validated generation in place, Narrow derives a subset, Clear revokes by
// installing nothing, and outstanding capabilities die with the revocation
// token those three replace. Extend is the one additive publication — it
// keeps the token, so adding edges never revokes. The gate is the ONE mutable
// cell in the admission design; everything it points to is immutable.
//
// A gate must not be copied after first use: it owns atomic state, and a copy
// would carry a duplicate of the current snapshot pointer rather than share
// the cell every guarded transport reads.
type DestinationGate struct {
	snap atomic.Pointer[destinationSnapshot]
}

// NewDestinationGate returns a gate in the deny-all state.
func NewDestinationGate() *DestinationGate {
	return &DestinationGate{}
}

// Install validates that policy grants every edge in manifest and atomically
// makes the pair the current generation. Install first revokes any current
// generation, so every failed install leaves the gate deny-all. On any
// ungranted edge it returns a DestinationDeniedError naming the first denied
// edge in deterministic order — the destination plus the purpose that reached
// it, which is what a user needs to fix an -allow-destination invocation.
//
// A successful Install mints a fresh revocation token, so capabilities from
// the previous generation stop authorizing even when the new manifest repeats
// their edges. Re-admitting the same edges is still a new consent decision.
func (g *DestinationGate) Install(policy DestinationPolicy, manifest *DestinationManifest) error {
	g.snap.Store(nil)
	if manifest == nil {
		return fmt.Errorf("%w: nil manifest", ErrDestinationInvalid)
	}
	for _, e := range manifest.sorted {
		if !policy.Permits(e.Destination) {
			return &DestinationDeniedError{Destination: e.Destination, Purpose: e.Purpose}
		}
	}
	g.snap.Store(&destinationSnapshot{manifest: manifest, policy: policy, token: new(destRevocationToken)})
	return nil
}

// Clear atomically revokes the current generation. The removed snapshot took
// its revocation token with it, so every outstanding capability stops
// authorizing at once; requests already inside a provider call are not
// cancelled.
func (g *DestinationGate) Clear() {
	g.snap.Store(nil)
}

// Narrow derives and installs a new generation containing only the current
// edges keep reports true for. Because it can only select from the existing
// edge set, it cannot add authority (#477 D11: discovery narrows its
// candidate envelope to the selected backend). The policy carries over
// unchanged. Narrowing an uninstalled gate is an error, not a silent deny-all.
//
// Like any revoking generation change, Narrow mints a fresh token and so
// invalidates capabilities issued by the previous snapshot — including ones
// for edges that were KEPT (unlike Extend, which preserves them). A request that
// bound before the narrow and authorizes after it denies transiently; that is
// the fail-closed direction, and in the required ordering Narrow runs in the
// quiet window between discovery and the first refresh, where no route
// traffic is in flight.
func (g *DestinationGate) Narrow(keep func(DestinationEdge) bool) error {
	cur := g.snap.Load()
	if cur == nil {
		return fmt.Errorf("%w: narrow on a gate with no installed generation", ErrDestinationInvalid)
	}
	var kept []DestinationEdge
	for _, e := range cur.manifest.sorted {
		if keep(e) {
			kept = append(kept, e)
		}
	}
	m, err := NewDestinationManifest(kept...)
	if err != nil {
		return err
	}
	// Compare-and-swap, not Store: a Clear (revocation) or re-Install that
	// landed since the load above must win. Blindly storing would overwrite
	// the newer generation with data derived from the older one — after a
	// Clear that is resurrected authority the user just revoked. On loss the
	// gate is left exactly as the concurrent writer set it.
	if !g.snap.CompareAndSwap(cur, &destinationSnapshot{manifest: m, policy: cur.policy, token: new(destRevocationToken)}) {
		return fmt.Errorf("%w: narrow lost to a concurrent generation change", ErrDestinationInvalid)
	}
	return nil
}

// Extend publishes an ADDITIVE generation: manifest must contain every
// current edge with its destination unchanged, and policy must grant every
// edge in it. Removals and retargeting are rejected, because this is the
// consent primitive for "the session also needs these destinations" (#376
// model switching), not a way to re-point an existing grant.
//
// Unlike Install and Narrow, Extend is not a revocation: it keeps the current
// generation's revocation token, so capabilities already issued for the
// preserved edges keep authorizing and an in-flight request is not killed by
// a model switch. Failed validation publishes nothing and leaves the current
// generation untouched.
func (g *DestinationGate) Extend(policy DestinationPolicy, manifest *DestinationManifest) error {
	return g.extendFrom(g.snap.Load(), policy, manifest)
}

// extendFrom validates and publishes an extension of one specific base
// generation: the snapshot it checks the superset property against is the
// snapshot it compare-and-swaps, so consent is never applied to a generation
// it was not derived from. Naming that base explicitly also lets a white-box
// test drive the load/CAS interleaving without a timing race.
func (g *DestinationGate) extendFrom(cur *destinationSnapshot, policy DestinationPolicy, manifest *DestinationManifest) error {
	if manifest == nil {
		return fmt.Errorf("%w: nil manifest", ErrDestinationInvalid)
	}
	if cur == nil {
		return fmt.Errorf("%w: extend on a gate with no installed generation", ErrDestinationInvalid)
	}
	for _, e := range cur.manifest.sorted {
		next, ok := manifest.edges[destEdgeKey{purpose: e.Purpose, provider: e.Destination.Provider()}]
		if !ok {
			return fmt.Errorf("%w: extension drops edge %q -> %s", ErrDestinationInvalid, e.Purpose, e.Destination)
		}
		if next.Destination != e.Destination {
			return fmt.Errorf("%w: extension retargets edge %q from %s to %s", ErrDestinationInvalid, e.Purpose, e.Destination, next.Destination)
		}
	}
	for _, e := range manifest.sorted {
		if !policy.Permits(e.Destination) {
			return &DestinationDeniedError{Destination: e.Destination, Purpose: e.Purpose}
		}
	}
	// Compare-and-swap for the same reason Narrow uses one: a Clear, Install,
	// Narrow, or competing Extend that landed since the load above must win.
	// Storing would overwrite it with a generation derived from stale consent
	// — after a Clear, that is resurrected authority. There is no retry: the
	// caller must re-read and re-consent.
	if !g.snap.CompareAndSwap(cur, &destinationSnapshot{manifest: manifest, policy: policy, token: cur.token}) {
		return fmt.Errorf("%w: extend lost to a concurrent generation change", ErrDestinationInvalid)
	}
	return nil
}

// Bind verifies that the current generation admits (purpose, provider) and
// returns a context carrying the capability for that exact edge. Everything
// unknown denies: no generation installed, empty purpose, or no such edge —
// the last is what stops an omitted use case from riding an endpoint that
// was admitted for a different purpose.
func (g *DestinationGate) Bind(ctx context.Context, purpose, provider string) (context.Context, error) {
	snap := g.snap.Load()
	if snap == nil {
		return nil, &DestinationDeniedError{Provider: provider, Purpose: purpose}
	}
	if purpose == "" {
		return nil, &DestinationDeniedError{Provider: provider}
	}
	dest, ok := snap.manifest.lookup(purpose, provider)
	if !ok {
		return nil, &DestinationDeniedError{Provider: provider, Purpose: purpose}
	}
	return context.WithValue(ctx, destCapabilityCtxKey{}, &destinationCapability{
		token:    snap.token,
		purpose:  purpose,
		provider: provider,
		dest:     dest,
	}), nil
}

// authorize is the transport-side check: the context must carry a capability
// carrying THIS gate's current revocation token, bound to exactly this
// provider and destination. String equality is never enough — a capability
// from another gate, or from a revoked generation, names the same strings and
// still denies, because authority is the token's, not the values'. A token is
// private to one gate's generation, so it needs no gate back-pointer; a
// capability with no token (a zero value that never came from Bind) fails
// closed here rather than matching anything.
func (g *DestinationGate) authorize(ctx context.Context, provider string, dest Destination) error {
	cap := capabilityFromContext(ctx)
	if cap == nil {
		return &DestinationDeniedError{Provider: provider, Destination: dest}
	}
	snap := g.snap.Load()
	if snap == nil || cap.token == nil || cap.token != snap.token {
		return &DestinationDeniedError{Provider: provider, Destination: dest, Purpose: cap.purpose}
	}
	if cap.provider != provider || cap.dest != dest {
		return &DestinationDeniedError{Provider: provider, Destination: dest, Purpose: cap.purpose}
	}
	return nil
}
