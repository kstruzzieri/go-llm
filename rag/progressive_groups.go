package rag

import (
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// This file is the CAPABILITY PROJECTION layer for the progressive renderer:
// it projects one prepared source into the complete set of alternatives rag
// could legally contribute, so a cross-domain allocator can choose among them.
// It renders through the same two helpers the flat path uses (orientationText,
// evidenceText) and owns no budget arithmetic of its own. Not to be confused
// with progressive.go, the DECLARATION SURFACE, which holds the request,
// trace, and constant vocabulary.

// ProgressiveGroup is one RAG source's capability declaration: every complete
// alternative the renderer could contribute for that source, payload
// included. A consumer picks AT MOST ONE alternative per group — they are
// mutually exclusive renderings of the same subject, not composable parts.
//
// Alternatives are in ascending utility order (cheapest orientation first,
// deepest evidence prefix last); the mixed-domain upgrade pass relies on that
// declaration order rather than re-deriving a ranking.
//
// A slice of groups is in SOURCE ORDER — ascending Desc.Rank, one group per
// distinct source — which lines up positionally with
// ProgressiveTrace.Sources from the same call.
type ProgressiveGroup struct {
	Desc         contextdepth.GroupDesc
	Alternatives []ProgressiveAlternative
}

// ProgressiveAlternative is one complete, ATOMIC rendering of a source: an
// allocator admits Content whole or not at all, because a partially emitted
// alternative would attribute evidence that is not in the output. Desc names
// the components without their payload; Content is the exact bytes those
// components render to; RenderedEvidence attributes the verbatim components
// in the same order, and is empty for orientation-only alternatives.
type ProgressiveAlternative struct {
	Desc             contextdepth.AlternativeDesc
	Content          string
	RenderedEvidence []RenderedEvidence
}

// The four representation descriptors the RAG ladder can offer. Depth alone
// would not separate the metadata overview from a stored abstract: both sit at
// L0, but only the generated one asserts that a producer read the source.
var (
	repMeta     = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata}
	repAbstract = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationGenerated}
	repOverview = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationGenerated}
	repEvidence = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}
)

// ladderRung pairs an internal orientation level with the descriptor
// components it renders as.
type ladderRung struct {
	level orientationLevel
	reps  []contextdepth.RepresentationDesc
}

// buildProgressiveGroups runs on the prepared snapshot BEFORE allocation. Its
// read set is source, fresh, results and firstIndex, plus every field
// orientationText and evidenceText read: prov, summary, reasons, and
// summaryBudgetOmitted. All but the last are untouched by the allocator.
//
// summaryBudgetOmitted IS allocator-written (progressive_alloc.go step 5), and
// reading it here is safe for a two-part reason that must hold together:
// the allocator sets it only on the orientationL0 path, reached only when
// cheapestOrientation returns orientationL0, i.e. only for a FRESH source;
// and orientationText reads it only at orientationMeta, a rung this builder
// offers only to NON-fresh sources. The two sets are disjoint, so the value
// this builder can observe is always the false zero value. Widening either
// side — offering fresh sources a metadata rung, or letting the allocator
// demote a non-fresh source — breaks the pre-allocate safety argument, not
// merely this comment.
//
// The evidence families cover src.results — every result in hand — never
// src.evidence, which after allocation holds only what the caller's LOCAL
// budget admitted. Building from post-allocation state would let the retrieve
// tool's 2048-token ceiling irrevocably prune alternatives a global allocator
// still had room for (#331 spec 3.1).
//
// Fresh sources get abstract rungs only. The metadata rung's note line claims
// "no summary", which is false for a fresh source, and an orientation block
// must never lie about provenance — the model uses exactly that line to judge
// what the block is worth. Stale and missing summaries get the metadata rung,
// where the note is true.
//
// Alternatives are ordered by (prefix length, rung index): declaration order
// IS utility order, which the mixed upgrade pass relies on.
func buildProgressiveGroups(sources []*progressiveSource) []ProgressiveGroup {
	groups := make([]ProgressiveGroup, 0, len(sources))
	for _, src := range sources {
		var rungs []ladderRung
		if src.fresh {
			rungs = []ladderRung{
				{orientationL0, []contextdepth.RepresentationDesc{repAbstract}},
				{orientationL0L1, []contextdepth.RepresentationDesc{repAbstract, repOverview}},
			}
		} else {
			rungs = []ladderRung{{orientationMeta, []contextdepth.RepresentationDesc{repMeta}}}
		}

		g := ProgressiveGroup{
			Desc: contextdepth.GroupDesc{
				Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: src.source},
				// Same 1-based rank ProgressiveSourceTrace.BestRank reports, so a
				// group and its trace entry can be lined up. Lane is left unset:
				// priority lanes are consumer-assigned (spec D7), not domain-owned.
				Rank: src.firstIndex + 1,
			},
			Alternatives: make([]ProgressiveAlternative, 0, (len(src.results)+1)*len(rungs)),
		}
		// Each rung's orientation block is rendered ONCE: orientationText
		// re-scans src.results and sorts the symbol/section lists, and the
		// prefix loop below would otherwise call it (n+1)*len(rungs) times for
		// the same len(rungs) distinct strings.
		orient := make([]string, len(rungs))
		for i, rung := range rungs {
			orient[i] = orientationText(src, rung.level)
			g.Alternatives = append(g.Alternatives, ProgressiveAlternative{
				// Cloned, not shared: without this every orientation-only
				// alternative of a source aliases one backing array, and a
				// consumer writing Desc.Representations[0] mutates its siblings.
				Desc:    contextdepth.AlternativeDesc{Representations: slices.Clone(rung.reps)},
				Content: orient[i],
			})
		}

		// Evidence prefixes: the first k results in retrieval order, at every
		// rung. The builder accumulates rather than re-rendering each prefix
		// from scratch, so prefix k+1 IS prefix k plus one block by
		// construction.
		var ev strings.Builder
		var rendered []RenderedEvidence
		for k := 1; k <= len(src.results); k++ {
			res := src.results[k-1]
			ev.WriteString(evidenceText(res))
			rendered = append(rendered, RenderedEvidence{
				Source: res.Chunk.Source, ChunkID: res.Chunk.ID, StableKey: res.Chunk.StableKey,
				StartLine: res.Chunk.StartLine, EndLine: res.Chunk.EndLine, Score: res.Score,
			})
			for i, rung := range rungs {
				reps := slices.Clone(rung.reps)
				for range rendered {
					reps = append(reps, repEvidence)
				}
				g.Alternatives = append(g.Alternatives, ProgressiveAlternative{
					Desc:             contextdepth.AlternativeDesc{Representations: reps},
					Content:          orient[i] + ev.String(),
					RenderedEvidence: slices.Clone(rendered),
				})
			}
		}
		groups = append(groups, g)
	}
	return groups
}
