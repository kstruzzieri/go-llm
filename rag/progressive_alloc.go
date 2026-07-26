package rag

import (
	"fmt"
	"sort"
)

// allocState is the whole-render allocation accounting.
type allocState struct {
	tokensUsed      int
	bytesUsed       int
	nonFitting      int
	omitted         int
	floorRequested  int
	floorRendered   int
	renderedSources int // orientations admitted; drives separator accounting
	unmatchedPins   []PinRef
}

// Sources are joined by one "\n" separator in assembly. Separators are part
// of the budget: every orientation after the first pays for its preceding
// separator at admission time, so assembled output can never exceed MaxBytes.
func separatorBytes(st *allocState) int {
	if st.renderedSources == 0 {
		return 0
	}
	return 1
}

func separatorTokens(st *allocState, estimate func(string) int) int {
	if st.renderedSources == 0 {
		return 0
	}
	return safeEstimate(estimate, "\n")
}

// safeEstimate wraps the caller estimator: a negative result falls back to
// defaultEstimate (rag/progressive.go) for that block. Never zero — a
// zero-cost block would bypass the hard token ceiling (spec section 9.4
// step 1). The heuristic itself lives with the Estimate field it documents,
// so the two cannot drift apart.
func safeEstimate(estimate func(string) int, s string) int {
	if estimate != nil {
		if n := estimate(s); n >= 0 {
			return n
		}
	}
	return defaultEstimate(s)
}

// cheapestOrientation is the base level for a source: stored L0 when the
// summary is fresh, deterministic metadata otherwise (spec section 9.4 step 5).
func cheapestOrientation(src *progressiveSource) orientationLevel {
	if src.fresh {
		return orientationL0
	}
	return orientationMeta
}

// admit charges a block against both budgets; returns false (and counts it
// non-fitting) without charging when either ceiling would be exceeded.
func (st *allocState) admit(req ProgressiveRenderRequest, tokens, bytes int) bool {
	if st.tokensUsed+tokens > req.MaxTokens || st.bytesUsed+bytes > req.MaxBytes {
		st.nonFitting++
		return false
	}
	st.tokensUsed += tokens
	st.bytesUsed += bytes
	return true
}

// allocate runs the spec section 9.4 algorithm over prepared sources,
// mutating their allocation state. Results inside each source must be in
// retrieval order; the sources slice itself is sorted here rather than
// trusted, and the caller's backing array is reordered in place — assembly
// wants the same order, so that is the point, not a side effect.
func allocate(sources []*progressiveSource, req ProgressiveRenderRequest, estimate func(string) int) (*allocState, error) {
	// Source order is an input invariant, not a caller convention: steps 5 and
	// 6b iterate this slice directly, so a slice built from map iteration would
	// make omission choices and L1 upgrade order vary run to run while every
	// assertion in the suite still passed. Normalizing at entry is the same
	// move as DEV-10 and costs one line. Stable, so equal firstIndex values
	// keep their relative order instead of being reshuffled arbitrarily.
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].firstIndex < sources[j].firstIndex })

	st := &allocState{}
	maxDepth := req.MaxDepth
	if maxDepth == DepthNone {
		maxDepth = DepthL2
	}
	bySource := make(map[string]*progressiveSource, len(sources))
	for _, src := range sources {
		bySource[src.source] = src
	}

	// Step 2: resolve pins. De-duplicate; unmatched are reported, never
	// silently dropped. Under MaxDepth < L2 every pin is vacuous => unmatched.
	type pinTarget struct {
		src *progressiveSource
		idx int // index into src.results
	}
	seenPins := map[PinRef]bool{}
	var pinTargets []pinTarget
	for _, pin := range req.Pinned {
		if seenPins[pin] {
			continue
		}
		seenPins[pin] = true
		var target *pinTarget
		if maxDepth == DepthL2 {
			if src, ok := bySource[pin.Source]; ok {
				for i, res := range src.results {
					if res.Chunk.ID == pin.ChunkID {
						target = &pinTarget{src: src, idx: i}
						break
					}
				}
			}
		}
		if target == nil {
			st.unmatchedPins = append(st.unmatchedPins, pin)
			continue
		}
		pinTargets = append(pinTargets, *target)
	}

	// Step 3: reserve pinned evidence plus each pinned source's cheapest
	// evidence-bearing orientation. Overflow of either budget is an error.
	pinnedTokens, pinnedBytes, pinnedSources := 0, 0, 0
	pinnedOrientation := map[string]bool{}
	for _, pt := range pinTargets {
		if !pinnedOrientation[pt.src.source] {
			pinnedOrientation[pt.src.source] = true
			o := orientationText(pt.src, cheapestOrientation(pt.src))
			pinnedTokens += safeEstimate(estimate, o)
			pinnedBytes += len(o)
			if pinnedSources > 0 { // separator between pinned sources
				pinnedTokens += safeEstimate(estimate, "\n")
				pinnedBytes++
			}
			pinnedSources++
		}
		e := evidenceText(pt.src.results[pt.idx])
		pinnedTokens += safeEstimate(estimate, e)
		pinnedBytes += len(e)
	}
	if pinnedTokens > req.MaxTokens || pinnedBytes > req.MaxBytes {
		return nil, fmt.Errorf(
			"rag: progressive render: pinned blocks need %d tokens / %d bytes, budget is %d / %d",
			pinnedTokens, pinnedBytes, req.MaxTokens, req.MaxBytes)
	}
	for _, pt := range pinTargets {
		src := pt.src
		if src.orientation == orientationNone {
			level := cheapestOrientation(src)
			o := orientationText(src, level)
			st.tokensUsed += safeEstimate(estimate, o) + separatorTokens(st, estimate)
			st.bytesUsed += len(o) + separatorBytes(st)
			src.orientation = level
			st.renderedSources++
		}
		e := evidenceText(src.results[pt.idx])
		st.tokensUsed += safeEstimate(estimate, e)
		st.bytesUsed += len(e)
		src.evidence = append(src.evidence, pt.idx)
		src.decisions[DecisionCallerPinned] = true
	}

	// Step 4: L2 floor preference — top MinFullResults results in retrieval
	// order, excluding already-pinned results. Greedy; shortfall recorded.
	if maxDepth == DepthL2 {
		floor := req.MinFullResults
		if floor == 0 {
			floor = 1
		}
		st.floorRequested = floor
		type flatResult struct {
			src *progressiveSource
			idx int
			pos int // global retrieval position
		}
		var flat []flatResult
		for _, src := range sources {
			for i := range src.results {
				flat = append(flat, flatResult{src, i, src.resultIdx[i]})
			}
		}
		// Stable: pos comes from resultIdx, which is only globally distinct by
		// the caller's convention. Same argument as sorting sources at entry —
		// do not let a cross-file convention decide output order.
		sort.SliceStable(flat, func(i, j int) bool { return flat[i].pos < flat[j].pos })
		for _, fr := range flat {
			if st.floorRendered >= floor {
				break
			}
			if containsInt(fr.src.evidence, fr.idx) {
				continue // already pinned; floor counts only floor-admitted results
			}
			cost, bytes := 0, 0
			if fr.src.orientation == orientationNone {
				o := orientationText(fr.src, cheapestOrientation(fr.src))
				cost += safeEstimate(estimate, o) + separatorTokens(st, estimate)
				bytes += len(o) + separatorBytes(st)
			}
			e := evidenceText(fr.src.results[fr.idx])
			cost += safeEstimate(estimate, e)
			bytes += len(e)
			if !st.admit(req, cost, bytes) {
				// Spec step 8: skip, keep considering later results — but a
				// block rejected once is NEVER reconsidered (spec section 10:
				// "rejected once and never reconsidered counts once").
				fr.src.rejectedEvidence = append(fr.src.rejectedEvidence, fr.idx)
				fr.src.costRejected = true
				continue
			}
			if fr.src.orientation == orientationNone {
				fr.src.orientation = cheapestOrientation(fr.src)
				st.renderedSources++
			}
			fr.src.evidence = append(fr.src.evidence, fr.idx)
			fr.src.decisions[DecisionFloorReserved] = true
			st.floorRendered++
		}
	}

	// Step 5: base orientation for every remaining source, in source order.
	for _, src := range sources {
		if src.orientation != orientationNone {
			continue
		}
		level := cheapestOrientation(src)
		// Currently UNREACHABLE and deliberately kept: cheapestOrientation
		// returns only orientationL0 or orientationMeta, and when maxDepth is
		// DepthL0 step 6b never runs either, so nothing can produce
		// orientationL0L1 at this point. Kept for the same reason as the >= in
		// orientationText (DEV-13): if cheapestOrientation ever returns L0L1,
		// or a level is added between them, this is what stops an overview
		// rendering above the caller's depth ceiling. No test can catch its
		// deletion — do not read it as live logic, do not delete it as dead.
		if maxDepth == DepthL0 && level == orientationL0L1 {
			level = orientationL0
		}
		o := orientationText(src, level)
		if !st.admit(req, safeEstimate(estimate, o)+separatorTokens(st, estimate), len(o)+separatorBytes(st)) {
			// Spec 9.5 rung 2: a fresh source whose stored abstract does not
			// fit falls back to the metadata overview rather than vanishing.
			// Step 5's "stored L0 abstract when the summary is fresh" is the
			// PREFERENCE, not the only option — 9.3 lists A0 below A1
			// cheapest-first, 9.5 names the metadata overview as the rung
			// below a stored summary, and step 8 skips an oversized
			// alternative rather than treating it as fatal. A0 is cheaper
			// than A1 for essentially every source, so without this a source
			// contributes nothing where a one-line block would have fit.
			if level == orientationL0 {
				// Set before rendering, not after: the flag changes the note
				// line, so the block charged here must be the block assembly
				// will emit. Reset if it does not fit — an omitted source has
				// no note to qualify, and a stale true would misreport it.
				src.summaryBudgetOmitted = true
				fallback := orientationText(src, orientationMeta)
				if st.admit(req, safeEstimate(estimate, fallback)+separatorTokens(st, estimate), len(fallback)+separatorBytes(st)) {
					src.orientation = orientationMeta
					// A1 was evaluated and rejected on cost and A0 rendered:
					// that is exactly budget_demoted (spec section 10).
					src.costRejected = true
					st.renderedSources++
					continue
				}
				src.summaryBudgetOmitted = false
			}
			src.decisions[DecisionNoFit] = true
			st.omitted++
			continue
		}
		src.orientation = level
		st.renderedSources++
	}

	// Step 6a: evidence upgrades in retrieval order.
	if maxDepth == DepthL2 {
		type flatResult struct {
			src *progressiveSource
			idx int
			pos int
		}
		var flat []flatResult
		for _, src := range sources {
			if src.orientation == orientationNone {
				continue // omitted sources cannot carry evidence (orientation always accompanies it)
			}
			for i := range src.results {
				if containsInt(src.evidence, i) || containsInt(src.rejectedEvidence, i) {
					continue // rejected once => never reconsidered (spec section 10)
				}
				flat = append(flat, flatResult{src, i, src.resultIdx[i]})
			}
		}
		// Stable: pos comes from resultIdx, which is only globally distinct by
		// the caller's convention. Same argument as sorting sources at entry —
		// do not let a cross-file convention decide output order.
		sort.SliceStable(flat, func(i, j int) bool { return flat[i].pos < flat[j].pos })
		for _, fr := range flat {
			e := evidenceText(fr.src.results[fr.idx])
			if !st.admit(req, safeEstimate(estimate, e), len(e)) {
				fr.src.rejectedEvidence = append(fr.src.rejectedEvidence, fr.idx)
				fr.src.costRejected = true
				continue
			}
			fr.src.evidence = append(fr.src.evidence, fr.idx)
			fr.src.decisions[DecisionRankUpgraded] = true
		}
	}

	// Step 6b: one orientation-upgrade pass in source order:
	// A1 -> A2 and A3b -> A3c (fresh summaries only; delta-cost admission).
	if maxDepth >= DepthL1 {
		for _, src := range sources {
			if !src.fresh || src.orientation != orientationL0 {
				continue
			}
			oldText := orientationText(src, orientationL0)
			newText := orientationText(src, orientationL0L1)
			// Signed delta: replace the old complete alternative's cost with
			// the new one. No clamping — a non-monotonic injected estimator
			// must not desynchronize allocator totals from measured blocks.
			deltaTokens := safeEstimate(estimate, newText) - safeEstimate(estimate, oldText)
			deltaBytes := len(newText) - len(oldText)
			if !st.admit(req, deltaTokens, deltaBytes) {
				src.costRejected = true
				continue
			}
			src.orientation = orientationL0L1
			src.decisions[DecisionRankUpgraded] = true
		}
	}

	// Evidence lists must render in retrieval order regardless of admission
	// order: a pin admits its result in step 3, before the floor admits any
	// earlier-ranked one in step 4.
	for _, src := range sources {
		sort.Ints(src.evidence)
		// budget_demoted needs BOTH halves of the spec section 10 condition —
		// a more expensive alternative rejected on cost AND a cheaper one
		// rendered. The orientation guard is the second half: costRejected is
		// set in step 4 before anything has rendered for that source, so
		// without it an omitted source would claim budget_demoted alongside
		// no_fit, which is incoherent — nothing was demoted, nothing rendered.
		if src.costRejected && src.orientation != orientationNone {
			src.decisions[DecisionBudgetDemoted] = true
		}
	}
	return st, nil
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
