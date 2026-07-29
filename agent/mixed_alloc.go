package agent

import "strings"

// guardedEstimate wraps a caller estimator with the same contract as rag's
// safeEstimate (rag/progressive_alloc.go): empty text is free, and a
// non-positive result for NON-empty text falls back to the default heuristic.
// Without the fallback an estimator that reports zero for real text would let a
// block of any size through admission, so the guard is a ceiling invariant, not
// a defensive nicety.
//
// Task 8 wraps ContextManager.Estimate with this ONCE, before building units, so
// span/envelope costs (messageCost) and the allocator's deltas share a single
// function — mixing a guarded and a raw estimator would break the exactness
// identity below.
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
// wholesale, a.tokens always equals est(a.content) and admission equals final
// recomputation for any pure estimator (#331 spec 4.1).
func (m ContextManager) tryAssign(st *mixedAllocState, a *mixedAnchor, s *mixedSubject, i int, est func(string) int, decision string) bool {
	prev := s.chosen
	s.chosen = i
	next := anchorJoined(a)
	if len(next) > a.cap {
		s.chosen = prev
		return false
	}
	dTok := est(next) - a.tokens
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

// allocateMixed runs the fixed lane-ordered allocation policy (#331 spec 4.1)
// over units in stable input order, writing chosen/decision/reason onto every
// subject and content/tokens onto every retained anchor. It mutates units in
// place and returns the ledger; the caller materializes and traces from the same
// structures (Task 8).
//
// budget is the state budget already net of tool schemas. est must be the
// guarded estimator.
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
func (m ContextManager) allocateMixed(units []*mixedUnit, budget, sysTokens int, est func(string) int) (*mixedAllocState, error) {
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

	// 2. Lane 0: current-run plain exchanges. Highest retention priority — the
	// reverse of RecencyCompactor's drop order.
	for _, u := range units {
		if u.kind != unitPlainSpan {
			continue
		}
		m.admitSpan(st, u)
	}

	// 3. Lane 1: completed tool chains in input order.
	for _, u := range units {
		if u.kind != unitChain {
			continue
		}
		m.allocateChain(st, u, est)
	}

	// 4. Lane 2: prior raw-history exchanges. Lowest retention priority.
	for _, u := range units {
		if u.kind != unitHistorySpan {
			continue
		}
		m.admitSpan(st, u)
	}

	// 5. Upgrades to ANY later-declared alternative: declaration order IS utility
	// order, so evidence-adding targets are considered first and then the
	// cheapest exact marginal cost wins. Adjacent-step-only would force an
	// orientation upgrade before any evidence, because the prefix families
	// interleave the orientation rungs with the evidence ones (spec 4.1 step 6).
	for m.upgradeOnce(st, units, est, true) || m.upgradeOnce(st, units, est, false) {
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
func (m ContextManager) allocateChain(st *mixedAllocState, u *mixedUnit, est func(string) int) {
	base := u.baseTokens
	capViolated := false
	for _, a := range u.anchors {
		// The cap bounds final model-visible bytes, and the placeholder is
		// model-visible, so a cap that cannot hold it has no legal rendering.
		if len(omittedObservation) > a.cap {
			capViolated = true
		}
		a.content = omittedObservation
		a.tokens = est(a.content)
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
			for i := range s.alts { // declaration order: cheapest evidence-bearing first
				if verbatimComponents(s.alts[i].Desc) == 0 {
					continue
				}
				if m.tryAssign(st, a, s, i, est, DecisionFloor) {
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
			for i := range s.alts { // declaration order = cheapest-first
				if m.tryAssign(st, a, s, i, est, DecisionBase) {
					admitted = true
					break
				}
			}
			if admitted {
				continue
			}
			// Attribute the omission to the constraint that actually blocked the
			// CHEAPEST alternative, so the trace reason is diagnostic rather
			// than a catch-all.
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
func (m ContextManager) upgradeOnce(st *mixedAllocState, units []*mixedUnit, est func(string) int, evidenceOnly bool) bool {
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
					if len(joined) > a.cap {
						continue
					}
					dTok := est(joined) - a.tokens
					if !st.fits(dTok) {
						continue
					}
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
	return m.tryAssign(st, best.a, best.s, best.next, est, DecisionUpgrade)
}
