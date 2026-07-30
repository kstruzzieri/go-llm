package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// Ladder descriptors mirroring rag's repAbstract/repOverview/repEvidence. Local
// copies because those are unexported and agent must not import rag.
var (
	altAbstract = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationGenerated}
	altOverview = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationGenerated}
	altEvidence = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}
)

// nonAdditiveEstimator is deliberately NOT additive over concatenation:
// est(a)+est(b) != est(a+b) for almost every pair. Every accounting assertion
// runs under it because an allocator that sums per-fragment estimates instead
// of taking whole-anchor deltas can still balance its books under a purely
// additive estimator.
func nonAdditiveEstimator(s string) int { return len(s)/4 + 7 }

// ladderGroup mirrors rag.buildProgressiveGroups for one fresh source: two
// orientation rungs ([abstract], [abstract overview]) crossed with the evidence
// PREFIX families, ordered by (prefix length, rung index). With two evidence
// blocks that is the real six-alternative shape; a two-alternative synthetic
// cannot expose an adjacent-step-only upgrade bug.
//
// Costs under runeEstimator, with |abstract|=A, |overview|=O, |ev[k]|=E_k:
// A, A+O, A+E1, A+O+E1, A+E1+E2, A+O+E1+E2. Verbatim component counts:
// 0, 0, 1, 1, 2, 2.
func ladderGroup(id string, rank int, abstract, overview string, ev ...string) ContextGroup {
	rungs := [][]contextdepth.RepresentationDesc{
		{altAbstract},
		{altAbstract, altOverview},
	}
	orient := []string{abstract, abstract + overview}
	g := ContextGroup{Desc: contextdepth.GroupDesc{
		Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: id},
		Rank:    rank,
	}}
	add := func(reps []contextdepth.RepresentationDesc, content string) {
		g.Alternatives = append(g.Alternatives, ContextAlternative{
			Desc:    contextdepth.AlternativeDesc{Representations: slices.Clone(reps)},
			Content: content,
		})
	}
	for i, reps := range rungs {
		add(reps, orient[i])
	}
	prefix := ""
	for k := range ev {
		prefix += ev[k]
		for i, reps := range rungs {
			r := slices.Clone(reps)
			for range k + 1 {
				r = append(r, altEvidence)
			}
			add(r, orient[i]+prefix)
		}
	}
	return g
}

// dropPrefix1 removes the k=1 evidence family from a two-evidence ladder,
// leaving a legal ladder whose CHEAPEST evidence-bearing alternative carries two
// verbatim components. That shape is what proves the floor pass stops on
// component COUNT rather than on the number of subjects it admitted.
func dropPrefix1(g ContextGroup) ContextGroup {
	g.Alternatives = slices.Delete(slices.Clone(g.Alternatives), 2, 4)
	return g
}

func chainSet(minVerbatim int, groups ...ContextGroup) *ContextSet {
	return &ContextSet{MinVerbatim: minVerbatim, Groups: groups}
}

// allocFixture wires the estimator exactly as Task 8's AssembleWithTrace will:
// guarded ONCE into m.Estimate, so envelope/span costs (messageCost) and
// allocator deltas read the same single field.
func allocFixture(t *testing.T, est func(string) int, st State, budget, sysTokens int) ([]*mixedUnit, *mixedAllocState, error) {
	t.Helper()
	return allocFixtureCtx(t, context.Background(), est, st, budget, sysTokens)
}

func allocFixtureCtx(t *testing.T, ctx context.Context, est func(string) int, st State, budget, sysTokens int) ([]*mixedUnit, *mixedAllocState, error) {
	t.Helper()
	m := ContextManager{Estimate: guardedEstimate(est)}
	units, err := m.buildMixedUnits(st)
	if err != nil {
		t.Fatalf("buildMixedUnits: %v", err)
	}
	alloc, allocErr := m.allocateMixed(ctx, units, budget, sysTokens)
	return units, alloc, allocErr
}

func unitSubjects(u *mixedUnit) []*mixedSubject {
	var out []*mixedSubject
	if u.subject != nil {
		out = append(out, u.subject)
	}
	for _, a := range u.anchors {
		out = append(out, a.subjects...)
	}
	return out
}

// assertDecided enforces spec 3.4's D6 invariant over every subject the
// allocator touched: retained => a base/floor/upgrade decision, a chosen
// alternative and no omission reason; omitted => DecisionOmitted, chosen -1 and
// a reason. Task 11 property-tests this on traces; asserting it in EVERY
// allocator test is what makes a dropped chain-subject decision fail here
// instead of two tasks downstream.
func assertDecided(t *testing.T, units []*mixedUnit) {
	t.Helper()
	for i, u := range units {
		for _, s := range unitSubjects(u) {
			where := fmt.Sprintf("unit %d subject %s/%s", i, s.ref.Domain, s.ref.ID)
			if s.omitted {
				if s.decision != DecisionOmitted {
					t.Errorf("%s: omitted but decision = %q, want %q", where, s.decision, DecisionOmitted)
				}
				if s.reason == "" {
					t.Errorf("%s: omitted with no reason", where)
				}
				if s.chosen != -1 {
					t.Errorf("%s: omitted but chosen = %d, want -1", where, s.chosen)
				}
				continue
			}
			switch s.decision {
			case DecisionBase, DecisionFloor, DecisionUpgrade:
			default:
				t.Errorf("%s: retained with decision %q, want base/floor/upgrade", where, s.decision)
			}
			if s.reason != "" {
				t.Errorf("%s: retained but carries reason %q", where, s.reason)
			}
			if s.chosen < 0 {
				t.Errorf("%s: retained but chosen = %d", where, s.chosen)
			}
		}
	}
}

// ledgerRecompute is the INDEPENDENT cost of what allocation actually decided:
// every span and envelope re-derived from the raw Messages, plus a fresh
// estimate of every retained anchor's materialized content. It deliberately does
// NOT reuse u.baseTokens — a builder-side envelope error would then appear on
// both sides of the identity and cancel, leaving the check green while both
// numbers were wrong.
func ledgerRecompute(m ContextManager, units []*mixedUnit, sysTokens int) int {
	total := sysTokens
	spanCost := func(msgs []Message) int {
		n := 0
		for _, msg := range msgs {
			n += m.messageCost(msg)
		}
		return n
	}
	for _, u := range units {
		switch u.kind {
		case unitPinned, unitUnresolved:
			total += spanCost(u.msgs)
		case unitPlainSpan, unitHistorySpan:
			if !u.subject.omitted {
				total += spanCost(u.msgs)
			}
		case unitChain:
			if u.evicted {
				continue
			}
			// A structured anchor contributes its ENVELOPE only; its content is
			// allocated, not assumed. msg is a range copy, so blanking Content
			// cannot reach the aliased State.Messages.
			for _, msg := range u.msgs {
				if msg.Context != nil {
					msg.Content = ""
				}
				total += m.messageCost(msg)
			}
			for _, a := range u.anchors {
				total += m.estimate(a.content)
			}
		}
	}
	return total
}

// allocSnapshot renders every allocator-written field, for determinism
// comparison and as the failure dump.
func allocSnapshot(units []*mixedUnit, st *mixedAllocState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "used=%d shortfalls=%d evictedGroups=%d\n", st.used, st.verbatimShortfalls, st.evictedGroups)
	for i, u := range units {
		fmt.Fprintf(&b, "unit %d kind=%d evicted=%v base=%d\n", i, u.kind, u.evicted, u.baseTokens)
		for _, a := range u.anchors {
			fmt.Fprintf(&b, "  anchor %s tokens=%d content=%q\n", a.callID, a.tokens, a.content)
		}
		for _, s := range unitSubjects(u) {
			fmt.Fprintf(&b, "  subject %s/%s chosen=%d decision=%q reason=%q\n",
				s.ref.Domain, s.ref.ID, s.chosen, s.decision, s.reason)
		}
	}
	return b.String()
}

// findSubject locates one subject by its domain-qualified ID across all units.
func findSubject(t *testing.T, units []*mixedUnit, domain, id string) *mixedSubject {
	t.Helper()
	for _, u := range units {
		for _, s := range unitSubjects(u) {
			if s.ref.Domain == domain && s.ref.ID == id {
				return s
			}
		}
	}
	t.Fatalf("no subject %s/%s among the units", domain, id)
	return nil
}

func TestGuardedEstimate(t *testing.T) {
	tests := []struct {
		name string
		est  func(string) int
		in   string
		want int
	}{
		{"empty text costs nothing", runeEstimator, "", 0},
		{"empty text with a nil estimator", nil, "", 0},
		{"nil estimator falls back to len/4 ceiling", nil, "abcde", 2},
		{"positive estimate is honored", runeEstimator, "abcde", 5},
		{"zero for non-empty text falls back", func(string) int { return 0 }, "abcde", 2},
		{"negative estimate falls back", func(string) int { return -100 }, "abcde", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := guardedEstimate(tc.est)(tc.in); got != tc.want {
				t.Errorf("guardedEstimate(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestMixedAllocMustFitOverflow: pinned messages plus unresolved chains are
// reservations, not candidates. When they alone exceed the budget the allocator
// fails closed, and st.used reports the FULL must-fit cost (spec 4.1 step 2) —
// not the pinned subset, which is what the caller's pressure row needs.
func TestMixedAllocMustFitOverflow(t *testing.T) {
	// SYS-PROMPT(10) pinned + unresolved chain call(20) = 30 must-fit.
	st := State{Messages: []Message{
		pinned("system", "SYS-PROMPT"),
		mixedAsstCall("c1"),
	}}

	tests := []struct {
		name      string
		budget    int
		sysTokens int
		wantErr   bool
	}{
		{"must-fit exceeds budget by one", 29, 0, true},
		{"must-fit exactly fills budget", 30, 0, false},
		{"sysTokens push must-fit over", 34, 5, true},
		{"sysTokens fit exactly", 35, 5, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, alloc, err := allocFixture(t, runeEstimator, st, tc.budget, tc.sysTokens)
			if tc.wantErr {
				if !errors.Is(err, ErrContextExhausted) {
					t.Fatalf("err = %v, want ErrContextExhausted", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if want := 30 + tc.sysTokens; alloc.used != want {
				t.Errorf("used = %d, want %d (system + pinned + unresolved chain)", alloc.used, want)
			}
			if !tc.wantErr {
				assertDecided(t, units)
			}
			if len(units) != 2 {
				t.Fatalf("units = %d, want 2", len(units))
			}
			if units[0].kind != unitPinned || units[1].kind != unitUnresolved {
				t.Fatalf("fixture no longer exercises must-fit: kinds %d %d", units[0].kind, units[1].kind)
			}
		})
	}
}

// laneOrderState is the three-lane fixture: one prior-history exchange (4), one
// pinned message (3), one current-run plain exchange (4), one completed chain
// with a structured anchor (call 20 + envelope 10 = 30 base, + 26 placeholder).
// The anchor's cheapest alternative costs 40, i.e. +14 over the placeholder it
// replaces.
func laneOrderState() State {
	set := chainSet(0, ladderGroup("pkg/one.go", 1,
		strings.Repeat("A", 40), strings.Repeat("O", 30), strings.Repeat("E", 20)))
	return State{Messages: []Message{
		history("user", "HQ"),      // 2 \ prior history: lane 2
		history("assistant", "HA"), // 2 /
		pinned("system", "SYS"),    // 3   must-fit
		elastic("user", "PQ"),      // 2 \ current-run plain: lane 0
		elastic("assistant", "PA"), // 2 /
		mixedAsstCall("c1"),
		mixedToolResult("c1", "FLAT", set, 4096),
	}}
}

// TestMixedAllocLaneOrder pins retention priority plain > chains > history.
// Evaluating lane 2 before lane 0 keeps every total plausible, so the rows
// below are chosen so the two orders disagree on WHICH subject loses.
func TestMixedAllocLaneOrder(t *testing.T) {
	tests := []struct {
		name           string
		budget         int
		wantUsed       int
		wantEvicted    int
		wantChainGone  bool
		wantPlain      string // decision, or omission reason when omitted
		wantHistory    string
		wantAnchorDec  string
		wantAnchorText string
	}{{
		// 3 + plain 4 + chain 56 + anchor delta 14 = 77; history's 4 does not
		// fit. Lane 2 first would admit history and omit the anchor content.
		name:   "tight budget drops history, keeps plain and the anchor",
		budget: 77, wantUsed: 77, wantEvicted: 1,
		wantPlain: DecisionBase, wantHistory: OmitTokenBudget,
		wantAnchorDec: DecisionBase, wantAnchorText: strings.Repeat("A", 40),
	}, {
		// Only one span fits at all. Lane 2 first would keep history and drop
		// the plain exchange.
		name:   "span-sized budget keeps plain, evicts the chain, drops history",
		budget: 7, wantUsed: 7, wantEvicted: 2, wantChainGone: true,
		wantPlain: DecisionBase, wantHistory: OmitTokenBudget,
		wantAnchorDec: OmitChainEvicted, wantAnchorText: omittedObservation,
	}, {
		// The plain-vs-CHAIN discrimination, which the rows above miss: 59 fits
		// the 4-token plain span or the 56-token chain base, not both. Plain wins
		// and the chain is evicted; swapping lanes 0 and 1 would charge the chain
		// first (3+56 = 59, exactly) and omit the plain exchange instead.
		name:   "budget for one of plain-or-chain keeps plain",
		budget: 59, wantUsed: 11, wantEvicted: 1, wantChainGone: true,
		wantPlain: DecisionBase, wantHistory: DecisionBase,
		wantAnchorDec: OmitChainEvicted, wantAnchorText: omittedObservation,
	}, {
		name:   "roomy budget admits all three lanes",
		budget: 81, wantUsed: 81, wantEvicted: 0,
		wantPlain: DecisionBase, wantHistory: DecisionBase,
		wantAnchorDec: DecisionBase, wantAnchorText: strings.Repeat("A", 40),
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, alloc, err := allocFixture(t, runeEstimator, laneOrderState(), tc.budget, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units)
			if alloc.used != tc.wantUsed {
				t.Errorf("used = %d, want %d\n%s", alloc.used, tc.wantUsed, allocSnapshot(units, alloc))
			}
			if alloc.evictedGroups != tc.wantEvicted {
				t.Errorf("evictedGroups = %d, want %d", alloc.evictedGroups, tc.wantEvicted)
			}
			plain := findSubject(t, units, contextdepth.DomainConversation, "3")
			if got := outcome(plain); got != tc.wantPlain {
				t.Errorf("plain exchange outcome = %q, want %q", got, tc.wantPlain)
			}
			hist := findSubject(t, units, contextdepth.DomainConversation, "0")
			if got := outcome(hist); got != tc.wantHistory {
				t.Errorf("history exchange outcome = %q, want %q", got, tc.wantHistory)
			}
			anchorSubject := findSubject(t, units, contextdepth.DomainRAG, "pkg/one.go")
			if got := outcome(anchorSubject); got != tc.wantAnchorDec {
				t.Errorf("anchor subject outcome = %q, want %q", got, tc.wantAnchorDec)
			}
			chain := units[3]
			if chain.kind != unitChain {
				t.Fatalf("unit 3 kind = %d, want unitChain", chain.kind)
			}
			if chain.evicted != tc.wantChainGone {
				t.Errorf("chain evicted = %v, want %v", chain.evicted, tc.wantChainGone)
			}
			if got := chain.anchors[0].content; got != tc.wantAnchorText {
				t.Errorf("anchor content = %q, want %q", got, tc.wantAnchorText)
			}
		})
	}
}

// outcome collapses a subject to the single string the tables compare against:
// its omission reason when omitted, its decision otherwise.
func outcome(s *mixedSubject) string {
	if s.omitted {
		return s.reason
	}
	return s.decision
}

// TestMixedAllocChainEviction: a chain whose base footprint cannot fit is
// evicted whole — every anchor subject AND the chain-level subject omitted with
// chain_evicted — and the allocator keeps going, so a later lane-2 span is
// still considered.
func TestMixedAllocChainEviction(t *testing.T) {
	set := chainSet(0, ladderGroup("pkg/one.go", 1,
		strings.Repeat("A", 40), strings.Repeat("O", 30), strings.Repeat("E", 20)))
	st := State{Messages: []Message{
		history("user", "HQ"),      // 2 \ lane 2, considered AFTER the chain
		history("assistant", "HA"), // 2 /
		pinned("system", "SYS"),    // 3
		mixedAsstCall("c1"),
		mixedToolResult("c1", "FLAT", set, 4096), // base 30 + placeholder 26 = 56
	}}

	// 3 must-fit + 4 history = 7; the chain's 56 never fits.
	units, alloc, err := allocFixture(t, runeEstimator, st, 17, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	if alloc.used != 7 {
		t.Errorf("used = %d, want 7 (pinned + history only)\n%s", alloc.used, allocSnapshot(units, alloc))
	}
	if alloc.evictedGroups != 1 {
		t.Errorf("evictedGroups = %d, want 1", alloc.evictedGroups)
	}
	if alloc.verbatimShortfalls != 0 {
		t.Errorf("verbatimShortfalls = %d, want 0 (MinVerbatim is 0 here)", alloc.verbatimShortfalls)
	}
	chain := units[2]
	if chain.kind != unitChain || !chain.evicted {
		t.Fatalf("unit 2 kind = %d evicted = %v, want an evicted unitChain", chain.kind, chain.evicted)
	}
	for _, s := range unitSubjects(chain) {
		if !s.omitted || s.reason != OmitChainEvicted {
			t.Errorf("chain subject %s/%s: omitted = %v reason = %q, want true %q",
				s.ref.Domain, s.ref.ID, s.omitted, s.reason, OmitChainEvicted)
		}
	}
	if chain.subject == nil {
		t.Fatal("chain has no chain-level subject to omit")
	}
	hist := findSubject(t, units, contextdepth.DomainConversation, "0")
	if hist.omitted || hist.decision != DecisionBase {
		t.Errorf("history subject = %+v, want retained at base: the allocator must keep going after an eviction", hist)
	}
}

// exactAccountingState is the widest fixture: both must-fit kinds, both span
// lanes, a two-anchor structured chain with a verbatim floor, and an
// unstructured chain. Every cost path the ledger touches is present.
func exactAccountingState() State {
	setA := chainSet(2,
		ladderGroup("pkg/a.go", 1, strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 10), strings.Repeat("2", 10)),
		ladderGroup("pkg/b.go", 2, strings.Repeat("B", 10), strings.Repeat("b", 20), strings.Repeat("3", 10)),
		ladderGroup("pkg/c.go", 3, strings.Repeat("C", 10), strings.Repeat("c", 20), strings.Repeat("4", 10)),
	)
	// setB's orientation alternatives are DEARER than the omission placeholder
	// (60 bytes vs 26), which is the only way an anchor can be retained while
	// every one of its subjects is omitted: a cheaper-than-placeholder
	// alternative always fits, because replacing the placeholder with it is a
	// negative delta.
	setB := chainSet(0,
		ladderGroup("mem/1", 1, strings.Repeat("M", 60), strings.Repeat("m", 25), strings.Repeat("5", 30)),
		ladderGroup("mem/2", 2, strings.Repeat("N", 60), strings.Repeat("n", 25), strings.Repeat("6", 30)),
	)
	return State{Messages: []Message{
		history("user", "HIST-Q"),
		history("assistant", "HIST-A"),
		pinned("system", "SYS-PROMPT"),
		elastic("user", "PLAIN-Q"),
		elastic("assistant", "PLAIN-A"),
		mixedAsstCall("c1", "c2"),
		mixedToolResult("c1", "FLAT-1", setA, 4096),
		mixedToolResult("c2", "FLAT-2", setB, 4096),
		mixedAsstCall("c3"),
		mixedToolResult("c3", "PLAIN-RESULT", nil, 0),
		mixedAsstCall("c4"), // unresolved tail
	}}
}

// TestMixedAllocExactAccounting is the exact-delta identity: after allocation
// the running ledger EQUALS a fresh recompute of the materialized content, and
// every anchor's cached token count equals a fresh estimate of its own joined
// content. It runs under a non-additive estimator precisely because an
// implementation that charges est(fragment) per admitted alternative — instead
// of estimate(newJoined) - estimate(oldJoined) — can still keep its books
// balanced when the estimator happens to be additive.
func TestMixedAllocExactAccounting(t *testing.T) {
	estimators := []struct {
		name string
		est  func(string) int
	}{
		{"character estimator", runeEstimator},
		{"non-additive estimator", nonAdditiveEstimator},
	}
	// The sweep has to cross every regime the ledger can be wrong about: chains
	// evicted, a chain retained whose anchors cannot afford any content, floors
	// met and unmet, and upgrades. The reachability probe below fails if it does
	// not, so a future edit cannot quietly narrow it.
	budgets := []int{120, 180, 195, 260, 400, 900}

	for _, e := range estimators {
		t.Run(e.name, func(t *testing.T) {
			m := ContextManager{Estimate: guardedEstimate(e.est)}
			est := m.estimate
			seen := map[string]int{}
			placeholders := 0
			for _, budget := range budgets {
				t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
					units, alloc, err := allocFixture(t, e.est, exactAccountingState(), budget, 11)
					if err != nil {
						t.Fatalf("allocateMixed(budget %d): %v", budget, err)
					}
					assertDecided(t, units)
					for _, u := range units {
						if u.kind != unitChain || u.evicted {
							continue
						}
						for _, a := range u.anchors {
							if want := anchorJoined(a); a.content != want {
								t.Errorf("anchor %s: content = %q, want the joined selection %q", a.callID, a.content, want)
							}
							if want := est(a.content); a.tokens != want {
								t.Errorf("anchor %s: tokens = %d, want %d = a fresh estimate of its own content",
									a.callID, a.tokens, want)
							}
							if a.content == omittedObservation {
								placeholders++
							}
						}
					}
					if want := ledgerRecompute(m, units, 11); alloc.used != want {
						t.Errorf("used = %d, want %d (independent recompute)\n%s",
							alloc.used, want, allocSnapshot(units, alloc))
					}
					for _, u := range units {
						for _, s := range unitSubjects(u) {
							seen[outcome(s)]++
						}
					}
				})
			}
			// Reachability probe: the sweep is worthless if it never produced a
			// floor, an upgrade or an omission for the identity to be wrong about.
			for _, want := range []string{DecisionBase, DecisionFloor, DecisionUpgrade, OmitTokenBudget, OmitChainEvicted} {
				if seen[want] == 0 {
					t.Errorf("budget sweep never produced outcome %q: %v", want, seen)
				}
			}
			if placeholders == 0 {
				t.Errorf("budget sweep never retained an anchor holding only the placeholder")
			}
		})
	}
}

// TestMixedAllocPlaceholderPrecharge: every structured anchor starts with the
// omission placeholder already charged, so the model-visible bytes of a retained
// but fully-omitted anchor are never free — and an OutputCap too small to hold
// even the placeholder evicts the chain rather than shipping an over-cap
// message.
func TestMixedAllocPlaceholderPrecharge(t *testing.T) {
	set := chainSet(0, ladderGroup("pkg/one.go", 1,
		strings.Repeat("A", 40), strings.Repeat("O", 30), strings.Repeat("E", 20)))

	t.Run("retained anchor keeps the precharged placeholder", func(t *testing.T) {
		st := State{Messages: []Message{
			mixedAsstCall("c1"),
			mixedToolResult("c1", "FLAT", set, 4096),
		}}
		// Exactly the chain base: call(20) + envelope(10) + placeholder(26).
		units, alloc, err := allocFixture(t, runeEstimator, st, 56, 0)
		if err != nil {
			t.Fatalf("allocateMixed: %v", err)
		}
		assertDecided(t, units)
		if units[0].evicted {
			t.Fatal("chain evicted: the base footprint must fit at exactly its own cost")
		}
		a := units[0].anchors[0]
		if a.content != omittedObservation {
			t.Errorf("anchor content = %q, want the placeholder %q", a.content, omittedObservation)
		}
		if a.tokens != len(omittedObservation) {
			t.Errorf("anchor tokens = %d, want %d (runeEstimator over the placeholder)", a.tokens, len(omittedObservation))
		}
		if alloc.used != 56 {
			t.Errorf("used = %d, want 56: the placeholder is charged at chain admission\n%s",
				alloc.used, allocSnapshot(units, alloc))
		}
		s := units[0].anchors[0].subjects[0]
		if !s.omitted || s.reason != OmitTokenBudget {
			t.Errorf("subject = %+v, want omitted with %q", s, OmitTokenBudget)
		}
	})

	t.Run("OutputCap below the placeholder evicts the chain", func(t *testing.T) {
		st := State{Messages: []Message{
			pinned("system", "SYS"), // 3, so a zero total cannot masquerade as success
			mixedAsstCall("c1"),
			mixedToolResult("c1", "FLAT", set, len(omittedObservation)-1),
		}}
		units, alloc, err := allocFixture(t, runeEstimator, st, 500, 0)
		if err != nil {
			t.Fatalf("allocateMixed: %v", err)
		}
		assertDecided(t, units)
		chain := units[1]
		if !chain.evicted {
			t.Fatal("chain retained with an OutputCap too small for the placeholder")
		}
		if alloc.used != 3 {
			t.Errorf("used = %d, want 3 (pinned only; an evicted chain is not charged)", alloc.used)
		}
		for _, s := range unitSubjects(chain) {
			if !s.omitted || s.reason != OmitChainEvicted {
				t.Errorf("subject %s/%s = %+v, want omitted with %q", s.ref.Domain, s.ref.ID, s, OmitChainEvicted)
			}
		}
	})
}

// floorState builds one chain whose single anchor carries the given groups and
// MinVerbatim.
func floorState(minVerbatim int, groups ...ContextGroup) State {
	return State{Messages: []Message{
		mixedAsstCall("c1"),
		mixedToolResult("c1", "FLAT", chainSet(minVerbatim, groups...), 4096),
	}}
}

// TestMixedAllocVerbatimFloor: the floor pass admits evidence-bearing
// alternatives, in group order, until the anchor's preferred verbatim-COMPONENT
// count is met; base admission then fills the subjects it never reached.
func TestMixedAllocVerbatimFloor(t *testing.T) {
	// Three sources. A carries two evidence blocks (six alternatives), B and C
	// one each (four). Every alternative content length is a multiple of 10 so
	// the joined lengths below are readable.
	ladderA := ladderGroup("pkg/a.go", 1, strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 10), strings.Repeat("2", 10))
	ladderB := ladderGroup("pkg/b.go", 2, strings.Repeat("B", 10), strings.Repeat("b", 20), strings.Repeat("3", 10))
	ladderC := ladderGroup("pkg/c.go", 3, strings.Repeat("C", 10), strings.Repeat("c", 20), strings.Repeat("4", 10))

	tests := []struct {
		name        string
		st          State
		budget      int
		wantShort   int
		wantVerb    int
		wantOutcome map[string]string
		wantChosen  map[string]int
		wantContent string
	}{{
		// Floor spreads across two subjects: A's cheapest evidence-bearing
		// alternative (index 2) carries one verbatim component, B's another.
		name:      "floor spreads across two subjects, base fills the third",
		st:        floorState(2, ladderA, ladderB, ladderC),
		budget:    82,
		wantShort: 0, wantVerb: 2,
		wantOutcome: map[string]string{"pkg/a.go": DecisionFloor, "pkg/b.go": DecisionFloor, "pkg/c.go": DecisionBase},
		wantChosen:  map[string]int{"pkg/a.go": 2, "pkg/b.go": 2, "pkg/c.go": 0},
		wantContent: strings.Repeat("A", 10) + strings.Repeat("1", 10) + "\n" +
			strings.Repeat("B", 10) + strings.Repeat("3", 10) + "\n" +
			strings.Repeat("C", 10),
	}, {
		// A's cheapest evidence-bearing alternative carries TWO verbatim
		// components, so one admission meets the floor and neither B nor C is
		// reached by the floor pass.
		name:      "one two-verbatim alternative meets the floor alone",
		st:        floorState(2, dropPrefix1(ladderA), ladderB, ladderC),
		budget:    82,
		wantShort: 0, wantVerb: 2,
		wantOutcome: map[string]string{"pkg/a.go": DecisionFloor, "pkg/b.go": DecisionBase, "pkg/c.go": DecisionBase},
		wantChosen:  map[string]int{"pkg/a.go": 2, "pkg/b.go": 0, "pkg/c.go": 0},
		wantContent: strings.Repeat("A", 10) + strings.Repeat("1", 10) + strings.Repeat("2", 10) + "\n" +
			strings.Repeat("B", 10) + "\n" + strings.Repeat("C", 10),
	}, {
		// Evidence blocks too expensive for the leftover budget: the floor goes
		// unmet and is recorded, and base admission still admits what fits.
		name: "unmet floor is recorded and base admission still runs",
		st: floorState(2,
			ladderGroup("pkg/a.go", 1, strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 60)),
			ladderGroup("pkg/b.go", 2, strings.Repeat("B", 10), strings.Repeat("b", 20), strings.Repeat("3", 60)),
			ladderGroup("pkg/c.go", 3, strings.Repeat("C", 10), strings.Repeat("c", 20), strings.Repeat("4", 60)),
		),
		budget:    76,
		wantShort: 1, wantVerb: 0,
		wantOutcome: map[string]string{"pkg/a.go": DecisionBase, "pkg/b.go": DecisionBase, "pkg/c.go": DecisionBase},
		wantChosen:  map[string]int{"pkg/a.go": 0, "pkg/b.go": 0, "pkg/c.go": 0},
		wantContent: strings.Repeat("A", 10) + "\n" + strings.Repeat("B", 10) + "\n" + strings.Repeat("C", 10),
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, alloc, err := allocFixture(t, runeEstimator, tc.st, tc.budget, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units)
			if units[0].evicted {
				t.Fatalf("chain evicted; fixture no longer exercises the floor\n%s", allocSnapshot(units, alloc))
			}
			a := units[0].anchors[0]
			if a.verbatimGot != tc.wantVerb {
				t.Errorf("verbatimGot = %d, want %d", a.verbatimGot, tc.wantVerb)
			}
			if alloc.verbatimShortfalls != tc.wantShort {
				t.Errorf("verbatimShortfalls = %d, want %d", alloc.verbatimShortfalls, tc.wantShort)
			}
			for id, want := range tc.wantOutcome {
				s := findSubject(t, units, contextdepth.DomainRAG, id)
				if got := outcome(s); got != want {
					t.Errorf("%s outcome = %q, want %q\n%s", id, got, want, allocSnapshot(units, alloc))
				}
				if s.chosen != tc.wantChosen[id] {
					t.Errorf("%s chosen = %d, want %d", id, s.chosen, tc.wantChosen[id])
				}
			}
			if a.content != tc.wantContent {
				t.Errorf("anchor content = %q, want %q", a.content, tc.wantContent)
			}
		})
	}
}

// TestMixedAllocFloorNeverOmits: a floor pass that cannot afford any
// evidence-bearing alternative must leave the subject ELIGIBLE, so base
// admission still admits its cheap orientation alternative (spec 4.1 step 4).
// Omitting on a floor non-fit would drop a subject that comfortably fits.
func TestMixedAllocFloorNeverOmits(t *testing.T) {
	// One subject, MinVerbatim 1. Its orientation alternative costs 10; every
	// evidence-bearing one costs 70+, which the budget cannot reach.
	st := floorState(1, ladderGroup("pkg/a.go", 1,
		strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 60)))

	// Exactly the chain base: 30 + placeholder 26 = 56. Admitting the 10-byte
	// orientation alternative REPLACES the 26-byte placeholder, a delta of -16,
	// so 16 tokens come free — still short of the 20 an orientation upgrade
	// costs and far short of the 60 the cheapest evidence rung costs.
	units, alloc, err := allocFixture(t, runeEstimator, st, 56, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	a := units[0].anchors[0]
	s := a.subjects[0]
	if s.omitted {
		t.Fatalf("subject omitted by the floor pass: %+v — a floor non-fit must leave it eligible", s)
	}
	if s.decision != DecisionBase || s.chosen != 0 {
		t.Errorf("subject decision = %q chosen = %d, want %q 0 (orientation, admitted by base)",
			s.decision, s.chosen, DecisionBase)
	}
	if a.content != strings.Repeat("A", 10) {
		t.Errorf("anchor content = %q, want the orientation alternative", a.content)
	}
	if alloc.verbatimShortfalls != 1 {
		t.Errorf("verbatimShortfalls = %d, want 1", alloc.verbatimShortfalls)
	}
	if alloc.used != 40 {
		t.Errorf("used = %d, want 40 (56 chain base, less the 16-token placeholder replacement)\n%s",
			alloc.used, allocSnapshot(units, alloc))
	}
	// Fixture self-check: the floor really could not be met at this budget.
	if got := len(a.subjects[0].alts); got != 4 {
		t.Fatalf("ladder has %d alternatives, want 4", got)
	}
}

// TestMixedAllocByteCap: the anchor byte cap bounds final model-visible Content,
// independently of the token budget. With room to spare on tokens, a cap that
// admits only the orientation alternative must keep the subject at orientation;
// a cap below every alternative omits it with byte_cap.
func TestMixedAllocByteCap(t *testing.T) {
	// Alternative lengths: 40, 70, 60, 90.
	group := ladderGroup("pkg/one.go", 1, strings.Repeat("A", 40), strings.Repeat("O", 30), strings.Repeat("E", 20))

	tests := []struct {
		name        string
		cap         int
		wantOutcome string
		wantChosen  int
		wantContent string
	}{{
		name: "cap admits orientation only: no upgrade, no omission",
		cap:  45, wantOutcome: DecisionBase, wantChosen: 0,
		wantContent: strings.Repeat("A", 40),
	}, {
		name: "roomy cap upgrades to the deepest alternative",
		cap:  4096, wantOutcome: DecisionUpgrade, wantChosen: 3,
		wantContent: strings.Repeat("A", 40) + strings.Repeat("O", 30) + strings.Repeat("E", 20),
	}, {
		name: "cap below every alternative omits with byte_cap",
		cap:  len(omittedObservation) + 4, wantOutcome: OmitByteCap, wantChosen: -1,
		wantContent: omittedObservation,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := State{Messages: []Message{
				mixedAsstCall("c1"),
				mixedToolResult("c1", "FLAT", chainSet(0, group), tc.cap),
			}}
			// 500 is far above every alternative's cost: only the cap can bind.
			units, alloc, err := allocFixture(t, runeEstimator, st, 500, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units)
			if units[0].evicted {
				t.Fatalf("chain evicted at cap %d; the placeholder fits", tc.cap)
			}
			a := units[0].anchors[0]
			s := a.subjects[0]
			if got := outcome(s); got != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q\n%s", got, tc.wantOutcome, allocSnapshot(units, alloc))
			}
			if s.chosen != tc.wantChosen {
				t.Errorf("chosen = %d, want %d", s.chosen, tc.wantChosen)
			}
			if a.content != tc.wantContent {
				t.Errorf("anchor content = %q, want %q", a.content, tc.wantContent)
			}
			if len(a.content) > a.cap {
				t.Errorf("anchor content is %d bytes, over its %d-byte cap", len(a.content), a.cap)
			}
		})
	}
}

// jumpLadder: alternative lengths 40, 100, 50, 110, 60, 120; verbatim counts
// 0,0,1,1,2,2. The orientation rung at index 1 is DEARER than the evidence rung
// at index 2 — the non-monotonic cost rag really produces (its metadata,
// abstract and abstract+overview rungs interleave with the evidence prefixes),
// and the reason declaration order is utility order rather than cost order.
// Two rungs rather than rag's three: this fixture exists for the jump, and a
// third would only lengthen it.
func jumpLadder() ContextGroup {
	return ladderGroup("pkg/one.go", 1, strings.Repeat("A", 40), strings.Repeat("O", 60),
		strings.Repeat("E", 10), strings.Repeat("F", 10))
}

// TestMixedAllocUpgradesJump: an upgrade considers every later alternative, not
// just the next rung. The leftover budget fits [abstract ev1] (index 2) but not
// the intervening orientation upgrade [abstract overview] (index 1), so an
// adjacent-step-only pass stalls at index 0 and ships no evidence at all.
//
// The second row also pins verbatimGot across a REPLACEMENT: upgrading a subject
// from a 1-verbatim to a 2-verbatim alternative must revert the count it already
// contributed, giving 2 rather than 3. Nothing in Task 7 reads verbatimGot after
// the upgrade phase, but Task 8's trace and Task 11's properties will.
func TestMixedAllocUpgradesJump(t *testing.T) {
	tests := []struct {
		name        string
		budget      int
		wantChosen  int
		wantUsed    int
		wantVerb    int
		wantContent string
	}{{
		// chain base 30 + placeholder 26 = 56; base admission of index 0 costs
		// +14 (40 - 26) => 70 used, 15 free. Index 2 costs +10, index 1 costs
		// +60 and index 4 costs +20, so exactly one jump is affordable.
		name:   "one jump past the dearer orientation rung",
		budget: 85, wantChosen: 2, wantUsed: 80, wantVerb: 1,
		wantContent: strings.Repeat("A", 40) + strings.Repeat("E", 10),
	}, {
		// 25 free: index 2 (+10) then index 4 (+10 more), the second one
		// REPLACING a 1-verbatim choice with a 2-verbatim one.
		name:   "second jump replaces a verbatim choice",
		budget: 95, wantChosen: 4, wantUsed: 90, wantVerb: 2,
		wantContent: strings.Repeat("A", 40) + strings.Repeat("E", 10) + strings.Repeat("F", 10),
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := State{Messages: []Message{
				mixedAsstCall("c1"),
				mixedToolResult("c1", "FLAT", chainSet(0, jumpLadder()), 4096),
			}}
			units, alloc, err := allocFixture(t, runeEstimator, st, tc.budget, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units)
			a := units[0].anchors[0]
			s := a.subjects[0]

			// Fixture self-check: the adjacent rung must be UNAFFORDABLE, or the
			// test cannot distinguish a jump from a step.
			if len(s.alts) != 6 {
				t.Fatalf("ladder has %d alternatives, want the real six-rung shape", len(s.alts))
			}
			free := tc.budget - 70
			if adjacent := len(s.alts[1].Content) - len(s.alts[0].Content); adjacent <= free {
				t.Fatalf("adjacent upgrade costs %d and fits in the %d free tokens; the fixture no longer traps a step-only pass",
					adjacent, free)
			}

			if s.chosen != tc.wantChosen {
				t.Errorf("chosen = %d, want %d: the upgrade must jump past the unaffordable orientation rung",
					s.chosen, tc.wantChosen)
			}
			if s.decision != DecisionUpgrade {
				t.Errorf("decision = %q, want %q", s.decision, DecisionUpgrade)
			}
			if a.content != tc.wantContent {
				t.Errorf("anchor content = %q, want %q", a.content, tc.wantContent)
			}
			if a.verbatimGot != tc.wantVerb {
				t.Errorf("verbatimGot = %d, want %d (a replacement must revert the count it superseded)",
					a.verbatimGot, tc.wantVerb)
			}
			if alloc.used != tc.wantUsed {
				t.Errorf("used = %d, want %d\n%s", alloc.used, tc.wantUsed, allocSnapshot(units, alloc))
			}
		})
	}
}

// TestMixedAllocUpgradesCancelled: a cancelled context stops the upgrade phase
// and leaves the base allocation intact. Cancellation must degrade, never
// corrupt — the same fixture that upgrades twice under a live context must still
// satisfy D6 and the ledger identity with no upgrades at all.
func TestMixedAllocUpgradesCancelled(t *testing.T) {
	newState := func() State {
		return State{Messages: []Message{
			mixedAsstCall("c1"),
			mixedToolResult("c1", "FLAT", chainSet(0, jumpLadder()), 4096),
		}}
	}
	// Shared by the cancelled run and the control below: if only one of them
	// changed, the control would re-run at its own budget and every assertion
	// here would still hold while nothing about cancellation was tested.
	const budget = 95

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m := ContextManager{Estimate: guardedEstimate(runeEstimator)}
	units, alloc, err := allocFixtureCtx(t, ctx, runeEstimator, newState(), budget, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	a := units[0].anchors[0]
	s := a.subjects[0]
	if s.chosen != 0 || s.decision != DecisionBase {
		t.Errorf("chosen = %d decision = %q, want 0 %q: a cancelled context must skip upgrades",
			s.chosen, s.decision, DecisionBase)
	}
	if want := strings.Repeat("A", 40); a.content != want {
		t.Errorf("anchor content = %q, want the base rung %q", a.content, want)
	}
	if alloc.used != 70 {
		t.Errorf("used = %d, want 70 (chain base 56, less the 26-token placeholder, plus the 40-token base rung)", alloc.used)
	}
	// Still internally consistent: the ledger identity and a.tokens hold.
	if want := ledgerRecompute(m, units, 0); alloc.used != want {
		t.Errorf("used = %d, want %d (independent recompute)\n%s", alloc.used, want, allocSnapshot(units, alloc))
	}
	if want := m.estimate(a.content); a.tokens != want {
		t.Errorf("anchor tokens = %d, want %d", a.tokens, want)
	}
	// Control: the same fixture and budget upgrade twice when not cancelled, so
	// the assertions above are about cancellation and not about the budget.
	liveUnits, liveAlloc, err := allocFixture(t, runeEstimator, newState(), budget, 0)
	if err != nil {
		t.Fatalf("allocateMixed (live): %v", err)
	}
	if got := liveUnits[0].anchors[0].subjects[0].chosen; got != 4 {
		t.Fatalf("live run chose %d, want 4; the fixture no longer distinguishes cancellation\n%s",
			got, allocSnapshot(liveUnits, liveAlloc))
	}
}

// twoAnchorState builds one chain answered by two structured results. A policy
// that only matters ACROSS anchors — upgrade priority, the tie-break direction,
// the selection-phase cap check — needs two of them to be wrong about. Chain
// base under runeEstimator: two calls (40) + two envelopes (20) = 60, plus two
// 26-token placeholders = 112.
func twoAnchorState(capA, capB int, groupA, groupB ContextGroup) State {
	return State{Messages: []Message{
		mixedAsstCall("c1", "c2"),
		mixedToolResult("c1", "F", chainSet(0, groupA), capA),
		mixedToolResult("c2", "F", chainSet(0, groupB), capB),
	}}
}

// upgradeCost is the exact marginal token cost of moving s from alternative
// from to alternative to. With one subject per anchor the joined content IS that
// alternative, so under runeEstimator this is a byte delta — which is what lets
// the fixture self-checks below state the candidate ordering in the test's own
// terms. from is explicit because these checks run AFTER allocation, when
// s.chosen has already moved off the base rung the candidates were ranked from.
func upgradeCost(s *mixedSubject, from, to int) int {
	return len(s.alts[to].Content) - len(s.alts[from].Content)
}

// TestMixedAllocUpgradesEvidenceBeforeCost: evidence-adding candidates are
// considered before cheaper orientation ones (spec 4.1 step 6). The priority is
// only observable ACROSS subjects — within one subject the exact deltas make the
// two passes converge on the same alternative — so this needs two anchors whose
// cheapest candidate and whose evidence candidate belong to different ones.
func TestMixedAllocUpgradesEvidenceBeforeCost(t *testing.T) {
	// A: base 40, evidence rung 60 (+20), orientation rung 140 (unaffordable).
	// B: base 40, orientation rung 45 (+5), evidence rung 90 (unaffordable).
	st := twoAnchorState(4096, 4096,
		ladderGroup("pkg/a.go", 1, strings.Repeat("A", 40), strings.Repeat("o", 100), strings.Repeat("E", 20)),
		ladderGroup("pkg/b.go", 2, strings.Repeat("B", 40), strings.Repeat("p", 5), strings.Repeat("F", 50)),
	)
	// 112 base + two base admissions (+14 each) = 140; 160 leaves exactly 20 —
	// A's evidence upgrade, or B's orientation upgrade with change to spare.
	units, alloc, err := allocFixture(t, runeEstimator, st, 160, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	subA := findSubject(t, units, contextdepth.DomainRAG, "pkg/a.go")
	subB := findSubject(t, units, contextdepth.DomainRAG, "pkg/b.go")

	// Fixture self-check: B's orientation upgrade must be strictly CHEAPER than
	// A's evidence upgrade, or evidence-first and cheapest-first agree and the
	// test proves nothing.
	if orCost, evCost := upgradeCost(subB, 0, 1), upgradeCost(subA, 0, 2); orCost >= evCost {
		t.Fatalf("B's orientation upgrade costs %d, not less than A's evidence upgrade %d; the fixture cannot separate the two passes",
			orCost, evCost)
	}
	if subA.chosen != 2 || subA.decision != DecisionUpgrade {
		t.Errorf("subject A: chosen = %d decision = %q, want 2 %q — the leftover budget must buy EVIDENCE first",
			subA.chosen, subA.decision, DecisionUpgrade)
	}
	if subB.chosen != 0 || subB.decision != DecisionBase {
		t.Errorf("subject B: chosen = %d decision = %q, want 0 %q — its cheaper orientation upgrade must not preempt A's evidence",
			subB.chosen, subB.decision, DecisionBase)
	}
	if alloc.used != 160 {
		t.Errorf("used = %d, want 160\n%s", alloc.used, allocSnapshot(units, alloc))
	}
}

// TestMixedAllocUpgradeCapDoesNotStarve: the byte cap is re-checked during
// candidate SELECTION, not only at commit. An over-cap candidate that wins
// selection on cost fails at commit, upgradeOnce reports no progress, and the
// whole upgrade phase halts with affordable upgrades elsewhere unbought — so
// dropping the selection-phase check starves the OTHER anchor rather than
// shipping anything over cap.
func TestMixedAllocUpgradeCapDoesNotStarve(t *testing.T) {
	// A's cap (42) admits its 40-byte base rung and its 26-byte placeholder, but
	// not the 45-byte next rung whose +5 is the cheapest candidate in the scan.
	// B's cap is roomy and its next rung costs +10.
	st := twoAnchorState(42, 4096,
		ladderGroup("pkg/a.go", 1, strings.Repeat("A", 40), strings.Repeat("o", 5), strings.Repeat("E", 60)),
		ladderGroup("pkg/b.go", 2, strings.Repeat("B", 40), strings.Repeat("p", 10), strings.Repeat("F", 60)),
	)
	// 112 base + two base admissions = 140; 152 leaves 12 — enough for B's +10.
	units, alloc, err := allocFixture(t, runeEstimator, st, 152, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	if units[0].evicted {
		t.Fatalf("chain evicted; both caps hold the placeholder\n%s", allocSnapshot(units, alloc))
	}
	subA := findSubject(t, units, contextdepth.DomainRAG, "pkg/a.go")
	subB := findSubject(t, units, contextdepth.DomainRAG, "pkg/b.go")

	// Fixture self-checks: A's next rung must be over ITS cap and cheaper than
	// B's, or it never wins selection and cannot starve anything.
	capA := units[0].anchors[0].cap
	if len(subA.alts[1].Content) <= capA {
		t.Fatalf("A's next rung is %d bytes, within its %d-byte cap; nothing would be rejected",
			len(subA.alts[1].Content), capA)
	}
	if a1, b1 := upgradeCost(subA, 0, 1), upgradeCost(subB, 0, 1); a1 >= b1 {
		t.Fatalf("A's over-cap candidate costs %d, not less than B's %d; it would not win selection", a1, b1)
	}
	if subB.chosen != 1 || subB.decision != DecisionUpgrade {
		t.Errorf("subject B: chosen = %d decision = %q, want 1 %q — an over-cap candidate elsewhere must not halt the upgrade phase",
			subB.chosen, subB.decision, DecisionUpgrade)
	}
	if subA.chosen != 0 || subA.decision != DecisionBase {
		t.Errorf("subject A: chosen = %d decision = %q, want 0 %q", subA.chosen, subA.decision, DecisionBase)
	}
	if alloc.used != 150 {
		t.Errorf("used = %d, want 150\n%s", alloc.used, allocSnapshot(units, alloc))
	}
	for _, a := range units[0].anchors {
		if len(a.content) > a.cap {
			t.Errorf("anchor %s: %d bytes over its %d-byte cap", a.callID, len(a.content), a.cap)
		}
	}
}

// TestMixedAllocUpgradeTieKeepsInputOrder: equal marginal costs are broken by
// stable input order (spec 4.1 step 6), so the EARLIER subject wins. A <= in the
// comparison is still deterministic — which is exactly why the determinism test
// cannot see it — but it hands every tie to the last subject scanned.
func TestMixedAllocUpgradeTieKeepsInputOrder(t *testing.T) {
	// Byte-identical ladders under different subject IDs: 40, 50, 100, 110. Both
	// next rungs therefore cost exactly +10.
	abs, ov, ev := strings.Repeat("X", 40), strings.Repeat("y", 10), strings.Repeat("Z", 60)
	st := twoAnchorState(4096, 4096,
		ladderGroup("pkg/a.go", 1, abs, ov, ev),
		ladderGroup("pkg/b.go", 2, abs, ov, ev),
	)
	// 112 base + two base admissions = 140; 150 affords exactly one +10 upgrade.
	units, alloc, err := allocFixture(t, runeEstimator, st, 150, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	subA := findSubject(t, units, contextdepth.DomainRAG, "pkg/a.go")
	subB := findSubject(t, units, contextdepth.DomainRAG, "pkg/b.go")

	// Fixture self-check: the tie must be exact, or the winner is decided by cost
	// and the tie-break direction is untested.
	if a1, b1 := upgradeCost(subA, 0, 1), upgradeCost(subB, 0, 1); a1 != b1 {
		t.Fatalf("upgrade costs are %d and %d; the fixture no longer ties", a1, b1)
	}
	if subA.chosen != 1 || subA.decision != DecisionUpgrade {
		t.Errorf("subject A: chosen = %d decision = %q, want 1 %q — a tie must go to the earlier subject",
			subA.chosen, subA.decision, DecisionUpgrade)
	}
	if subB.chosen != 0 || subB.decision != DecisionBase {
		t.Errorf("subject B: chosen = %d decision = %q, want 0 %q — the later subject must not take the tie",
			subB.chosen, subB.decision, DecisionBase)
	}
	if alloc.used != 150 {
		t.Errorf("used = %d, want 150\n%s", alloc.used, allocSnapshot(units, alloc))
	}
}

// TestMixedAllocShortfallIncludesEvicted: VerbatimShortfalls counts an anchor
// whose preference went unmet whether or not its chain survived (spec 3.4), and
// counts only anchors that expressed a preference.
func TestMixedAllocShortfallIncludesEvicted(t *testing.T) {
	// The MinVerbatim=2 anchor's cheapest evidence-bearing alternative carries
	// two verbatim components, so a retained chain meets the floor outright.
	setWithFloor := chainSet(2, dropPrefix1(ladderGroup("pkg/a.go", 1,
		strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 10), strings.Repeat("2", 10))))
	setNoFloor := chainSet(0, ladderGroup("pkg/b.go", 1,
		strings.Repeat("B", 10), strings.Repeat("b", 20), strings.Repeat("3", 10)))
	newState := func() State {
		return State{Messages: []Message{
			mixedAsstCall("c1", "c2"), // 40
			mixedToolResult("c1", "F", setWithFloor, 4096),
			mixedToolResult("c2", "F", setNoFloor, 4096),
		}}
	}

	tests := []struct {
		name        string
		budget      int
		wantEvicted bool
		wantShort   int
	}{
		// base 40 + 10 + 10 = 60, plus two placeholders (26 each) = 112.
		{"evicted chain still counts its unmet floor", 111, true, 1},
		{"retained chain meets the floor", 300, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, alloc, err := allocFixture(t, runeEstimator, newState(), tc.budget, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units)
			if units[0].evicted != tc.wantEvicted {
				t.Fatalf("evicted = %v, want %v\n%s", units[0].evicted, tc.wantEvicted, allocSnapshot(units, alloc))
			}
			if alloc.verbatimShortfalls != tc.wantShort {
				t.Errorf("verbatimShortfalls = %d, want %d (only the MinVerbatim>0 anchor may count)\n%s",
					alloc.verbatimShortfalls, tc.wantShort, allocSnapshot(units, alloc))
			}
			if len(units[0].anchors) != 2 {
				t.Fatalf("chain has %d anchors, want 2", len(units[0].anchors))
			}
		})
	}
}

// TestMixedAllocChainSubjectDecided: an UNSTRUCTURED completed chain — any tool
// that projects no ContextSet — has no anchors, so its only trace row is the
// chain-level subject. The allocator must decide it on both paths, or spec
// 3.4's D6 invariant fails two tasks downstream with its cause buried here.
func TestMixedAllocChainSubjectDecided(t *testing.T) {
	newState := func() State {
		return State{Messages: []Message{
			mixedAsstCall("c1"),                           // 20
			mixedToolResult("c1", "PLAIN-RESULT", nil, 0), // 12 + 8 + 2 = 22
		}}
	}

	tests := []struct {
		name        string
		budget      int
		wantEvicted bool
		wantOutcome string
		wantUsed    int
	}{
		{"retained chain subject decides base", 42, false, DecisionBase, 42},
		{"evicted chain subject is omitted", 41, true, OmitChainEvicted, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, alloc, err := allocFixture(t, runeEstimator, newState(), tc.budget, 0)
			if err != nil {
				t.Fatalf("allocateMixed: %v", err)
			}
			assertDecided(t, units) // the D6 check the two chain-subject lines exist for
			u := units[0]
			if u.kind != unitChain || len(u.anchors) != 0 {
				t.Fatalf("fixture is not an unstructured chain: kind %d, %d anchors", u.kind, len(u.anchors))
			}
			if u.evicted != tc.wantEvicted {
				t.Fatalf("evicted = %v, want %v", u.evicted, tc.wantEvicted)
			}
			if u.subject == nil {
				t.Fatal("unstructured chain has no chain-level subject: nothing would trace it")
			}
			if got := outcome(u.subject); got != tc.wantOutcome {
				t.Errorf("chain subject outcome = %q, want %q\n%s", got, tc.wantOutcome, allocSnapshot(units, alloc))
			}
			if alloc.used != tc.wantUsed {
				t.Errorf("used = %d, want %d", alloc.used, tc.wantUsed)
			}
		})
	}
}

// TestMixedAllocDeterminism: identical input must yield identical decisions.
// The fixture carries cost TIES (pkg/b.go and pkg/c.go declare byte-identical
// ladders under different IDs) so a tie broken by anything other than input
// order would show up as churn.
func TestMixedAllocDeterminism(t *testing.T) {
	tie := func(id string, rank int) ContextGroup {
		return ladderGroup(id, rank, strings.Repeat("X", 20), strings.Repeat("y", 20), strings.Repeat("9", 20))
	}
	newState := func() State {
		return State{Messages: []Message{
			pinned("system", "SYS-PROMPT"),
			elastic("user", "PLAIN-Q"),
			elastic("assistant", "PLAIN-A"),
			mixedAsstCall("c1"),
			mixedToolResult("c1", "FLAT", chainSet(2,
				ladderGroup("pkg/a.go", 1, strings.Repeat("A", 10), strings.Repeat("a", 20), strings.Repeat("1", 10), strings.Repeat("2", 10)),
				tie("pkg/b.go", 2), tie("pkg/c.go", 3),
			), 4096),
		}}
	}

	var first string
	for i := range 8 {
		units, alloc, err := allocFixture(t, runeEstimator, newState(), 180, 0)
		if err != nil {
			t.Fatalf("run %d: allocateMixed: %v", i, err)
		}
		got := allocSnapshot(units, alloc)
		if i == 0 {
			first = got
			assertDecided(t, units)
			continue
		}
		if got != first {
			t.Fatalf("run %d diverged\n--- first ---\n%s--- run %d ---\n%s", i, first, i, got)
		}
	}
	// The snapshot must actually contain decisions, or "identical" is vacuous.
	for _, want := range []string{DecisionFloor, DecisionBase} {
		if !strings.Contains(first, want) {
			t.Errorf("snapshot carries no %q decision; determinism is vacuous here:\n%s", want, first)
		}
	}
}

// TestMixedAllocZeroCostEstimatorCannotBypassBudget: an estimator that reports
// zero tokens for real text must not buy free context. guardedEstimate falls
// back to the default heuristic on a non-positive result, so the block is
// charged and rejected exactly like any other.
func TestMixedAllocZeroCostEstimatorCannotBypassBudget(t *testing.T) {
	// 400 bytes of content the estimator claims is free. Guarded, it costs
	// (400+3)/4 = 100 tokens — far more than the 0 free tokens below.
	free := strings.Repeat("F", 400)
	group := ContextGroup{
		Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "pkg/free.go"}, Rank: 1},
		Alternatives: []ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract}},
			Content: free,
		}},
	}
	lying := func(s string) int {
		if strings.Contains(s, "F") {
			return 0
		}
		return len([]rune(s))
	}
	st := State{Messages: []Message{
		mixedAsstCall("c1"),
		mixedToolResult("c1", "FLAT", chainSet(0, group), 4096),
	}}

	// Exactly the chain base under the guarded estimator: 30 + 26 = 56.
	units, alloc, err := allocFixture(t, lying, st, 56, 0)
	if err != nil {
		t.Fatalf("allocateMixed: %v", err)
	}
	assertDecided(t, units)
	s := units[0].anchors[0].subjects[0]
	if !s.omitted || s.reason != OmitTokenBudget {
		t.Errorf("subject = %+v, want omitted with %q: a zero-cost claim must not bypass the ceiling", s, OmitTokenBudget)
	}
	if alloc.used != 56 {
		t.Errorf("used = %d, want 56\n%s", alloc.used, allocSnapshot(units, alloc))
	}
	if units[0].anchors[0].content != omittedObservation {
		t.Errorf("anchor content = %q, want the placeholder", units[0].anchors[0].content)
	}
}

// memoState is a multi-anchor, multi-chain fixture: enough subjects that the
// upgrade pass runs many rounds, and enough anchors that most rounds leave most
// anchors untouched — which is the only condition under which upgradeCandidate's
// memo can be wrong and go unnoticed.
//
// outputCap is a parameter because the two stale facts a memo can hold fail
// DIFFERENTLY. A stale dTok only mis-ORDERS upgrades: tryAssign re-prices at
// commit, so the ledger stays exact and the greedy loop usually converges on the
// same terminal state anyway. A stale CAP verdict is the one that changes the
// outcome — an over-cap candidate wins selection, tryAssign refuses it,
// upgradeOnce reports no progress and the whole upgrade phase stops with
// affordable upgrades elsewhere left unbought. Only the tight-cap case below
// detects an invalidation that covers just the committed subject.
func memoState(t *testing.T, outputCap int) State {
	t.Helper()
	var msgs []Message
	for c := range 3 {
		call := fmt.Sprintf("c%d", c)
		var groups []ContextGroup
		for g := range 3 {
			id := fmt.Sprintf("pkg/%d-%d.go", c, g)
			// Deliberately uneven block sizes: equal costs would let a memo that
			// returned the WRONG candidate still land on an equal-cost one.
			groups = append(groups, ladderGroup(id, g+1,
				strings.Repeat("A", 10+g), strings.Repeat("a", 20+c),
				strings.Repeat("1", 10+2*g), strings.Repeat("2", 15+3*c)))
		}
		msgs = append(msgs, mixedAsstCall(call), mixedToolResult(call, "FLAT", chainSet(1, groups...), outputCap))
	}
	return State{Messages: msgs}
}

// clearUpgradeMemo drops every subject's memo, which is what makes the next
// upgradeOnce scan price its candidates from scratch. Calling it before EVERY
// scan reproduces the pre-memo allocator exactly: within one scan each subject
// is visited once, so a memo written during that scan is never read.
func clearUpgradeMemo(units []*mixedUnit) {
	for _, u := range units {
		for _, a := range u.anchors {
			for _, s := range a.subjects {
				s.cands = nil
			}
		}
	}
}

// TestMixedAllocUpgradeMemoMatchesUnmemoized pins upgradeCandidate's per-anchor
// memo as a pure optimization. The reference run reaches the same starting point
// through a SUPPORTED path — a cancelled context yields base admission with no
// upgrades (TestMixedAllocUpgradesCancelled) — and then drives the same two-pass
// upgrade loop allocateMixed runs, clearing the memo before every scan.
//
// The comparison surface is allocSnapshot: the ledger, every anchor's exact
// content and token count, and every subject's chosen index, decision and
// reason. A stale memo can only show up as a different candidate winning some
// round, and every one of those is visible here.
func TestMixedAllocUpgradeMemoMatchesUnmemoized(t *testing.T) {
	const budget = 700
	for _, tc := range []struct {
		name      string
		outputCap int
	}{
		{"budget-bound", 4096},
		// Tight enough that the cap binds part-way through the upgrade phase, so
		// a candidate priced as cap-OK before a sibling grew the anchor becomes
		// cap-violating afterwards.
		{"cap-bound", 150},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memoUnits, memoAlloc, err := allocFixture(t, nonAdditiveEstimator, memoState(t, tc.outputCap), budget, 0)
			if err != nil {
				t.Fatalf("memoized allocateMixed: %v", err)
			}
			assertDecided(t, memoUnits)

			// Vacuity guard: the fixture must actually reach the upgrade phase, on
			// more than one anchor, or "the two runs agree" says nothing about the
			// memo.
			upgraded, anchorsUpgraded := 0, map[string]bool{}
			for _, u := range memoUnits {
				for _, a := range u.anchors {
					for _, s := range a.subjects {
						if s.decision == DecisionUpgrade {
							upgraded++
							anchorsUpgraded[a.callID] = true
						}
					}
				}
			}
			if upgraded < 4 || len(anchorsUpgraded) < 2 {
				t.Fatalf("fixture ran %d upgrades across %d anchors; too few to exercise the memo\n%s",
					upgraded, len(anchorsUpgraded), allocSnapshot(memoUnits, memoAlloc))
			}

			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			refUnits, refAlloc, err := allocFixtureCtx(t, cancelled, nonAdditiveEstimator, memoState(t, tc.outputCap), budget, 0)
			if err != nil {
				t.Fatalf("reference allocateMixed: %v", err)
			}
			m := ContextManager{Estimate: guardedEstimate(nonAdditiveEstimator)}
			for {
				clearUpgradeMemo(refUnits)
				if m.upgradeOnce(context.Background(), refAlloc, refUnits, true) {
					continue
				}
				clearUpgradeMemo(refUnits)
				if m.upgradeOnce(context.Background(), refAlloc, refUnits, false) {
					continue
				}
				break
			}

			if got, want := allocSnapshot(memoUnits, memoAlloc), allocSnapshot(refUnits, refAlloc); got != want {
				t.Errorf("memoized allocation differs from the unmemoized reference\nMEMOIZED:\n%s\nREFERENCE:\n%s", got, want)
			}
		})
	}
}
