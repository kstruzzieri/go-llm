package agent

import (
	"context"
	"strings"
)

// guardedEstimate wraps a caller estimator with the same contract as rag's
// safeEstimate (rag/progressive_alloc.go): empty text is free, and a
// non-positive result for NON-empty text falls back to the default heuristic.
// Without the fallback an estimator that reports zero for real text would let a
// block of any size through admission, so the guard is a ceiling invariant, not
// a defensive nicety.
//
// Task 8 wraps ContextManager.Estimate with this ONCE, before building units.
// Everything downstream then reads that one field: span and envelope costs go
// through messageCost and the allocator's deltas through m.estimate, so there is
// no second estimator channel that could drift out of alignment and break the
// exactness identity below.
func guardedEstimate(est func(string) int) func(string) int {
	return func(s string) int {
		if s == "" {
			return 0
		}
		if est != nil {
			if n := est(s); n > 0 {
				return n
			}
		}
		return 1 + (len(s)-1)/4
	}
}

// mixedAllocState is the running admission ledger. used is exact: for a pure
// estimator it equals a fresh recompute of everything the allocator decided to
// materialize (#331 spec 4.1).
type mixedAllocState struct {
	budget             int
	used               int
	arithmeticOverflow bool
	verbatimShortfalls int // anchors whose MinVerbatim preference went unmet, evicted ones included
	evictedGroups      int
	// anchorOmissions counts subjects dropped from a RETAINED anchor — content
	// the model never sees while its carrier message, its chain and every other
	// signal look nominal. Disjoint from evictedGroups by construction: a
	// subject under an evicted chain is already counted there as one group, and
	// counting it twice would inflate both figures.
	anchorOmissions int
}

func (st *mixedAllocState) fits(tokens int) bool {
	if st.arithmeticOverflow {
		return false
	}
	next, ok := checkedTokenAdd(st.used, tokens)
	if !ok {
		st.used, st.arithmeticOverflow = next, true
	}
	return ok && next <= st.budget
}

func (st *mixedAllocState) charge(tokens int) bool {
	if st.arithmeticOverflow {
		return false
	}
	var ok bool
	st.used, ok = checkedTokenAdd(st.used, tokens)
	st.arithmeticOverflow = st.arithmeticOverflow || !ok
	return ok
}

// anchorJoined renders the anchor's would-be Content for the current chosen
// assignment: the selected fragments in GROUP (subject) order joined by "\n", or
// the fixed omission placeholder when nothing is chosen. Admission and (Task 8)
// materialization share this one function, which is what makes the ledger exact
// rather than an estimate of an estimate.
func anchorJoined(a *mixedAnchor) string {
	var parts []string
	for _, s := range a.subjects {
		if s.chosen >= 0 {
			parts = append(parts, s.alts[s.chosen].Content)
		}
	}
	if len(parts) == 0 {
		return omittedObservation
	}
	return strings.Join(parts, "\n")
}

// tryAssign trial-assigns alternative i to s and charges the EXACT whole-anchor
// deltas — estimate(newJoined) - estimate(oldJoined) for tokens, and the joined
// byte length against the anchor cap. It commits when both accept and reverts
// otherwise, so a rejected candidate leaves no trace.
//
// Exactness removes every estimator-additivity assumption: because the anchor's
// content starts as the precharged placeholder and each admission replaces it
// wholesale, a.tokens always equals m.estimate(a.content) and admission equals
// final recomputation for any pure estimator (#331 spec 4.1).
func (m ContextManager) tryAssign(st *mixedAllocState, a *mixedAnchor, s *mixedSubject, i int, decision string) bool {
	prev := s.chosen
	s.chosen = i
	next := anchorJoined(a)
	if len(next) > a.cap {
		s.chosen = prev
		return false
	}
	nextTokens := m.estimate(next)
	dTok := nextTokens - a.tokens
	if !st.fits(dTok) {
		s.chosen = prev
		return false
	}
	st.charge(dTok) // fits already proved this addition cannot overflow
	a.content, a.tokens = next, nextTokens
	a.verbatimGot += verbatimComponents(s.alts[i].Desc)
	if prev >= 0 {
		a.verbatimGot -= verbatimComponents(s.alts[prev].Desc)
	}
	s.decision = decision
	// Everything upgradeCandidate memoized about THIS anchor is now stale: the
	// joined text, a.tokens and this subject's own base index all just moved.
	// Dropped here rather than in upgradeOnce because tryAssign is the single
	// place any anchor's assignment changes — the floor pass and base admission
	// commit through it too, and a cache invalidated only on the upgrade path
	// would survive those with a stale a.tokens.
	for _, sub := range a.subjects {
		sub.cands = nil
	}
	return true
}

// omitSubject records an explicit omission. Never a silent skip: a subject that
// reached no alternative must still carry a decision and a reason (spec 3.4 D6).
func omitSubject(s *mixedSubject, reason string) {
	s.omitted = true
	s.decision = DecisionOmitted
	s.reason = reason
}

// allocateMixed runs the fixed lane-ordered allocation policy (#331 spec 4.1),
// admitting NEWEST-first within each lane, and writes chosen/decision/reason
// onto every subject and content/tokens onto every retained anchor. It mutates
// units in place and returns the ledger; the caller materializes and traces from
// the same structures, walking units forward, so the OUTPUT is in stable input
// order regardless of the order they were admitted in (Task 8).
//
// budget is the state budget already net of tool schemas. Every cost goes
// through m.estimate, so the caller must have wrapped m.Estimate with
// guardedEstimate BEFORE building the units it passes here: that single field is
// what makes the builder's envelope costs and these deltas commensurable.
//
// ctx only cancels the upgrade phase, which is the sole unbounded-ish loop. A
// cancelled context yields the fully-admitted allocation without upgrades — a
// valid result satisfying every invariant, not a partial one.
//
// sysTokens covers only what units do NOT represent: the system prompt and the
// materialized durable summary. Pinned MESSAGES are units (unitPinned) and are
// charged in step 1 — adding them to sysTokens as well would double-charge the
// whole pinned span, and the ledger would be silently wrong in the one direction
// that manufactures an exhaustion.
//
// ErrContextExhausted is the only error: the must-fit reservations alone exceed
// the budget. st.used then reports the FULL must-fit cost so the caller's
// pressure row covers the reservations, not just the pinned subset (spec 5).
func (m ContextManager) allocateMixed(ctx context.Context, units []*mixedUnit, budget, sysTokens int) (*mixedAllocState, error) {
	st := &mixedAllocState{budget: budget, used: sysTokens}

	// 1. Must-fit reservations: pinned spans and unresolved tool chains. Neither
	// is droppable, so they are charged before any lane competes for the rest.
	reservationsOK := true
	for _, u := range units {
		if u.kind == unitPinned || u.kind == unitUnresolved {
			reservationsOK = st.charge(u.baseTokens) && reservationsOK
		}
	}
	if !reservationsOK || st.used > st.budget {
		return st, ErrContextExhausted
	}

	// Lanes 0-2. Two ORTHOGONAL orderings are in play and conflating them
	// inverts the policy:
	//
	//   - Lane PRECEDENCE (which kind competes first) is the reverse of
	//     RecencyCompactor's dropKind order: plain > chains > history.
	//   - WITHIN a lane, admission is NEWEST-FIRST — hence the descending walks
	//     below. Compact's dropKind scans groups from index 0 and drops until the
	//     transcript fits, so it KEEPS the newest members of each kind. Admitting
	//     in forward State order would keep the oldest instead: the model would
	//     receive its stalest prior exchanges and lose the most recent ones, which
	//     is worse than not compacting at all.
	//
	// Only the admission ORDER reverses. Materialization and tracing walk units
	// forward, so output stays in State order.

	// 2. Lane 0: current-run plain exchanges. Highest retention priority.
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].kind != unitPlainSpan {
			continue
		}
		m.admitSpan(st, units[i])
	}

	// 3. Lane 1: completed tool chains, newest first.
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].kind != unitChain {
			continue
		}
		m.allocateChain(st, units[i])
	}

	// 4. Lane 2: prior raw-history exchanges. Lowest retention priority.
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].kind != unitHistorySpan {
			continue
		}
		m.admitSpan(st, units[i])
	}

	// 5. Upgrades to ANY later-declared alternative: declaration order IS utility
	// order, so evidence-adding targets are considered first and then the
	// cheapest exact marginal cost wins. Adjacent-step-only would force an
	// orientation upgrade before any evidence, because the prefix families
	// interleave the orientation rungs with the evidence ones (spec 4.1 step 6).
	for {
		if m.upgradeOnce(ctx, st, units, true) || m.upgradeOnce(ctx, st, units, false) {
			m.retryOmitted(st, units)
			continue
		}
		break
	}
	if !st.arithmeticOverflow {
		m.refreshOmissionReasons(st, units)
	}
	for _, u := range units {
		for _, a := range u.anchors {
			if a.minVerbatim > 0 && a.verbatimGot < a.minVerbatim {
				st.verbatimShortfalls++
			}
		}
	}
	if st.arithmeticOverflow {
		return st, ErrContextExhausted
	}
	return st, nil
}

// admitSpan charges one whole conversation span or explicitly omits it. Spans
// are atomic: a dropped question must never orphan its answer.
func (m ContextManager) admitSpan(st *mixedAllocState, u *mixedUnit) {
	if st.fits(u.baseTokens) {
		st.charge(u.baseTokens)
		u.subject.chosen, u.subject.decision = 0, DecisionBase
		return
	}
	omitSubject(u.subject, OmitTokenBudget)
	st.evictedGroups++
}

// allocateChain charges one completed chain's base footprint with the omission
// placeholder PRECHARGED as every structured anchor's initial content, then runs
// the verbatim-floor pass and base admission over its subjects.
//
// The chain's envelope footprint (u.baseTokens) is charged exactly once, here,
// and is what the chain-level subject accounts for; anchor CONTENT is charged
// separately per admitted alternative. The two are disjoint by construction, so
// nothing double-counts.
func (m ContextManager) allocateChain(st *mixedAllocState, u *mixedUnit) {
	base := u.baseTokens
	baseOK := true
	capViolated := false
	for _, a := range u.anchors {
		// The cap bounds final model-visible bytes, and the placeholder is
		// model-visible, so a cap that cannot hold it has no legal rendering.
		if len(omittedObservation) > a.cap {
			capViolated = true
		}
		a.content = omittedObservation
		a.tokens = m.estimate(a.content)
		var ok bool
		base, ok = checkedTokenAdd(base, a.tokens)
		baseOK = ok && baseOK
	}
	if capViolated || !baseOK || !st.fits(base) {
		if !baseOK {
			st.used, st.arithmeticOverflow = base, true
		}
		// Evicted: content/tokens set above are never charged and never
		// materialized, because Task 8 skips evicted chains entirely.
		u.evicted = true
		st.evictedGroups++
		omitSubject(u.subject, OmitChainEvicted)
		for _, a := range u.anchors {
			for _, s := range a.subjects {
				omitSubject(s, OmitChainEvicted)
			}
		}
		return
	}
	st.charge(base)
	// The chain span is retained. Every completed chain carries this subject,
	// structured or not, so an unstructured chain (no anchors at all) still has
	// exactly one decided trace row.
	u.subject.chosen, u.subject.decision = 0, DecisionBase

	for _, a := range u.anchors {
		if a.minVerbatim <= 0 {
			continue
		}
		for _, s := range a.subjects {
			if a.verbatimGot >= a.minVerbatim {
				break
			}
			// declaration order = ascending utility; alternative 0 is the cheapest
			// for every in-repo producer, so the first evidence-bearing
			// alternative that fits is also the cheapest one that does.
			for i := range s.alts {
				if verbatimComponents(s.alts[i].Desc) == 0 {
					continue
				}
				if m.tryAssign(st, a, s, i, DecisionFloor) {
					break
				}
			}
			// The floor pass NEVER omits. A non-fit leaves the subject eligible
			// for base admission below, which may well afford its cheap
			// orientation alternative (spec 4.1 step 4).
		}
	}

	for _, a := range u.anchors {
		for _, s := range a.subjects {
			if s.chosen != -1 || s.omitted {
				continue
			}
			admitted := false
			// Declaration order is ascending UTILITY — the one ordering
			// validateContextSet actually enforces. Cost is NOT enforced to be
			// monotone and in-repo producers do not deliver it: rag's fresh-source
			// ladder renders alternative 0 at orientationMeta, which carries a
			// trailing "note: metadata overview (summary omitted: budget)" line
			// that alternative 1 (orientationL0, which writes "purpose:" instead)
			// does not, so alternative 0's bytes are not a prefix of alternative
			// 1's and can even be the dearer of the two. First-that-fits is
			// therefore lowest-utility-that-fits, not strictly cheapest-that-fits;
			// the loop still finds a fitting alternative whenever one exists, it
			// just may not be the globally cheapest. The omission probe below
			// inherits the same caveat.
			for i := range s.alts {
				if m.tryAssign(st, a, s, i, DecisionBase) {
					admitted = true
					break
				}
			}
			if admitted {
				continue
			}
			// Attribute the omission to the constraint that blocked the
			// LOWEST-UTILITY alternative, so the trace reason is diagnostic
			// rather than a catch-all. It is alternative 0 specifically, which
			// is not guaranteed to be the cheapest (see above), so with a
			// non-monotone-cost producer the named constraint can be the one
			// that blocked alternative 0 rather than the one that blocked them
			// all. Diagnostic, never load-bearing: the subject is omitted either
			// way.
			//
			// The chain and its anchor are RETAINED, so no group eviction records
			// this: without the counter a byte-cap omission is invisible to every
			// operator signal (Pressure, the compaction event, ToolResult.Truncated,
			// which describes the discarded flat rendering). Counted for BOTH
			// reasons — a token-budget omission inside a retained anchor is exactly
			// as silent, it just happens to correlate with a high UsedPct.
			st.anchorOmissions++
			omitSubject(s, m.omissionReason(st, a, s))
		}
	}
}

// refreshOmissionReasons re-prices retained omissions against the terminal
// anchor and ledger. Chain evictions and malformed no-alternative subjects keep
// their original semantics.
func (m ContextManager) refreshOmissionReasons(st *mixedAllocState, units []*mixedUnit) {
	for _, u := range units {
		if u.kind != unitChain || u.evicted {
			continue
		}
		for _, a := range u.anchors {
			for _, s := range a.subjects {
				if s.omitted && s.reason != OmitChainEvicted && len(s.alts) > 0 {
					s.reason = m.omissionReason(st, a, s)
				}
			}
		}
	}
}

// omissionReason diagnoses the lowest-utility alternative, matching base
// admission's deterministic declaration order and byte-before-token checks.
func (m ContextManager) omissionReason(st *mixedAllocState, a *mixedAnchor, s *mixedSubject) string {
	if len(s.alts) == 0 {
		return s.reason
	}
	prev := s.chosen
	s.chosen = 0
	next := anchorJoined(a)
	s.chosen = prev
	if len(next) > a.cap {
		return OmitByteCap
	}
	nextTokens := m.estimate(next)
	total, ok := checkedTokenAdd(st.used, nextTokens-a.tokens)
	if !ok || total > st.budget {
		return OmitTokenBudget
	}
	if s.reason == "" {
		return OmitTokenBudget // all alternatives just failed; estimator re-priced during diagnosis
	}
	return s.reason
}

// retryOmitted gives retained-anchor subjects another base-admission chance
// after an upgrade changes capacity. Chains keep their original newest-first
// order; subjects keep declaration order. A successful retry removes the
// omission from pressure accounting.
func (m ContextManager) retryOmitted(st *mixedAllocState, units []*mixedUnit) {
	for {
		progress := false
		for i := len(units) - 1; i >= 0; i-- {
			u := units[i]
			if u.kind != unitChain || u.evicted {
				continue
			}
			for _, a := range u.anchors {
				for _, s := range a.subjects {
					if !s.omitted || s.reason == OmitChainEvicted {
						continue
					}
					for j := range s.alts {
						if !m.tryAssign(st, a, s, j, DecisionBase) {
							continue
						}
						s.omitted, s.reason = false, ""
						st.anchorOmissions--
						progress = true
						break
					}
				}
			}
		}
		if !progress {
			return
		}
	}
}

// upgradeOnce commits the single best affordable upgrade and reports whether it
// did. evidenceOnly restricts candidates to alternatives that ADD verbatim
// components, which is the first of the two passes spec 4.1 step 6 prescribes.
//
// Per subject only the cheapest affordable later alternative in declaration
// order competes; across subjects the smallest exact marginal cost wins, with
// input order breaking ties (strict <, so the first candidate found keeps it).
//
// Still a full re-scan of every subject per commit, but the scan itself is now
// memoized per anchor (upgradeCandidate): a round re-prices only the anchor the
// previous round committed to, and reads a cached verdict for the rest. Cost is
// therefore dominated by the number of commits times the number of SUBJECTS,
// not times total content bytes.
//
// Measured on a 20-step run of retrieve-shaped states, 2 KB blocks, median of
// three, before -> after this memo:
//
//	subjects   ceiling 8192          ceiling 131072
//	 40        537us  ->  206us      644us   ->  259us
//	100        70.7ms ->  6.41ms     39.5ms  ->  6.64ms
//	220        77.4ms ->  6.92ms     211.4ms ->  22.9ms
//	420        131.7ms -> 15.5ms     776.1ms ->  70.0ms
//
// 420 subjects is the worst case agent/tools' Retrieve can actually reach: it
// truncates to k TOTAL results across all sources and k <= maxRetrieveMaxK, so
// one call tops out at 20 groups, and a 20-step run at 420 subjects. The
// remaining 70ms is the per-round walk over all subjects; collapsing that needs
// a priority queue over anchors, which is not worth its invalidation bugs at
// these sizes.
//
// ponytail: the memo is deliberately per-ANCHOR-wide rather than per-subject.
// Invalidating only the committed subject leaves its siblings holding a cap
// verdict priced against a shorter anchor, and an over-cap candidate that wins
// selection stops the whole upgrade phase (TestMixedAllocUpgradeMemoMatchesUnmemoized,
// cap-bound case, is red without it). Dropping the invalidation altogether does
// not merely mis-order upgrades — it hands back a candidate whose index no
// longer advances s.chosen, and the caller's loop never terminates.
func (m ContextManager) upgradeOnce(ctx context.Context, st *mixedAllocState, units []*mixedUnit, evidenceOnly bool) bool {
	// Cancellation stops upgrading, it does not fail: base admission already
	// produced a complete allocation, so every invariant (D6 decisions, the
	// ledger identity, cap and budget compliance) holds without this phase.
	if ctx.Err() != nil {
		return false
	}
	type cand struct {
		a    *mixedAnchor
		s    *mixedSubject
		next int
		dTok int
	}
	var best *cand
	for _, u := range units {
		if u.kind != unitChain || u.evicted {
			continue
		}
		for _, a := range u.anchors {
			for _, s := range a.subjects {
				if s.chosen < 0 {
					continue
				}
				next, dTok, ok := m.upgradeCandidate(st, a, s, evidenceOnly)
				if !ok {
					continue
				}
				// Strict <: ties keep the EARLIER candidate, so the winner is
				// stable in input order (spec 4.1 step 6). <= would silently
				// hand every tie to the last subject scanned.
				if best == nil || dTok < best.dTok {
					best = &cand{a: a, s: s, next: next, dTok: dTok}
				}
			}
		}
	}
	if best == nil {
		return false
	}
	// Returning tryAssign's own verdict rather than an unconditional true is what
	// bounds this loop: an impure estimator that re-priced the same candidate
	// between selection and commit would otherwise spin here forever. Each
	// successful upgrade strictly advances one subject's chosen index, so the
	// caller's loop terminates.
	return m.tryAssign(st, best.a, best.s, best.next, DecisionUpgrade)
}

// upgradeCandidate returns the cheapest affordable later alternative for s —
// the first one, in declaration order, that clears the evidenceOnly filter, the
// anchor byte cap and the remaining budget. It is the inner scan upgradeOnce
// used to run inline; the split exists so the per-anchor memo below has one
// owner.
//
// MEMO: everything the scan measures except affordability is anchor-local.
// dTok and the cap verdict depend on the anchor's joined text and a.tokens,
// which change ONLY when that anchor commits (tryAssign, which drops the memo);
// addsVerbatim compares against s.chosen, which likewise only moves on a commit
// to this anchor. So a candidate priced in one round stays correct for every
// later round until its own anchor is touched, and each round re-applies just
// st.fits — the one global term. s.cands is filled lazily, in declaration
// order, and never past the candidate that won: a scan that stops at index 3
// prices three candidates, and a later scan whose budget rejects them extends
// from there rather than re-pricing.
//
// The cap verdict is computed HERE, not left to tryAssign. An over-cap
// candidate with the smallest dTok would otherwise win selection, fail at
// commit, and stop the whole upgrade phase with affordable upgrades elsewhere
// left unbought — starvation, not just a wasted round. dTok stays unset for an
// over-cap candidate, exactly as the inline scan skipped the estimate call.
func (m ContextManager) upgradeCandidate(st *mixedAllocState, a *mixedAnchor, s *mixedSubject, evidenceOnly bool) (int, int, bool) {
	base := s.chosen + 1
	for i := 0; base+i < len(s.alts); i++ {
		if i == len(s.cands) {
			next := base + i
			prev := s.chosen
			s.chosen = next
			joined := anchorJoined(a)
			s.chosen = prev
			c := upgradeCand{
				next:         next,
				capOK:        len(joined) <= a.cap,
				addsVerbatim: verbatimComponents(s.alts[next].Desc) > verbatimComponents(s.alts[prev].Desc),
			}
			if c.capOK {
				c.dTok = m.estimate(joined) - a.tokens
			}
			s.cands = append(s.cands, c)
		}
		c := s.cands[i]
		if evidenceOnly && !c.addsVerbatim {
			continue
		}
		if !c.capOK || !st.fits(c.dTok) {
			continue
		}
		return c.next, c.dTok, true
	}
	return 0, 0, false
}

// upgradeCand is one later alternative priced against its anchor's current
// assignment. capOK and dTok are the cap and cost verdicts; addsVerbatim is the
// evidenceOnly filter's answer. All three survive any commit to a DIFFERENT
// anchor, which is what makes the memo sound.
type upgradeCand struct {
	next         int
	dTok         int
	capOK        bool
	addsVerbatim bool
}
