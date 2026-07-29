package agent

import (
	"fmt"
	"slices"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// ContextAlternative is one content fragment admitted whole or not at all
// (#331 spec 3.2): a partially emitted alternative would attribute evidence
// that is not in the output. Attrib is set only on evidence-bearing
// alternatives; orientation/compact alternatives never inherit evidence
// attribution. The rule is deliberately ONE-directional: Attrib on an
// alternative with no verbatim component is rejected, but a verbatim
// alternative with nil Attrib is legal — memory-domain records carry verbatim
// content and no attribution, because RetrievedSource is RAG-shaped.
type ContextAlternative struct {
	Desc    contextdepth.AlternativeDesc
	Content string
	Attrib  *RetrievalAttribution
}

// ContextGroup is one tool-result subject plus its complete legal
// alternatives in declaration (cheapest-first) order; measured-cost ties
// preserve declaration order. A consumer picks AT MOST ONE alternative per
// group — they are mutually exclusive renderings of the same subject.
type ContextGroup struct {
	Desc         contextdepth.GroupDesc
	Alternatives []ContextAlternative
}

// ContextSet is the structured payload of one tool result. In mixed mode the
// groups are the authoritative model-visible alternatives and replace, never
// duplicate, the fallback ToolResult.Content; with Mixed off the set is
// ignored everywhere.
type ContextSet struct {
	Groups      []ContextGroup
	MinVerbatim int // preferred count of verbatim components; 0 = no floor
}

// Carrier bounds: ContextSet is a public field settable by untrusted tools.
// These cap the work and trace size of mixed assembly (groups x alternatives
// candidates, each estimated and traced), which is where they are enforced.
// They do NOT bound dispatch's clone: that runs unconditionally and before
// validation, and Representations per alternative is unbounded — clone-time
// cost stays bounded only by what the tool already allocated (#331 spec 3.2).
// Built-in producers sit far below both limits.
const (
	maxContextGroups       = 256
	maxContextAlternatives = 64
)

// clone deep-copies the set so tool-owned mutation after dispatch can never
// change State (spec 3.2). String bytes are shared because strings are
// immutable; every slice and the nested attribution are copied.
func (s *ContextSet) clone() *ContextSet {
	if s == nil {
		return nil
	}
	out := &ContextSet{MinVerbatim: s.MinVerbatim, Groups: make([]ContextGroup, len(s.Groups))}
	for i, g := range s.Groups {
		cg := ContextGroup{Desc: g.Desc, Alternatives: make([]ContextAlternative, len(g.Alternatives))}
		for j, a := range g.Alternatives {
			// Copy the struct, then replace only its slice: a composite literal
			// naming just the slice field would silently drop any field a later
			// slice adds to AlternativeDesc or RetrievalAttribution.
			ca := ContextAlternative{Desc: a.Desc, Content: a.Content}
			ca.Desc.Representations = slices.Clone(a.Desc.Representations)
			if a.Attrib != nil {
				at := *a.Attrib
				at.Sources = slices.Clone(a.Attrib.Sources)
				ca.Attrib = &at
			}
			cg.Alternatives[j] = ca
		}
		out.Groups[i] = cg
	}
	return out
}

// validateContextSet enforces the mixed-assembly boundary (#331 spec 4.3).
// ToolResult.Context is a public field, so a hand-built set is validated
// here; the built-in projections cannot produce a malformed one. Never skip,
// never fabricate: a malformed set is an assembly error, indexed and
// domain-qualified so the offending group is identifiable.
func validateContextSet(callID string, s *ContextSet) error {
	// Total by construction: legacy anchors carry no set, and a caller should
	// not have to guard. Nil is absence, which is legal everywhere; the
	// "non-nil set with zero groups" rule below is untouched by this.
	if s == nil {
		return nil
	}
	if s.MinVerbatim < 0 {
		return fmt.Errorf("agent: context set (call %q): MinVerbatim must be >= 0, got %d", callID, s.MinVerbatim)
	}
	if len(s.Groups) == 0 {
		return fmt.Errorf("agent: context set (call %q): non-nil set with zero groups", callID)
	}
	if len(s.Groups) > maxContextGroups {
		return fmt.Errorf("agent: context set (call %q): %d groups exceeds limit %d", callID, len(s.Groups), maxContextGroups)
	}
	// One ContextSet belongs to one tool result, and chain bijection gives one
	// tool result per call ID, so the spec's (ToolCallID, Domain, ID) triple is
	// fully determined here. Duplicates ACROSS calls are legal — the same
	// {rag, source} subject from two retrieve calls is expected.
	seen := make(map[contextdepth.SubjectRef]int, len(s.Groups))
	for i, g := range s.Groups {
		switch g.Desc.Subject.Domain {
		case contextdepth.DomainRAG, contextdepth.DomainMemory:
		case contextdepth.DomainConversation:
			return fmt.Errorf("agent: context set (call %q): group %d: domain %q is assembler-owned", callID, i, g.Desc.Subject.Domain)
		default:
			return fmt.Errorf("agent: context set (call %q): group %d: unknown domain %q", callID, i, g.Desc.Subject.Domain)
		}
		if g.Desc.Subject.ID == "" {
			return fmt.Errorf("agent: context set (call %q): group %d: blank subject ID", callID, i)
		}
		if prev, dup := seen[g.Desc.Subject]; dup {
			return fmt.Errorf("agent: context set (call %q): group %d: duplicate subject %s/%q (also group %d)", callID, i, g.Desc.Subject.Domain, g.Desc.Subject.ID, prev)
		}
		seen[g.Desc.Subject] = i
		if len(g.Alternatives) == 0 {
			return fmt.Errorf("agent: context set (call %q): group %d (%q): no alternatives", callID, i, g.Desc.Subject.ID)
		}
		if len(g.Alternatives) > maxContextAlternatives {
			return fmt.Errorf("agent: context set (call %q): group %d (%q): %d alternatives exceeds limit %d", callID, i, g.Desc.Subject.ID, len(g.Alternatives), maxContextAlternatives)
		}
		for j, a := range g.Alternatives {
			if !a.Desc.Valid() {
				return fmt.Errorf("agent: context set (call %q): group %d (%q): alternative %d: invalid descriptor", callID, i, g.Desc.Subject.ID, j)
			}
			if a.Content == "" {
				return fmt.Errorf("agent: context set (call %q): group %d (%q): alternative %d: empty content", callID, i, g.Desc.Subject.ID, j)
			}
			if a.Attrib != nil && verbatimComponents(a.Desc) == 0 {
				return fmt.Errorf("agent: context set (call %q): group %d (%q): alternative %d: attribution on a non-verbatim alternative", callID, i, g.Desc.Subject.ID, j)
			}
		}
	}
	return nil
}

// verbatimComponents counts RepresentationVerbatim components; the
// MinVerbatim floor is satisfied in these units.
func verbatimComponents(d contextdepth.AlternativeDesc) int {
	n := 0
	for _, r := range d.Representations {
		if r.Kind == contextdepth.RepresentationVerbatim {
			n++
		}
	}
	return n
}
