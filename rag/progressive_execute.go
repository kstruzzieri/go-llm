package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// This file is the ORCHESTRATION layer for the progressive renderer: both
// public entry points, the three store reads in their contracted order,
// assembly, and trace filling. Text rendering lives in
// progressive_render.go, budget arithmetic in progressive_alloc.go, the
// capability projection in progressive_groups.go, and the declaration
// surface in progressive.go (DEV-15).

// RenderProgressive renders already-selected retrieval results at mixed
// depths under hard token and byte budgets (spec sections 9-10). It never
// re-ranks, never prunes the candidate set, and never injects stale summary
// text. BuildContext is unchanged; this is the opt-in progressive path.
//
// Security: rendered source paths and managed document titles are UNTRUSTED
// data that reaches the model. Render format v2 quotes both values at
// structural positions, so embedded newlines, labels, and delimiters remain
// data and cannot forge orientation or evidence headers.
func (r *Retriever) RenderProgressive(ctx context.Context, req ProgressiveRenderRequest) (string, ProgressiveTrace, error) {
	out, trace, _, err := r.renderProgressive(ctx, req, false)
	return out, trace, err
}

// RenderProgressiveWithGroups is RenderProgressive plus the domain-owned
// groups projection, built from the SAME prepared-source snapshot BEFORE
// allocation — the tool-local budget never prunes the capability declaration
// (#331 spec 3.1). Output and trace are value-identical to RenderProgressive
// for the same request, on every path including the degraded one below.
//
// A blank Chunk.Source on ANY result yields NO groups for the whole call:
// SubjectRef.ID must identify a source, so such a result cannot be projected,
// and a partial projection would lose its blocks under mixed assembly. The
// render itself is unaffected — blank-sourced results render exactly as the
// legacy entry point has always rendered them.
//
// Groups are returned in source order (ascending ProgressiveGroup.Desc.Rank),
// one per distinct source, lining up positionally with the trace's Sources.
//
// COST CEILING: unlike the returned output, the groups are NOT bounded by
// MaxTokens/MaxBytes. Every evidence prefix is materialized unconditionally,
// so content memory is Theta(rungs * n^2/2 * average block bytes) in the
// number of results for one source — quadratic where the flat path is capped.
// Callers that pass a model-supplied result count must clamp it themselves;
// this entry point applies no upper bound today.
func (r *Retriever) RenderProgressiveWithGroups(ctx context.Context, req ProgressiveRenderRequest) (string, ProgressiveTrace, []ProgressiveGroup, error) {
	return r.renderProgressive(ctx, req, true)
}

func (r *Retriever) renderProgressive(ctx context.Context, req ProgressiveRenderRequest, wantGroups bool) (string, ProgressiveTrace, []ProgressiveGroup, error) {
	trace := ProgressiveTrace{
		MaxTokens: req.MaxTokens, MaxBytes: req.MaxBytes, MaxDepth: req.MaxDepth,
		SelectedResults:     len(req.Results),
		RenderFormatVersion: ProgressiveRenderFormatVersion,
	}
	// Every error return below yields the ZERO trace, never the partially
	// filled one. A half-filled trace is indistinguishable from a completed
	// render: "MaxBytes:1 SelectedResults:1 DistinctSources:1 used:0 free:0"
	// reads as a render that found one source, spent nothing, and has no
	// budget left, and EstimatedTokensFree of zero is byte-identical to
	// genuine exhaustion. Nothing is lost: a caller that wants MaxTokens
	// after an error still holds the request it passed in.
	//
	// This buys a contract a caller can actually apply: used + free ==
	// MaxTokens holds whenever the trace is non-zero.
	if err := validateProgressiveRequest(req); err != nil {
		return "", ProgressiveTrace{}, nil, err
	}
	// A blank Chunk.Source has no subject id, so it cannot be projected. That
	// degrades the PROJECTION, never the render: failing the call would break
	// every caller of the WithGroups entry point, including the ones that only
	// wanted the output (agent/tools.Retrieve builds groups unconditionally when
	// Progressive is set, because it cannot see whether mixed assembly is on).
	//
	// All-or-nothing per call, deliberately. Projecting only the sourced results
	// would declare a partial capability, and under mixed assembly the anchor's
	// flat content is REPLACED by the selected alternatives — so the blank-sourced
	// blocks would silently vanish from the model's view. Returning no groups
	// leaves the anchor a legacy one carrying the complete flat rendering.
	if wantGroups {
		for _, res := range req.Results {
			if res.Chunk.Source == "" {
				wantGroups = false
				break
			}
		}
	}
	if len(req.Results) == 0 {
		// fillProgressiveTrace never runs on this path, so restate the
		// invariant it maintains. Leaving free at zero would tell a caller the
		// budget is exhausted when nothing was charged against it.
		trace.EstimatedTokensFree = req.MaxTokens
		return "", trace, nil, nil
	}

	sources, err := r.prepareProgressiveSources(ctx, req)
	if err != nil {
		return "", ProgressiveTrace{}, nil, err
	}
	trace.DistinctSources = len(sources)

	// BEFORE allocate, deliberately: allocate mutates per-source allocation
	// state, and the groups projection must declare what this source COULD
	// contribute, not what this call's budget happened to admit (#331 spec
	// 3.1). Moving this below the allocate call would make the retrieve tool's
	// local ceiling the permanent upper bound on every downstream allocator.
	var groups []ProgressiveGroup
	if wantGroups {
		groups = buildProgressiveGroups(sources)
	}

	// allocate sorts sources by firstIndex in place (DEV-16); assembly and the
	// trace both consume that normalized order, not construction order.
	st, err := allocate(sources, req, req.Estimate)
	if err != nil {
		return "", ProgressiveTrace{}, nil, err
	}

	out, trimmed, err := assembleProgressive(sources, req.MaxBytes)
	if err != nil {
		return "", ProgressiveTrace{}, nil, err
	}
	fillProgressiveTrace(&trace, sources, st, out, trimmed, req.Estimate)
	return out, trace, groups, nil
}

// prepareProgressiveSources groups results by source (source order = index of
// each source's first result) and loads store-side state. READ-ORDER CONTRACT
// (spec section 8): provenance and summaries FIRST, content digests LAST, no
// parallelization — the digest comparison is the ground truth that catches a
// reindex racing the earlier reads. Reversing the order reopens the race.
//
// The digest read must also cover EVERY retrieved chunk, not a budget-limited
// subset: chunkIDs is collected here, before allocation has decided anything,
// precisely so that a chunk dropped on cost still contributes its race
// detector to its source's reason set.
func (r *Retriever) prepareProgressiveSources(ctx context.Context, req ProgressiveRenderRequest) ([]*progressiveSource, error) {
	var sources []*progressiveSource
	bySource := map[string]*progressiveSource{}
	var sourceNames []string
	var chunkIDs []string
	for i, res := range req.Results {
		src, ok := bySource[res.Chunk.Source]
		if !ok {
			src = &progressiveSource{
				source:     res.Chunk.Source,
				firstIndex: i,
				decisions:  map[string]bool{},
			}
			bySource[res.Chunk.Source] = src
			sources = append(sources, src)
			sourceNames = append(sourceNames, res.Chunk.Source)
		}
		src.results = append(src.results, res)
		src.resultIdx = append(src.resultIdx, i)
		if res.Chunk.ID != "" {
			// A blank ID cannot be looked up, so it is left out of the query
			// rather than sent as one. The comparison below still fails closed
			// for it: no stored digest means evidence_mismatch.
			chunkIDs = append(chunkIDs, res.Chunk.ID)
		}
	}

	reader, readerOK := r.store.(progressiveStoreReader)
	provenance := map[string]SourceProvenance{}
	summaries := map[string]SourceSummary{}
	digests := map[string]string{}
	if readerOK {
		var err error
		provenance, err = reader.SourceProvenanceBatch(ctx, sourceNames)
		if err != nil {
			return nil, err
		}
		summaries, err = reader.SourceSummaryBatch(ctx, sourceNames)
		if err != nil {
			return nil, err
		}
		digests, err = reader.ChunkContentDigestBatch(ctx, chunkIDs) // LAST, per contract above
		if err != nil {
			return nil, err
		}
	}

	for _, src := range sources {
		prov, provFound := provenance[src.source]
		src.prov, src.provFound = prov, provFound && readerOK

		evidenceOK := readerOK
		if readerOK {
			for _, res := range src.results {
				stored, ok := digests[res.Chunk.ID]
				if !ok || stored != sha256Hex(res.Chunk.Content) {
					evidenceOK = false
					break
				}
			}
		} else {
			evidenceOK = true // no store to compare against; degradation is via unknowns
		}

		var row *SourceSummary
		if s, ok := summaries[src.source]; ok {
			row = &s
		}
		src.reasons = deriveSummaryValidity(row, src.prov, src.provFound, evidenceOK)
		src.summary = row
		src.fresh = row != nil && len(src.reasons) == 0

		// Decisions derive from reason MEMBERSHIP, not summary presence: the
		// spec's emission table (section 10) allows summary_missing and
		// summary_stale to co-occur (e.g. a custom store yields missing plus
		// unknown_* reasons). They live here, beside the reasons they are
		// computed from, rather than in fillProgressiveTrace — which the name
		// promises only reads.
		//
		// budget_demoted is deliberately NOT derived anywhere in this file:
		// the allocator sets it, guarded on "a cheaper alternative actually
		// rendered" (DEV-17). Deriving it a second time from costRejected
		// re-opens the way to emitting it beside no_fit on a source that
		// rendered nothing.
		for _, reason := range src.reasons {
			if reason == ValidityReasonMissing {
				src.decisions[DecisionSummaryMissing] = true
			} else {
				src.decisions[DecisionSummaryStale] = true
			}
		}
		if !evidenceOK {
			// Metadata must describe the retrieval snapshot the evidence came
			// from, not the current index (spec section 8). The orientation
			// builder already reads only the in-hand chunks; this records the
			// fact for the trace.
			src.snapshotMeta = true
		}
	}
	return sources, nil
}

// assembleProgressive emits sources in source order: one orientation block,
// then that source's admitted evidence blocks in retrieval order. Sources are
// separated by exactly one "\n" with none trailing the last — that separator
// was budgeted at admission time (separatorBytes/separatorTokens), so output
// stays within MaxBytes and the allocator's byte total equals len(out)
// exactly. Blocks within one source concatenate with no separator because
// orientationText and evidenceText each already end in one newline.
//
// The drop loop is the defensive whole-block trim required by spec section
// 11: with correct admission it never fires, but if it does, it drops whole
// trailing blocks (evidence first, then the source's orientation) so
// truncation can never land inside a multi-byte rune and no partially
// rendered block is ever attributed. It exists so the runtime capOutput can
// never be the thing that truncates.
//
// Trim contract (#331 spec 3.6): every output-derived trace field is
// recomputed from the surviving blocks in fillProgressiveTrace, so a trace
// always describes the returned output. TrimmedBlocks counts the drops.
// Dropping PINNED evidence is an error, never silent: admission already
// errored if pins alone exceeded a ceiling, so reaching a pinned block here
// means the accounting invariant is broken.
func assembleProgressive(sources []*progressiveSource, maxBytes int) (string, int, error) {
	trimmed := 0
	for {
		var parts []string
		for _, src := range sources {
			if src.orientation == orientationNone {
				continue
			}
			var b strings.Builder
			b.WriteString(orientationText(src, src.orientation))
			for _, idx := range src.evidence {
				b.WriteString(evidenceText(src.results[idx]))
			}
			parts = append(parts, b.String())
		}
		out := strings.Join(parts, "\n")
		if len(out) <= maxBytes {
			return out, trimmed, nil
		}
		dropped, err := dropLastBlock(sources)
		if err != nil {
			return "", trimmed, err
		}
		if !dropped {
			// Defensive-unreachable: for dropLastBlock to find nothing, every
			// source is orientationNone, so out is "" and MaxBytes > 0 would
			// have returned above.
			return "", trimmed, nil
		}
		trimmed++
	}
}

// dropLastBlock removes the last rendered block in assembly order: the last
// evidence block of the last rendered source, or that source's orientation
// (omitting the source) when it has no evidence left. Returns false when no
// rendered block remains, and an error when the block to drop is pinned.
func dropLastBlock(sources []*progressiveSource) (bool, error) {
	for i := len(sources) - 1; i >= 0; i-- {
		src := sources[i]
		if src.orientation == orientationNone {
			continue
		}
		if n := len(src.evidence); n > 0 {
			idx := src.evidence[n-1]
			if containsInt(src.pinnedEvidence, idx) {
				return false, fmt.Errorf(
					"rag: progressive render: defensive trim would drop pinned evidence (source %q, chunk %q); admission accounting invariant broken",
					src.source, src.results[idx].Chunk.ID)
			}
			src.evidence = src.evidence[:n-1]
			return true, nil
		}
		src.orientation = orientationNone
		src.decisions[DecisionNoFit] = true
		return true, nil
	}
	return false, nil
}

// fillProgressiveTrace populates the whole-render and per-source telemetry
// FROM THE SURVIVING BLOCKS (#331 spec 3.6): after a defensive trim, every
// output-derived field still describes exactly the returned output.
// Request/candidate facts (MaxTokens, MaxBytes, MaxDepth, SelectedResults,
// DistinctSources, FloorRequested, UnmatchedPins) pass through unchanged,
// and NonFittingBlocks keeps its admission-time meaning. estimate is the
// caller's estimator (nil-safe via safeEstimate) so token counts agree with
// the allocator's accounting; when no trim fired the recompute below equals
// st.tokensUsed exactly (charges are per-block sums of the same texts).
func fillProgressiveTrace(trace *ProgressiveTrace, sources []*progressiveSource, st *allocState, out string, trimmed int, estimate func(string) int) {
	trace.TrimmedBlocks = trimmed
	trace.OutputTruncated = trimmed > 0
	trace.BytesUsed = len(out)
	trace.NonFittingBlocks = st.nonFitting
	trace.FloorRequested = st.floorRequested
	trace.UnmatchedPins = st.unmatchedPins

	tokensUsed, renderedSources, omitted, floorRendered := 0, 0, 0, 0
	for _, src := range sources {
		depth := DepthNone
		switch {
		case src.orientation == orientationNone:
			// Omitted first, mirroring assembleProgressive: the assembler
			// skips an orientationNone source wholesale, leftover evidence
			// indices included, so the source is DepthNone regardless of
			// what the evidence slice still holds.
		case len(src.evidence) > 0:
			depth = DepthL2
		case src.orientation == orientationL0L1:
			depth = DepthL1
		default:
			depth = DepthL0
		}
		switch depth {
		case DepthL2:
			trace.SourcesWithEvidence++
			trace.EvidenceBlocks += len(src.evidence)
		case DepthL1:
			trace.SourcesAtL1++
		case DepthL0:
			trace.SourcesAtL0++
		case DepthNone:
			omitted++
		}
		if depth != DepthNone {
			renderedSources++
		}
		for _, idx := range src.evidence {
			if containsInt(src.floorEvidence, idx) {
				floorRendered++
			}
		}

		decisions := make([]string, 0, len(src.decisions))
		for d := range src.decisions {
			decisions = append(decisions, d)
		}
		sort.Strings(decisions)

		var rendered []RenderedEvidence
		for _, idx := range src.evidence {
			res := src.results[idx]
			rendered = append(rendered, RenderedEvidence{
				Source: res.Chunk.Source, ChunkID: res.Chunk.ID, StableKey: res.Chunk.StableKey,
				StartLine: res.Chunk.StartLine, EndLine: res.Chunk.EndLine, Score: res.Score,
			})
		}

		srcTrace := ProgressiveSourceTrace{
			Source:   src.source,
			Managed:  src.prov.Managed,
			BestRank: src.firstIndex + 1,
			// results are in retrieval order within a source, so [0] is the
			// first-ranked one — the same result firstIndex points at.
			BestScore:      src.results[0].Score,
			ScoreKind:      "semantic_similarity",
			Omitted:        depth == DepthNone,
			EffectiveDepth: depth,
			// What was RENDERED, not whether a row existed: a fresh source that
			// fell back to the metadata overview on cost (DEV-18) has a summary
			// and still renders none of its text, so orientationMeta must report
			// false. Meaningful iff !Omitted (spec 3.3).
			OrientationGenerated: src.orientation >= orientationL0,
			MetadataFromSnapshot: src.snapshotMeta,
			ValidityReasons:      src.reasons,
			Decisions:            decisions,
			RenderedEvidence:     rendered,
		}
		if src.orientation != orientationNone {
			text := orientationText(src, src.orientation)
			srcTrace.EstimatedTokens = safeEstimate(estimate, text)
			for _, idx := range src.evidence {
				srcTrace.EstimatedTokens += safeEstimate(estimate, evidenceText(src.results[idx]))
			}
		}
		tokensUsed += srcTrace.EstimatedTokens
		trace.Sources = append(trace.Sources, srcTrace)
	}
	if renderedSources > 1 {
		tokensUsed += (renderedSources - 1) * safeEstimate(estimate, "\n")
	}
	trace.EstimatedTokensUsed = tokensUsed
	trace.OmittedSources = omitted
	trace.FloorRendered = floorRendered
	trace.EstimatedTokensFree = trace.MaxTokens - tokensUsed
	if trace.EstimatedTokensFree < 0 {
		// Belt-and-braces: admission keeps the charge within MaxTokens, so
		// only an estimator violating the Estimate contract (stateful or
		// nondeterministic) can push the recompute past the ceiling.
		trace.EstimatedTokensFree = 0
	}
}
