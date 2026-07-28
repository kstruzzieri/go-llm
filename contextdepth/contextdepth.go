// Package contextdepth expresses the rendered fidelity of one subject of
// model context, independent of domain (RAG sources, conversation spans,
// agent-memory records). It is a leaf package: it imports nothing from this
// module and holds no renderers, allocators, or I/O. Domain packages own
// what a depth MEANS for their data; this package only names the ladder and
// the payload-free descriptors shared across them (#331 / #189 slice 3).
package contextdepth

// Depth is the highest fidelity of one complete rendered alternative. The
// zero value is invalid: omission is explicit state on traces, never a depth,
// and new APIs must reject DepthInvalid on input. (rag's legacy
// "MaxDepth == 0 means unrestricted" survives only at the rag boundary.)
type Depth uint8

const (
	DepthInvalid Depth = iota // zero value: no valid fidelity; never renderable
	DepthL0                   // orientation
	DepthL1                   // compact, content-bearing representation
	DepthL2                   // verbatim underlying evidence
)

// Valid reports whether d is a renderable fidelity (L0..L2).
func (d Depth) Valid() bool { return d >= DepthL0 && d <= DepthL2 }

// String returns "invalid", "L0", "L1", or "L2"; out-of-range values are
// "invalid" rather than a formatted number so traces never carry a depth
// String that no constant produces.
func (d Depth) String() string {
	switch d {
	case DepthL0:
		return "L0"
	case DepthL1:
		return "L1"
	case DepthL2:
		return "L2"
	default:
		return "invalid"
	}
}

// RepresentationKind says what a rendered component IS, orthogonally to how
// deep it goes. Depth alone cannot carry citation or freshness rules: a
// metadata overview and a generated abstract both sit at L0, but only the
// generated one asserts a producer read the source. Citation rules key on
// RepresentationVerbatim; freshness applies only to RepresentationGenerated.
type RepresentationKind uint8

const (
	RepresentationInvalid   RepresentationKind = iota
	RepresentationMetadata                     // deterministic, derived from data in hand
	RepresentationGenerated                    // model-generated summary text
	RepresentationCompact                      // stored compact record (memory content, durable summary)
	RepresentationVerbatim                     // verbatim underlying evidence
)

// Valid reports whether k is a real representation kind.
func (k RepresentationKind) Valid() bool {
	return k >= RepresentationMetadata && k <= RepresentationVerbatim
}

// String returns the lowercase kind name; out-of-range values are "invalid".
func (k RepresentationKind) String() string {
	switch k {
	case RepresentationMetadata:
		return "metadata"
	case RepresentationGenerated:
		return "generated"
	case RepresentationCompact:
		return "compact"
	case RepresentationVerbatim:
		return "verbatim"
	default:
		return "invalid"
	}
}

// SubjectRef identifies one subject of context across domains. Domain is one
// of the Domain* constants; ID is domain-owned and unique within
// (Domain, one assembly call).
type SubjectRef struct {
	Domain string
	ID     string
}

// Domain vocabulary for SubjectRef.Domain.
const (
	DomainRAG          = "rag"
	DomainConversation = "conversation"
	DomainMemory       = "memory"
)

// RepresentationDesc identifies one component of a complete alternative.
// Pairing a kind with its own depth is what keeps A3b (L0 orientation + L2
// evidence) and A3c (L1 orientation + L2 evidence) distinct descriptors.
type RepresentationDesc struct {
	Depth Depth
	Kind  RepresentationKind
}

// AlternativeDesc describes one complete alternative without its payload.
// An alternative is atomic — an allocator admits it whole or not at all.
// Representations is non-empty and in declaration/render order. Token cost
// is deliberately absent: producers cannot know the final provider envelope
// or estimator, so the consumer measures the materialized alternative at
// admission time.
type AlternativeDesc struct {
	Representations []RepresentationDesc
}

// Depth returns the highest component depth, or DepthInvalid when the
// descriptor is malformed (empty, or any component invalid).
func (a AlternativeDesc) Depth() Depth {
	if !a.Valid() {
		return DepthInvalid
	}
	max := DepthInvalid
	for _, r := range a.Representations {
		if r.Depth > max {
			max = r.Depth
		}
	}
	return max
}

// Valid reports whether every component carries a valid depth and kind and
// at least one component exists.
func (a AlternativeDesc) Valid() bool {
	if len(a.Representations) == 0 {
		return false
	}
	for _, r := range a.Representations {
		if !r.Depth.Valid() || !r.Kind.Valid() {
			return false
		}
	}
	return true
}

// GroupDesc describes one subject's candidacy for assembly.
type GroupDesc struct {
	Subject SubjectRef
	Lane    int // consumer-assigned priority lane; smaller = earlier (spec D7)
	Rank    int // domain-owned rank for trace/debugging; not cross-domain comparable
}
