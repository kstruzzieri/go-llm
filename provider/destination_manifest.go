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

// destinationSnapshot is one immutable admission generation: the manifest
// plus the policy that admitted every edge in it. Install validates the pair,
// so holding a snapshot IS the proof that each edge was granted — Bind never
// re-checks the policy.
type destinationSnapshot struct {
	manifest *DestinationManifest
	policy   DestinationPolicy
}

// destinationCapability is the opaque value Bind writes into a context. It
// is unexported and keyed by an unexported type, so no other package can
// forge one — a capability exists only because the snapshot that issued it
// verified the edge. Pointer identity of snap is the generation check: after
// Clear or a re-Install the gate holds a different snapshot pointer, and
// every capability issued before it stops authorizing.
type destinationCapability struct {
	snap     *destinationSnapshot
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
// validated generation in place, Clear revokes by installing nothing, and
// outstanding capabilities die with the snapshot that issued them. The gate
// is the ONE mutable cell in the admission design; everything it points to
// is immutable.
type DestinationGate struct {
	snap atomic.Pointer[destinationSnapshot]
}

// NewDestinationGate returns a gate in the deny-all state.
func NewDestinationGate() *DestinationGate {
	return &DestinationGate{}
}

// Install validates that policy grants every edge in manifest and atomically
// makes the pair the current generation. On any ungranted edge it installs
// nothing and returns a DestinationDeniedError naming the first denied edge
// in deterministic order — the destination plus the purpose that reached it,
// which is what a user needs to fix an -allow-destination invocation.
func (g *DestinationGate) Install(policy DestinationPolicy, manifest *DestinationManifest) error {
	if manifest == nil {
		return fmt.Errorf("%w: nil manifest", ErrDestinationInvalid)
	}
	for _, e := range manifest.sorted {
		if !policy.Permits(e.Destination) {
			return &DestinationDeniedError{Destination: e.Destination, Purpose: e.Purpose}
		}
	}
	g.snap.Store(&destinationSnapshot{manifest: manifest, policy: policy})
	return nil
}

// Clear atomically revokes the current generation. Every outstanding
// capability was issued by the removed snapshot and stops authorizing at
// once; requests already inside a provider call are not cancelled.
func (g *DestinationGate) Clear() {
	g.snap.Store(nil)
}

// Narrow derives and installs a new generation containing only the current
// edges keep reports true for. Because it can only select from the existing
// edge set, it cannot add authority (#477 D11: discovery narrows its
// candidate envelope to the selected backend). The policy carries over
// unchanged. Narrowing an uninstalled gate is an error, not a silent deny-all.
//
// Like any generation change, Narrow invalidates capabilities issued by the
// previous snapshot — including ones for edges that were KEPT. A request that
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
	g.snap.Store(&destinationSnapshot{manifest: m, policy: cur.policy})
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
		snap:     snap,
		purpose:  purpose,
		provider: provider,
		dest:     dest,
	}), nil
}

// authorize is the transport-side check: the context must carry a capability
// issued by THIS gate's current snapshot, bound to exactly this provider and
// destination. String equality is never enough — a capability from another
// gate, or from a generation since cleared, names the same strings and still
// denies, because authority is the snapshot's, not the values'.
func (g *DestinationGate) authorize(ctx context.Context, provider string, dest Destination) error {
	cap := capabilityFromContext(ctx)
	if cap == nil {
		return &DestinationDeniedError{Provider: provider, Destination: dest}
	}
	if cap.snap != g.snap.Load() {
		return &DestinationDeniedError{Provider: provider, Destination: dest, Purpose: cap.purpose}
	}
	if cap.provider != provider || cap.dest != dest {
		return &DestinationDeniedError{Provider: provider, Destination: dest, Purpose: cap.purpose}
	}
	return nil
}
