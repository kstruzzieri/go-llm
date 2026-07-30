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
		return (len(s) + 3) / 4
	}
}

// mixedAllocState is the running admission ledger. used is exact: for a pure
// estimator it equals a fresh recompute of everything the allocator decided to
// materialize (#331 spec 4.1).
type mixedAllocState struct {
	budget             int
	used               int
	verbatimShortfalls int // anchors whose MinVerbatim preference went unmet, evicted ones included
	evictedGroups      int
	// anchorOmissions counts subjects dropped from a RETAINED anchor — content
	// the model never sees while its carrier message, its chain and every other
	// signal look nominal. Disjoint from evictedGroups by construction: a
	// subject under an evicted chain is already counted there as one group, and
	// counting it twice would inflate both figures.
	anchorOmissions int
}

func (st *mixedAllocState) fits(tokens int) bool { return st.used+tokens <= st.budget }

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
	dTok := m.estimate(next) - a.tokens
	if !st.fits(dTok) {
		s.chosen = prev
		return false
	}
	st.used += dTok
	a.content, a.tokens = next, a.tokens+dTok
	a.verbatimGot += verbatimComponents(s.alts[i].Desc)
	if prev >= 0 {
		a.verbatimGot -= verbatimComponents(s.alts[prev].Desc)
	}
	s.decision = decision
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
	for _, u := range units {
		if u.kind == unitPinned || u.kind == unitUnresolved {
			st.used += u.baseTokens
		}
	}
	if st.used > st.budget {
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
	for m.upgradeOnce(ctx, st, units, true) || m.upgradeOnce(ctx, st, units, false) {
	}
	return st, nil
}

// admitSpan charges one whole conversation span or explicitly omits it. Spans
// are atomic: a dropped question must never orphan its answer.
func (m ContextManager) admitSpan(st *mixedAllocState, u *mixedUnit) {
	if st.fits(u.baseTokens) {
		st.used += u.baseTokens
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
	capViolated := false
	for _, a := range u.anchors {
		// The cap bounds final model-visible bytes, and the placeholder is
		// model-visible, so a cap that cannot hold it has no legal rendering.
		if len(omittedObservation) > a.cap {
			capViolated = true
		}
		a.content = omittedObservation
		a.tokens = m.estimate(a.content)
		base += a.tokens
	}
	if capViolated || !st.fits(base) {
		// Evicted: content/tokens set above are never charged and never
		// materialized, because Task 8 skips evicted chains entirely.
		u.evicted = true
		st.evictedGroups++
		omitSubject(u.subject, OmitChainEvicted)
		for _, a := range u.anchors {
			if a.minVerbatim > 0 {
				st.verbatimShortfalls++ // the preference went unmet, evicted or not (spec 3.4)
			}
			for _, s := range a.subjects {
				omitSubject(s, OmitChainEvicted)
			}
		}
		return
	}
	st.used += base
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
		if a.verbatimGot < a.minVerbatim {
			st.verbatimShortfalls++
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
			s.chosen = 0
			wouldBe := anchorJoined(a)
			s.chosen = -1
			if len(wouldBe) > a.cap {
				omitSubject(s, OmitByteCap)
			} else {
				omitSubject(s, OmitTokenBudget)
			}
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
// ponytail: a full re-scan per commit, ~O(groups^2.5) in the counts and linear
// in total content bytes, since every candidate re-joins its whole anchor. On a
// 10-byte-per-rung fixture: 8 groups x 32 alternatives 635us, 128x32 1.24s, and
// tens of seconds at the maxContextGroups / maxContextAlternatives ceiling
// (measured 21.1s and 49.2s on two differently-sized ceiling fixtures — the
// figure tracks bytes, not just counts). Realistic input is a retrieve call's
// ~8 sources x ~30 alternatives, i.e. sub-millisecond, and the carrier bounds
// keep even a pathological set finite, which is what they exist for; ctx is the
// backstop for the rest. Worth memoizing per-anchor candidate costs and
// invalidating only the committed anchor if realistic sizes ever approach the
// ceiling. Not done now because #331 Task 11's exhaustive oracle is written
// against this exact greedy shape, and a silent divergence between allocator and
// oracle costs more than the scan does.
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
				for next := s.chosen + 1; next < len(s.alts); next++ {
					if evidenceOnly && verbatimComponents(s.alts[next].Desc) <= verbatimComponents(s.alts[s.chosen].Desc) {
						continue
					}
					prev := s.chosen
					s.chosen = next
					joined := anchorJoined(a)
					s.chosen = prev
					// The cap must be checked HERE, not only in tryAssign. An
					// over-cap candidate with the smallest dTok would otherwise win
					// selection, fail at commit, and stop the whole upgrade phase
					// with affordable upgrades elsewhere left unbought — starvation,
					// not just a wasted round.
					if len(joined) > a.cap {
						continue
					}
					dTok := m.estimate(joined) - a.tokens
					if !st.fits(dTok) {
						continue
					}
					// Strict <: ties keep the EARLIER candidate, so the winner is
					// stable in input order (spec 4.1 step 6). <= would silently
					// hand every tie to the last subject scanned.
					if best == nil || dTok < best.dTok {
						best = &cand{a: a, s: s, next: next, dTok: dTok}
					}
					break // cheapest affordable later alternative for this subject
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
