package rag

import (
	"context"
	"sort"
	"strings"
)

// This file is the ORCHESTRATION layer for the progressive renderer: the
// public entry point, the three store reads in their contracted order,
// assembly, and trace filling. Text rendering lives in
// progressive_render.go, budget arithmetic in progressive_alloc.go, and the
// declaration surface in progressive.go (DEV-15).

// RenderProgressive renders already-selected retrieval results at mixed
// depths under hard token and byte budgets (spec sections 9-10). It never
// re-ranks, never prunes the candidate set, and never injects stale summary
// text. BuildContext is unchanged; this is the opt-in progressive path.
func (r *Retriever) RenderProgressive(ctx context.Context, req ProgressiveRenderRequest) (string, ProgressiveTrace, error) {
	trace := ProgressiveTrace{
		MaxTokens: req.MaxTokens, MaxBytes: req.MaxBytes, MaxDepth: req.MaxDepth,
		SelectedResults: len(req.Results),
	}
	if err := validateProgressiveRequest(req); err != nil {
		return "", trace, err
	}
	if len(req.Results) == 0 {
		// fillProgressiveTrace never runs on this path, so restate the
		// invariant it maintains: used + free == MaxTokens. Leaving free at
		// zero would tell a caller the budget is exhausted when nothing was
		// charged against it.
		trace.EstimatedTokensFree = req.MaxTokens
		return "", trace, nil
	}

	sources, err := r.prepareProgressiveSources(ctx, req)
	if err != nil {
		return "", trace, err
	}
	trace.DistinctSources = len(sources)

	// allocate sorts sources by firstIndex in place (DEV-16); assembly and the
	// trace both consume that normalized order, not construction order.
	st, err := allocate(sources, req, req.Estimate)
	if err != nil {
		return "", trace, err
	}

	out, truncated := assembleProgressive(sources, req.MaxBytes)
	trace.OutputTruncated = truncated
	fillProgressiveTrace(&trace, sources, st, len(out), req.Estimate)
	return out, trace, nil
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
// The drop loop is the defensive whole-block trim required by spec section 11:
// with correct admission it never fires, but if it does, it drops whole
// trailing blocks (evidence first, then the source's orientation) so
// truncation can never land inside a multi-byte rune and no partially
// rendered block is ever attributed. It exists so the runtime capOutput can
// never be the thing that truncates.
//
// It mutates each source's rendered state (evidence, orientation, decisions),
// which is what keeps attribution and EffectiveDepth describing exactly what
// was emitted. It does NOT rewind allocState: on this path
// EstimatedTokensUsed and OmittedSources over- and under-report respectively.
// That is tolerable only because the path is unreachable under correct
// admission — see TestProgressiveByteAccountingMatchesAssembly, which pins the
// equality the unreachability rests on.
func assembleProgressive(sources []*progressiveSource, maxBytes int) (string, bool) {
	truncated := false
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
			return out, truncated
		}
		truncated = true
		if !dropLastBlock(sources) {
			return "", truncated // nothing left to drop
		}
	}
}

// dropLastBlock removes the last rendered block in assembly order: the last
// evidence block of the last rendered source, or that source's orientation
// (omitting the source) when it has no evidence left. Returns false when no
// rendered block remains.
func dropLastBlock(sources []*progressiveSource) bool {
	for i := len(sources) - 1; i >= 0; i-- {
		src := sources[i]
		if src.orientation == orientationNone {
			continue
		}
		if n := len(src.evidence); n > 0 {
			src.evidence = src.evidence[:n-1]
			return true
		}
		src.orientation = orientationNone
		src.decisions[DecisionNoFit] = true
		return true
	}
	return false
}

// fillProgressiveTrace populates the whole-render and per-source telemetry.
// estimate is the caller's estimator (nil-safe via safeEstimate) so per-source
// token counts agree with the allocator's accounting.
func fillProgressiveTrace(trace *ProgressiveTrace, sources []*progressiveSource, st *allocState, outBytes int, estimate func(string) int) {
	trace.EstimatedTokensUsed = st.tokensUsed
	// Free is the REMAINDER of the ceiling, not a second copy of used. admit
	// keeps tokensUsed <= MaxTokens, so the clamp below is belt-and-braces
	// against a future path that charges without admitting.
	trace.EstimatedTokensFree = trace.MaxTokens - st.tokensUsed
	if trace.EstimatedTokensFree < 0 {
		trace.EstimatedTokensFree = 0
	}
	trace.BytesUsed = outBytes
	trace.NonFittingBlocks = st.nonFitting
	trace.OmittedSources = st.omitted
	trace.FloorRequested = st.floorRequested
	trace.FloorRendered = st.floorRendered
	trace.UnmatchedPins = st.unmatchedPins

	for _, src := range sources {
		depth := DepthNone
		switch {
		case len(src.evidence) > 0:
			depth = DepthL2
		case src.orientation == orientationL0L1:
			depth = DepthL1
		case src.orientation != orientationNone:
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
			// Counted by the allocator as st.omitted, not here.
		}

		// Decisions derive from reason MEMBERSHIP, not summary presence: the
		// spec's emission table (section 10) allows summary_missing and
		// summary_stale to co-occur (e.g. a custom store yields missing plus
		// unknown_* reasons).
		//
		// budget_demoted is deliberately NOT derived here: the allocator sets
		// it, guarded on "a cheaper alternative actually rendered" (DEV-17).
		// Deriving it a second time from costRejected re-opens the way to
		// emitting it beside no_fit on a source that rendered nothing.
		for _, reason := range src.reasons {
			if reason == ReasonMissing {
				src.decisions[DecisionSummaryMissing] = true
			} else {
				src.decisions[DecisionSummaryStale] = true
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
			EffectiveDepth: depth,
			// What was RENDERED, not whether a row existed: a fresh source that
			// fell back to the metadata overview on cost (DEV-18) has a summary
			// and still renders none of its text, so orientationMeta must report
			// false.
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
		trace.Sources = append(trace.Sources, srcTrace)
	}
}
