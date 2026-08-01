package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// This file is the property/oracle layer for mixed assembly (#331 spec 3.4,
// 4.1, 4.2). The per-task tests pin named scenarios; these pin the rules that
// must hold for EVERY scenario, plus one exhaustive enumeration.
//
// WHY THE EXACT-STRING PINS BELOW EXIST. The trace's ledger identity is
// TAUTOLOGICAL — fillMixedTrace computes EstimatedTokensFree as maxTokens-used,
// so Used+Free==MaxTokens cannot fail — and the independent-recompute form is
// blind to anchor CONTENT, because admission and materialization both call
// anchorJoined: a mutated separator moves both sides together. Measured twice
// (Task 11 and its review): substituting " " for "\n" in anchorJoined leaves
// both the identity and the recompute green (runeEstimator prices "A\nB" and
// "A B" alike), and DROPPING the placeholder precharge likewise leaves the whole
// sweep and oracle green, ledger and cap included. Two arithmetic-invariant
// blind spots, both caught only by literal content pins:
// TestMixedTraceAnchorContentExactPin. The identity assertions are kept — they
// pin the arithmetic wiring — but they are NOT coverage of anchorJoined.
//
// SCOPE OF THAT CLAIM: it is about THIS layer. Module-wide, the separator
// mutation is also caught by TestMixedAllocVerbatimFloor and
// TestAssembleWithTraceMixedEndToEnd, and the precharge mutation by five
// pre-existing per-task tests. What is true is that within the property layer
// nothing else catches either one, which is why the pins live here rather than
// being left to the scenario tests.
//
// NOT COVERED HERE: VerbatimShortfalls carries no property-layer invariant —
// only the == 0 pin in TestMixedTraceFloorTerminalRow. Both directions are
// covered by per-task tests (TestMixedAllocVerbatimFloor,
// TestAssembleWithTraceUpgradedSubject), so this is a known residual, not a gap
// this file silently certifies.

// Content sentinels. They are deliberately unlike every trace vocabulary word,
// subject ID and call ID in this file, so the content-free property can search
// a marshaled trace for them LITERALLY, and its vacuity guard can prove the
// same strings did reach the model-visible state.
const (
	pinFragA = "QQ-ALPHA"
	pinFragB = "ZZ-BETA"
	pinFragC = "WW-GAMMA"

	floorOrient   = "FLR-ORIENT"
	floorEvidence = "FLR-EVIDENCE-BLOCK"

	oracleShortAbs = "QQA"                   // 3 bytes
	oracleShortEv1 = oracleShortAbs + "-EV1" // 7
	oracleShortEv2 = oracleShortEv1 + "-EV2" // 11
	oracleLongAbs  = "ZZB-bbbbbbbbbbbbbbbb"  // 20
	oracleLongEv1  = oracleLongAbs + "-EV1"  // 24
	oracleLongEv2  = oracleLongEv1 + "-EV2"  // 28
	oracleLadderID = "s1.go"                 // shared by every anchor, on purpose
	oracleLongID   = "s2.go"                 // ditto
)

// oneAltGroup is a single-alternative group. With exactly one alternative no
// upgrade can move the choice, so an exact-string pin on the owning anchor is
// stable at every budget that admits the subject at all.
func oneAltGroup(id string, rank int, content string, reps ...contextdepth.RepresentationDesc) ContextGroup {
	return ContextGroup{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: id},
			Rank:    rank,
		},
		Alternatives: []ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: slices.Clone(reps)},
			Content: content,
		}},
	}
}

// floorLadder is a TWO-rung ladder: orientation, then the evidence rung that
// satisfies MinVerbatim — and NOTHING after it. The terminality is the whole
// point: DecisionFloor reaches zero trace rows in every other fixture in this
// package, because the floor pass admits and the upgrade pass then immediately
// replaces the choice, leaving `decision` base or upgrade by the time
// fillMixedTrace reads it (confirmed by sweeping nine fixtures x budgets
// 1-300). With no later rung to buy, the row stays `floor`, which is what makes
// anchorRow's decision copy verifiable for that value.
func floorLadder(id string) ContextGroup {
	return ContextGroup{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: id},
			Rank:    4,
		},
		Alternatives: []ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract}},
			Content: floorOrient,
		}, {
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract, altEvidence}},
			Content: floorEvidence,
			Attrib:  &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "kf", Source: id}}},
		}},
	}
}

// floorTerminalState is one chain whose first subject can only ever be decided
// by the floor pass and whose second is left for base admission, so one trace
// carries both decisions.
func floorTerminalState() State {
	return floorState(1, floorLadder("floor.go"), oneAltGroup("plain.go", 2, pinFragC, altAbstract))
}

// oracleLadder is a three-rung ladder with ascending verbatim components
// (0, 1, 2) — the minimum shape that lets the floor pass, base admission and
// the upgrade pass each settle on a different rung. Evidence rungs carry
// attribution; the orientation rung must not (the carriers reject it).
func oracleLadder(id string, rank int, abs, ev1, ev2 string) ContextGroup {
	attrib := func() *RetrievalAttribution {
		return &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "key-" + id, Source: id}}}
	}
	return ContextGroup{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: id},
			Rank:    rank,
		},
		Alternatives: []ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract}},
			Content: abs,
		}, {
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract, altEvidence}},
			Content: ev1,
			Attrib:  attrib(),
		}, {
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract, altEvidence, altEvidence}},
			Content: ev2,
			Attrib:  attrib(),
		}},
	}
}

// mixedOracleState is the exhaustive oracle's fixture: TWO completed chains,
// each answered by TWO structured anchors, each anchor carrying THE SAME two
// subjects. The repeated subject refs are deliberate — eight anchor rows then
// differ ONLY in ToolCallID, which is what makes the (ToolCallID, Domain, ID)
// uniqueness property falsifiable at all: blanking the call ID collapses eight
// rows into two. The long subject's cheapest rung (20 bytes) also exceeds what
// the tighter caps can hold beside a sibling fragment, so the byte cap bites
// inside the sweep rather than only at chain-eviction size.
//
// Each anchor builds FRESH groups: sharing one ContextGroup value across
// anchors would share its alternative slices, and the non-aliasing property
// below would then be comparing rows against a single backing array.
func mixedOracleState(minVerbatim, outputCap int) State {
	anchor := func(callID string) Message {
		set := chainSet(minVerbatim,
			oracleLadder(oracleLadderID, 1, oracleShortAbs, oracleShortEv1, oracleShortEv2),
			oracleLadder(oracleLongID, 2, oracleLongAbs, oracleLongEv1, oracleLongEv2),
		)
		return mixedToolResult(callID, traceFallback, set, outputCap)
	}
	return State{Messages: []Message{
		mixedAsstCall("c1", "c2"), // 0: chain span subject "0"
		anchor("c1"),              // 1
		anchor("c2"),              // 2
		mixedAsstCall("c3", "c4"), // 3: chain span subject "3"
		anchor("c3"),              // 4
		anchor("c4"),              // 5
	}}
}

// oracleChains lists each chain's call IDs in message order, and
// oracleChainSpans the conversation subject ID of each chain's span — the
// decimal index of its assistant call in the caller-visible State.Messages.
// Both are restated rather than derived from the State, so a materializer that
// dropped a whole chain cannot make the expectation shrink with it.
var (
	oracleChains     = [][]string{{"c1", "c2"}, {"c3", "c4"}}
	oracleChainSpans = []string{"0", "3"}
)

// oracleSentinels is every model-visible string mixedOracleState can render.
func oracleSentinels() []string {
	return []string{
		oracleShortAbs, oracleShortEv1, oracleShortEv2,
		oracleLongAbs, oracleLongEv1, oracleLongEv2,
	}
}

// sysReservation is the only cost mixed assembly charges outside the units: the
// system prompt plus the materialized durable summary (spec 4.1 step 1). It is
// mixedSysTokens generalized over the estimator, because the ledger properties
// here run under the non-additive estimator too. Pinned MESSAGES are units and
// must NOT appear — adding them would double-charge the pinned span.
func sysReservation(est func(string) int, st State) int {
	e := emptyFreeEstimator(est)
	n := e(st.System)
	if summary := strings.TrimSpace(st.DurableSummary); summary != "" {
		n += e(DurableSummaryPrompt(summary))
	}
	return n
}

// emptyFreeEstimator mirrors the ONE part of guardedEstimate's contract these
// estimators can reach: empty text is free. Written out rather than calling
// guardedEstimate, which is production code under test — nonAdditiveEstimator
// prices "" at 7, so an oracle that skipped this would disagree with assembly
// on every blank envelope field.
func emptyFreeEstimator(est func(string) int) func(string) int {
	return func(s string) int {
		if s == "" {
			return 0
		}
		return est(s)
	}
}

// costOracle recomputes an assembled State's model-visible cost from the raw
// estimator INLINE — not through messageCost/totalTokens, which are the
// functions under test — so a drift between the trace's number and the state it
// describes shows up instead of cancelling on both sides.
func costOracle(est func(string) int, out State) int {
	e := emptyFreeEstimator(est)
	n := e(out.System)
	for _, msg := range out.Messages {
		n += e(msg.Content)
		for _, tc := range msg.ToolCalls {
			n += e(tc.ID) + e(tc.Type) + e(tc.Function.Name) + e(string(tc.Function.Arguments))
		}
		n += e(msg.ToolName) + e(msg.ToolCallID)
	}
	return n
}

// assertLedgerExact is assertMixedLedger generalized over the estimator: the
// trace total must equal BOTH the admission ledger (re-derived by re-running
// build+allocate on the same pre-materialization State) and an independent
// recompute of the returned State. The two come from different code — a running
// delta ledger vs a fresh walk of the materialized copy — so an allocator that
// charged per-fragment estimates instead of whole-anchor deltas breaks this
// under the non-additive estimator even though each side stays internally
// consistent.
func assertLedgerExact(t *testing.T, where string, est func(string) int, st, out State, tr ContextAssemblyTrace) {
	t.Helper()
	_, alloc, err := allocFixture(t, est, st, tr.MaxTokens, sysReservation(est, st))
	if err != nil {
		t.Fatalf("%s: ledger oracle: re-running allocation failed: %v", where, err)
	}
	if tr.EstimatedTokensUsed != alloc.used {
		t.Errorf("%s: EstimatedTokensUsed = %d, want the admission ledger %d", where, tr.EstimatedTokensUsed, alloc.used)
	}
	if got := costOracle(est, out); tr.EstimatedTokensUsed != got {
		t.Errorf("%s: EstimatedTokensUsed = %d, but the returned State costs %d", where, tr.EstimatedTokensUsed, got)
	}
	if tr.EstimatedTokensUsed+tr.EstimatedTokensFree != tr.MaxTokens {
		t.Errorf("%s: used %d + free %d != MaxTokens %d", where, tr.EstimatedTokensUsed, tr.EstimatedTokensFree, tr.MaxTokens)
	}
	// TWO-FAULT BACKSTOP with NO single-mutation coverage, kept deliberately.
	// Given the identity above, `Free < 0` is the SAME predicate, so it is not
	// restated. Reaching this needs two faults at once: st.fits must stop
	// bounding alloc.used AND assembleMixed's fail-closed exhaustion gate must
	// be gone — either alone converts the overrun into ErrContextExhausted,
	// which the sweep classifies as the must-fit path instead.
	if tr.EstimatedTokensUsed > tr.MaxTokens {
		t.Errorf("%s: used %d exceeds MaxTokens %d (free %d)", where, tr.EstimatedTokensUsed, tr.MaxTokens, tr.EstimatedTokensFree)
	}
}

// stateSnapshot renders everything a materializer could reach through the
// aliased mixedUnit.msgs span. The marshaled State alone is not enough:
// Message.Context and OutputCap are json:"-" (runtime-only), and the structured
// payload is exactly what an in-place rewrite would corrupt.
func stateSnapshot(t *testing.T, st State) string {
	t.Helper()
	var b strings.Builder
	js, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal State: %v", err)
	}
	b.Write(js)
	for i, msg := range st.Messages {
		set, err := json.Marshal(msg.Context)
		if err != nil {
			t.Fatalf("marshal message %d Context: %v", i, err)
		}
		fmt.Fprintf(&b, "\nmsg %d outputCap=%d context=%s", i, msg.OutputCap, set)
	}
	return b.String()
}

// assertContentFree: a trace carries descriptors, depths, kinds, costs and a
// fixed vocabulary — never rendered block text (spec 3.4).
func assertContentFree(t *testing.T, where, js string, sentinels []string) {
	t.Helper()
	for _, s := range sentinels {
		if strings.Contains(js, s) {
			t.Errorf("%s: trace leaked rendered content %q: %s", where, s, js)
		}
	}
}

// assertNoNullSlices: ContextAssemblyTrace and ContextSubjectTrace hold no
// pointer fields, so a `null` VALUE in the marshaled form is a nil slice —
// Subjects (the orchestrator's zero-trace guard keys on it) or a row's
// Representations. The search is anchored to `":null"` rather than bare "null"
// because subject IDs are source paths: a bare search false-positives on
// pkg/nullable.go, and a check that fails on legitimate data gets deleted.
func assertNoNullSlices(t *testing.T, where, js string) {
	t.Helper()
	if strings.Contains(js, ":null") {
		t.Errorf("%s: marshaled trace contains null; every slice field must marshal []: %s", where, js)
	}
}

// assertRowsUnaliased: no row's Representations may share a backing array with
// the caller's ContextSet or with the package-level implicit descriptors, both
// of which outlive the trace — spanRepresentations and summaryRepresentations
// are shared by every assembly in the process. This covers ALL THREE clone sites
// (anchorRow, spanRow and the summary row in fillMixedTrace), provided the sweep
// includes a fixture with a DurableSummary — it does. Pointer identity is asserted
// directly, so this fails on the aliasing itself rather than by tripping
// validation on some later call.
func assertRowsUnaliased(t *testing.T, where string, st State, tr ContextAssemblyTrace) int {
	t.Helper()
	type owner struct {
		what string
		head *contextdepth.RepresentationDesc
	}
	var forbidden []owner
	add := func(what string, reps []contextdepth.RepresentationDesc) {
		if len(reps) > 0 {
			forbidden = append(forbidden, owner{what, &reps[0]})
		}
	}
	add("agent.spanRepresentations", spanRepresentations)
	add("agent.summaryRepresentations", summaryRepresentations)
	for i, msg := range st.Messages {
		if msg.Context == nil {
			continue
		}
		for gi, g := range msg.Context.Groups {
			for ai, a := range g.Alternatives {
				add(fmt.Sprintf("message %d group %d alternative %d", i, gi, ai), a.Desc.Representations)
			}
		}
	}
	if len(forbidden) < 3 {
		t.Fatalf("%s: only %d shared descriptor slices to compare against; the check would be near-vacuous",
			where, len(forbidden))
	}
	checked := 0
	for ri, row := range tr.Subjects {
		if len(row.Representations) == 0 {
			continue
		}
		checked++
		head := &row.Representations[0]
		for _, f := range forbidden {
			if head == f.head {
				t.Errorf("%s: row %d (%s/%s) Representations aliases %s; a consumer mutating the trace would corrupt it",
					where, ri, row.Subject.Domain, row.Subject.ID, f.what)
			}
		}
	}
	return checked
}

// collectVocabulary records every Decision and OmissionReason seen and rejects
// anything outside the fixed sets (spec 3.4). Decisions and reasons must never
// be payload-derived: a trace whose vocabulary varied with content would be
// unaggregatable.
func collectVocabulary(t *testing.T, where string, tr ContextAssemblyTrace, decisions, reasons map[string]bool) {
	t.Helper()
	for i, row := range tr.Subjects {
		switch row.Decision {
		case DecisionBase, DecisionFloor, DecisionUpgrade, DecisionOmitted:
			decisions[row.Decision] = true
		default:
			t.Errorf("%s: row %d (%s/%s): Decision %q outside the fixed vocabulary",
				where, i, row.Subject.Domain, row.Subject.ID, row.Decision)
		}
		switch row.OmissionReason {
		case "":
		case OmitTokenBudget, OmitByteCap, OmitChainEvicted:
			reasons[row.OmissionReason] = true
		default:
			t.Errorf("%s: row %d (%s/%s): OmissionReason %q outside the fixed vocabulary",
				where, i, row.Subject.Domain, row.Subject.ID, row.OmissionReason)
		}
	}
}

// TestMixedTraceAnchorContentExactPin pins materialized anchor Content as an
// EXACT string, separator and fragment order included. See the file header for
// why nothing weaker suffices: the ledger identity and its independent
// recompute are both blind to anchorJoined's separator.
func TestMixedTraceAnchorContentExactPin(t *testing.T) {
	ctx := context.Background()
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	t.Run("fragments joined by one newline in group order", func(t *testing.T) {
		st := floorState(0,
			oneAltGroup("frag-a", 1, pinFragA, altAbstract),
			oneAltGroup("frag-b", 2, pinFragB, altAbstract),
			oneAltGroup("frag-c", 3, pinFragC, altAbstract),
		)
		out, _, tr, err := m.AssembleWithTrace(ctx, st, 0, TokenBudget{Input: 300})
		if err != nil {
			t.Fatalf("AssembleWithTrace: %v", err)
		}
		assertTraceInvariants(t, tr)
		if tr.OmittedSubjects != 0 {
			t.Fatalf("%d subjects omitted; the pin below would not describe a full anchor: %+v",
				tr.OmittedSubjects, tr.Subjects)
		}
		if len(out.Messages) != 2 {
			t.Fatalf("%d messages, want assistant + anchor: %+v", len(out.Messages), out.Messages)
		}
		// Written out literally: separator, order, and nothing else.
		want := pinFragA + "\n" + pinFragB + "\n" + pinFragC
		if got := out.Messages[1].Content; got != want {
			t.Errorf("anchor Content = %q, want %q", got, want)
		}
	})

	t.Run("each anchor joins only its own fragments", func(t *testing.T) {
		st := State{Messages: []Message{
			mixedAsstCall("c1", "c2"),
			mixedToolResult("c1", traceFallback, chainSet(0,
				oneAltGroup("frag-a", 1, pinFragA, altAbstract),
				oneAltGroup("frag-b", 2, pinFragB, altAbstract)), 4096),
			mixedToolResult("c2", traceFallback, chainSet(0,
				oneAltGroup("frag-c", 3, pinFragC, altAbstract)), 4096),
		}}
		out, _, tr, err := m.AssembleWithTrace(ctx, st, 0, TokenBudget{Input: 300})
		if err != nil {
			t.Fatalf("AssembleWithTrace: %v", err)
		}
		assertTraceInvariants(t, tr)
		want := []struct{ callID, content string }{
			{"c1", pinFragA + "\n" + pinFragB},
			{"c2", pinFragC},
		}
		if len(out.Messages) != len(want)+1 {
			t.Fatalf("%d messages, want assistant + %d anchors: %+v", len(out.Messages), len(want), out.Messages)
		}
		for i, w := range want {
			msg := out.Messages[i+1]
			if msg.ToolCallID != w.callID || msg.Content != w.content {
				t.Errorf("anchor %d = callID %q content %q, want %q %q",
					i, msg.ToolCallID, msg.Content, w.callID, w.content)
			}
		}
	})

	t.Run("retained anchor with nothing chosen is the placeholder verbatim", func(t *testing.T) {
		// The single alternative costs 60, affordable only well past the chain's
		// own base footprint (56), so the anchor keeps the precharged
		// placeholder — model-visible bytes that were charged at admission.
		st := mixedOmitState(4096)
		out, _, tr, err := m.AssembleWithTrace(ctx, st, 0, TokenBudget{Input: 60})
		if err != nil {
			t.Fatalf("AssembleWithTrace: %v", err)
		}
		assertTraceInvariants(t, tr)
		if len(out.Messages) != 2 {
			t.Fatalf("%d messages, want assistant + anchor: %+v", len(out.Messages), out.Messages)
		}
		if got := out.Messages[1].Content; got != omittedObservation {
			t.Errorf("anchor Content = %q, want exactly the placeholder %q", got, omittedObservation)
		}
	})
}

// TestMixedTraceFloorTerminalRow exercises the one Decision value no other
// fixture in this package reaches. See floorLadder for why.
func TestMixedTraceFloorTerminalRow(t *testing.T) {
	st := floorTerminalState()
	// Vacuity guard: the ladder must really be terminal at the floor rung, or
	// the upgrade pass would overwrite the decision and this test would be
	// asserting `base`/`upgrade` under a floor-shaped name.
	alts := st.Messages[1].Context.Groups[0].Alternatives
	if len(alts) != 2 || verbatimComponents(alts[1].Desc) == 0 {
		t.Fatalf("fixture is not floor-terminal: %d alternatives, last carries %d verbatim components",
			len(alts), verbatimComponents(alts[len(alts)-1].Desc))
	}

	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 300})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	assertTraceInvariants(t, tr)
	assertLedgerExact(t, "floor terminal", runeEstimator, st, out, tr)

	assertRows(t,
		[]ContextSubjectTrace{
			traceRow(t, tr, contextdepth.DomainRAG, "floor.go"),
			traceRow(t, tr, contextdepth.DomainRAG, "plain.go"),
		},
		[]wantRow{{
			domain: contextdepth.DomainRAG, id: "floor.go", callID: "c1", lane: 1, rank: 4,
			depth: contextdepth.DepthL2,
			reps:  []contextdepth.RepresentationDesc{altAbstract, altEvidence},
			// The floor rung, not the cheaper orientation one it skipped.
			tokens: len(floorEvidence), bytes: len(floorEvidence), decision: DecisionFloor,
		}, {
			domain: contextdepth.DomainRAG, id: "plain.go", callID: "c1", lane: 1, rank: 2,
			depth:  contextdepth.DepthL0,
			reps:   []contextdepth.RepresentationDesc{altAbstract},
			tokens: len(pinFragC), bytes: len(pinFragC), decision: DecisionBase,
		}})

	if len(out.Messages) != 2 || out.Messages[1].Content != floorEvidence+"\n"+pinFragC {
		t.Errorf("anchor Content = %q, want %q", out.Messages[1].Content, floorEvidence+"\n"+pinFragC)
	}
	if tr.VerbatimShortfalls != 0 {
		t.Errorf("VerbatimShortfalls = %d, want 0: the floor of 1 was met", tr.VerbatimShortfalls)
	}
}

// traceFixture is one input to the property sweep, with the model-visible
// strings its trace must never carry.
type traceFixture struct {
	name string
	st   State
	// sentinels are checked as a SET, not one by one: the sighting guard below
	// demands that SOME sentinel reached the model-visible state, not each. Some
	// never can — traceFallback is the flat rendering mixed mode always replaces,
	// and floorOrient loses to the floor rung at every budget (swapping the
	// 26-byte placeholder for the 18-byte evidence rung is a NEGATIVE delta, so
	// it always fits). Their absence from a trace proves nothing; the property
	// binds through the reachable ones.
	sentinels []string
	// maxBudget must be high enough to reach a zero-omission trace under BOTH
	// estimators, which the sawFull guard below enforces — nonAdditiveEstimator
	// prices the same fixture higher, and a number tuned against runeEstimator
	// alone silently leaves the retained end of the fixture unswept.
	maxBudget int
	// invisible marks a fixture whose content can never reach the model at ANY
	// budget — its chain is evicted by an under-placeholder cap, or its only
	// alternative is larger than the anchor's byte cap. The content-free check on
	// it is vacuous by design (these fixtures exist for the omission paths), so
	// the per-fixture vacuity guard must not demand a sighting; the sweep-wide
	// one still does.
	invisible bool
}

func traceFixtures() []traceFixture {
	summarized := mixedTraceState()
	summarized.DurableSummary = "PRIOR-SUMMARY-TEXT"
	bigX := strings.Repeat("X", 60)
	ladder := func(id string, rank int) ContextGroup {
		return ladderGroup(id, rank, "LAD-ABS-"+id, "LAD-OVR", "LAD-EV1", "LAD-EV2")
	}
	ladderSentinels := []string{"LAD-ABS-a.go", "LAD-ABS-b.go", "LAD-OVR", "LAD-EV1", "LAD-EV2"}
	return []traceFixture{
		{name: "end to end", st: mixedTraceState(),
			sentinels: []string{traceCardContent, traceEvidenceContent, traceFallback}, maxBudget: 110},
		{name: "durable summary and history lane", st: summarized,
			sentinels: []string{traceCardContent, traceEvidenceContent, traceFallback}, maxBudget: 130},
		{name: "omission by token budget", st: mixedOmitState(4096),
			sentinels: []string{bigX, traceFallback}, maxBudget: 130},
		{name: "omission by byte cap", st: mixedOmitState(30),
			sentinels: []string{bigX, traceFallback}, maxBudget: 130, invisible: true},
		{name: "chain evicted by an under-placeholder cap", st: mixedOmitState(20),
			sentinels: []string{bigX, traceFallback}, maxBudget: 130, invisible: true},
		{name: "floor terminal", st: floorTerminalState(),
			sentinels: []string{floorOrient, floorEvidence, pinFragC}, maxBudget: 130},
		{name: "real rag ladder over two anchors", st: twoAnchorState(4096, 4096, ladder("a.go", 1), ladder("b.go", 2)),
			sentinels: ladderSentinels, maxBudget: 200},
		{name: "two chains, four anchors", st: mixedOracleState(1, 1<<20),
			sentinels: oracleSentinels(), maxBudget: 290},
	}
}

// TestMixedTraceProperties sweeps every fixture x every budget x both
// estimators and asserts the rules that must hold on EVERY successful trace:
// spec 3.4's D6 pairing in both directions, the selected = rendered + omitted
// partition, the exact ledger identity, the fixed decision/reason vocabulary,
// content-freedom, [] rather than null for every slice field, rows that own
// their descriptor slices, and input-State immutability.
//
// The vocabulary check is also a coverage check: all four Decision values and
// all three OmissionReason values must actually OCCUR across the sweep, so the
// fixture table cannot silently stop exercising one of them.
//
// BISECTING NOTE: those coverage guards live at the parent level and run whatever
// -run filter is in effect, so narrowing to one subtest (-run
// 'TestMixedTraceProperties/rune/end_to_end') reports spurious "never occurred"
// errors. Read the subtest's own failures and ignore the parent's; the guards are
// only meaningful over the whole table.
func TestMixedTraceProperties(t *testing.T) {
	estimators := []struct {
		name string
		est  func(string) int
	}{
		{"rune", runeEstimator},
		// Deliberately not additive over concatenation: an allocator that summed
		// per-fragment estimates instead of taking whole-anchor deltas can still
		// balance its books under the character estimator.
		{"non-additive", nonAdditiveEstimator},
	}
	decisions, reasons := map[string]bool{}, map[string]bool{}
	sightings, rowsChecked := 0, 0

	for _, e := range estimators {
		for _, f := range traceFixtures() {
			t.Run(e.name+"/"+f.name, func(t *testing.T) {
				before := stateSnapshot(t, f.st)
				seen, sawFull := false, false
				for budget := 0; budget <= f.maxBudget; budget++ {
					where := fmt.Sprintf("%s/%s budget %d", e.name, f.name, budget)
					m := ContextManager{Mixed: true, Estimate: e.est}
					out, _, tr, err := m.AssembleWithTrace(context.Background(), f.st, 0, TokenBudget{Input: budget})
					if err != nil {
						// The ONLY tolerated failure is a must-fit overflow at a
						// budget too small for the reservations. Anything else —
						// a validation error, a compactor error — is a real
						// failure that must not be skipped as "expected".
						if !errors.Is(err, ErrContextExhausted) {
							t.Fatalf("%s: AssembleWithTrace: %v", where, err)
						}
						if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
							t.Errorf("%s: error path must return a zero trace, got %+v", where, tr)
						}
						continue
					}
					if tr.Subjects == nil {
						t.Fatalf("%s: successful mixed assembly with a nil Subjects slice", where)
					}
					if tr.MaxTokens != budget {
						t.Errorf("%s: MaxTokens = %d, want the state budget %d", where, tr.MaxTokens, budget)
					}
					if tr.OmittedSubjects == 0 {
						sawFull = true
					}
					assertTraceInvariants(t, tr)
					assertLedgerExact(t, where, e.est, f.st, out, tr)
					collectVocabulary(t, where, tr, decisions, reasons)
					rowsChecked += assertRowsUnaliased(t, where, f.st, tr)

					js, err := json.Marshal(tr)
					if err != nil {
						t.Fatalf("%s: marshal trace: %v", where, err)
					}
					assertContentFree(t, where, string(js), f.sentinels)
					assertNoNullSlices(t, where, string(js))

					// Vacuity guard for the content-free check: prove the same
					// sentinels reach the model-visible state, or its absence
					// from the trace proves nothing.
					for _, s := range f.sentinels {
						for _, msg := range out.Messages {
							if strings.Contains(msg.Content, s) {
								seen, sightings = true, sightings+1
							}
						}
					}
					// mixedUnit.msgs ALIASES State.Messages, so this is the
					// property that catches a materializer rewriting Content in
					// place instead of on a copy.
					if got := stateSnapshot(t, f.st); got != before {
						t.Fatalf("%s: assembly mutated the input State:\n got %s\nwant %s", where, got, before)
					}
				}
				if !seen && !f.invisible {
					t.Errorf("no fixture content ever reached the assembled State; the content-free check on %q is vacuous", f.name)
				}
				// The CONVERSE of the annotation, asserted in the same place it is
				// honored: `invisible` only ever weakens a guard, so a fixture that
				// becomes visible — or a new one mislabelled — would otherwise lose
				// its sighting guard silently.
				if f.invisible && seen {
					t.Errorf("fixture %q is annotated invisible but surfaced content; the guard is disarmed for nothing", f.name)
				}
				// Per-fixture counterpart of the oracle's fullRuns guard: without it
				// a stale maxBudget leaves the fixture's RETAINED end unswept and
				// nothing notices. Skipped for invisible fixtures, whose subjects are
				// omitted at every budget by construction.
				if !sawFull && !f.invisible {
					t.Errorf("maxBudget %d never reached a zero-omission trace; the retained end of %q is unswept",
						f.maxBudget, f.name)
				}
			})
		}
	}

	if rowsChecked == 0 {
		t.Error("no row carried representations; the non-aliasing property never ran")
	}
	if sightings == 0 {
		t.Error("no sentinel ever reached the model-visible state; the content-free property is vacuous")
	}
	for _, want := range []string{DecisionBase, DecisionFloor, DecisionUpgrade, DecisionOmitted} {
		if !decisions[want] {
			t.Errorf("Decision %q never occurred in the sweep; the vocabulary assertion never exercised it", want)
		}
	}
	for _, want := range []string{OmitTokenBudget, OmitByteCap, OmitChainEvicted} {
		if !reasons[want] {
			t.Errorf("OmissionReason %q never occurred in the sweep; the vocabulary assertion never exercised it", want)
		}
	}
}

// TestMixedExhaustiveOracle enumerates two chains x two anchors x two subjects
// over MinVerbatim in {0,1}, four caps (under-placeholder, exactly the
// placeholder, a middling one, effectively unbounded), both estimators and every
// budget up to past the fixture's full cost, asserting on EVERY combination:
//
//	(a) the admission ledger equals an independent recompute;
//	(b) every materialized anchor's Content fits its cap, or the chain is evicted;
//	(c) the subject partition Selected == Rendered + Omitted;
//	(d) chain atomicity: an assistant call and its anchors are retained or
//	    evicted together, and in step with the chain's own span row. The SPAN-ROW
//	    half carries this property: a retained chain whose span row says omitted
//	    is invisible to every other assertion here (the ledger balances). The
//	    message-count half is not independently falsifiable — (a) fires first on
//	    any change to which messages are emitted — so it is a cheap restatement,
//	    not a second net. Eight per-task tests also catch the span-row mutant;
//	    this is the only PROPERTY-layer one.
//	(e) (ToolCallID, Domain, ID) uniqueness across rows;
//	(f) determinism: the same input twice yields an identical trace and State.
//	    FORWARD-LOOKING INSURANCE, not coverage of a present defect: no map is
//	    iterated anywhere in the three production files, so today only an
//	    injected one (verified: keying anchor subjects by ID) can break it.
//
// The fixture reserves nothing must-fit (no pinned message, no unresolved
// chain, empty System), so every budget — zero included — must SUCCEED by
// degrading. An error anywhere in the sweep is itself a failure.
func TestMixedExhaustiveOracle(t *testing.T) {
	// One under the placeholder (chain eviction), exactly the placeholder (the
	// cheapest sibling pair overruns it: byte-cap omission), 32 — a length the
	// ladders hit EXACTLY, so the bound is touched rather than merely respected,
	// and overrun by the next rung up — and effectively unbounded.
	caps := []int{len(omittedObservation) - 1, len(omittedObservation), 32, 1 << 20}
	estimators := []struct {
		name string
		est  func(string) int
	}{{"rune", runeEstimator}, {"non-additive", nonAdditiveEstimator}}

	sharedRefs, evictions, capBound, fullRuns, splitRuns := 0, 0, 0, 0, 0
	for _, e := range estimators {
		// Swept past what one subject can satisfy: with two subjects per anchor and
		// at most two verbatim components per alternative, a floor of 1 is met by
		// the FIRST subject and the pass never spreads. 2 and 3 make it walk both,
		// and 3 is unreachable, which is the shortfall-counting path.
		for _, minVerbatim := range []int{0, 1, 2, 3} {
			for _, outputCap := range caps {
				name := fmt.Sprintf("%s/minVerbatim=%d/cap=%d", e.name, minVerbatim, outputCap)
				t.Run(name, func(t *testing.T) {
					st := mixedOracleState(minVerbatim, outputCap)
					before := stateSnapshot(t, st)
					capOf := map[string]int{}
					for _, msg := range st.Messages {
						if msg.ToolCallID != "" {
							capOf[msg.ToolCallID] = msg.OutputCap
						}
					}
					for budget := 0; budget <= 290; budget++ {
						where := fmt.Sprintf("%s budget %d", name, budget)
						m := ContextManager{Mixed: true, Estimate: e.est}
						out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: budget})
						if err != nil {
							t.Fatalf("%s: the fixture reserves nothing must-fit, so assembly must degrade rather than fail: %v", where, err)
						}

						// (a) + the identity.
						assertLedgerExact(t, where, e.est, st, out, tr)
						// (c) partition, and D6 per row.
						assertTraceInvariants(t, tr)

						// (b) hard cap on model-visible anchor bytes, with NO
						// placeholder exemption: a cap too small for the
						// placeholder evicts the chain instead.
						for i, msg := range out.Messages {
							if msg.Role != "tool" {
								continue
							}
							lim, ok := capOf[msg.ToolCallID]
							if !ok {
								t.Fatalf("%s: materialized anchor %d has call ID %q, absent from the input", where, i, msg.ToolCallID)
							}
							if len(msg.Content) > lim {
								t.Errorf("%s: anchor %q Content is %d bytes, over its cap %d: %q",
									where, msg.ToolCallID, len(msg.Content), lim, msg.Content)
							}
							if len(msg.Content) == lim {
								capBound++
							}
						}

						if tr.OmittedSubjects == 0 {
							fullRuns++
						}
						// (d) chain atomicity, cross-checked against the span row.
						retainedChains := 0
						for ci, chain := range oracleChains {
							calls, results := 0, 0
							for _, msg := range out.Messages {
								if slices.ContainsFunc(msg.ToolCalls, func(tc provider.ToolCall) bool {
									return slices.Contains(chain, tc.ID)
								}) {
									calls++
								}
								if msg.Role == "tool" && slices.Contains(chain, msg.ToolCallID) {
									results++
								}
							}
							retained := calls > 0 || results > 0
							if retained && (calls != 1 || results != len(chain)) {
								t.Errorf("%s: chain %d materialized %d assistant calls and %d results, want 1 and %d or nothing at all",
									where, ci, calls, results, len(chain))
							}
							span := traceRow(t, tr, contextdepth.DomainConversation, oracleChainSpans[ci])
							if span.Omitted == retained {
								t.Errorf("%s: chain %d span row Omitted = %v while %d of its messages were materialized",
									where, ci, span.Omitted, calls+results)
							}
							if retained {
								retainedChains++
							} else {
								evictions++
							}
						}
						if retainedChains == 1 {
							splitRuns++
						}

						// (e) assembly-wide subject identity.
						seen := map[[3]string]int{}
						byRef := map[[2]string]int{}
						for i, row := range tr.Subjects {
							key := [3]string{row.ToolCallID, row.Subject.Domain, row.Subject.ID}
							if prev, dup := seen[key]; dup {
								t.Errorf("%s: rows %d and %d share subject identity %v", where, prev, i, key)
							}
							seen[key] = i
							byRef[[2]string{row.Subject.Domain, row.Subject.ID}]++
						}
						for _, n := range byRef {
							if n > 1 {
								sharedRefs++
							}
						}

						// (f) determinism.
						out2, _, tr2, err2 := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: budget})
						if err2 != nil {
							t.Fatalf("%s: second assembly failed: %v", where, err2)
						}
						if !reflect.DeepEqual(tr, tr2) {
							t.Errorf("%s: the same input yielded two different traces:\n%+v\n%+v", where, tr, tr2)
						}
						if !reflect.DeepEqual(out, out2) {
							t.Errorf("%s: the same input yielded two different States:\n%+v\n%+v", where, out, out2)
						}
						if got := stateSnapshot(t, st); got != before {
							t.Fatalf("%s: assembly mutated the input State:\n got %s\nwant %s", where, got, before)
						}
					}
				})
			}
		}
	}

	// Vacuity guards: each property above needs its precondition to have
	// actually occurred somewhere in the enumeration.
	if sharedRefs == 0 {
		t.Error("no (Domain, ID) ever appeared on two rows; the uniqueness property never depended on ToolCallID")
	}
	if evictions == 0 {
		t.Error("no chain was ever evicted; the atomicity property never saw the eviction side")
	}
	if capBound == 0 {
		t.Error("no anchor ever rendered exactly to its cap; the cap sweep never bound anything")
	}
	if fullRuns == 0 {
		t.Error("no budget ever retained every subject; the sweep never reached the unconstrained end")
	}
	if splitRuns == 0 {
		t.Error("no budget ever retained exactly one of the two chains; atomicity never saw a partial transcript")
	}
}
