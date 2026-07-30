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

// Fixture content strings. They are deliberately unlike any trace vocabulary
// word, so the content-free property test can look for them literally.
const (
	traceCardContent     = "ONE-CARD"
	traceEvidenceContent = "TWO-EVIDENCE"
	traceFallback        = "FLAT-FALLBACK"
	traceFallbackKey     = "FALLBACK-KEY"
)

// mixedTraceSet is a two-group anchor payload with exactly ONE alternative per
// group, so the allocation is fully determined by the fixture: no upgrade can
// fire and no cheaper alternative exists to fall back to. The two groups differ
// in every traced dimension — depth (L0 vs L2), representation kind, rank (7 and
// 3, deliberately not the group indices) and attribution (nil vs one source) —
// so a row builder that read the wrong group's descriptor, or numbered ranks by
// loop index, cannot pass.
func mixedTraceSet() *ContextSet {
	return &ContextSet{Groups: []ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			Rank:    7,
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: traceCardContent,
		}},
	}, {
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "two.go"},
			Rank:    3,
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim},
			}},
			Content: traceEvidenceContent,
			Attrib: &RetrievalAttribution{Sources: []RetrievedSource{
				{StableKey: "k2", Source: "two.go", StartLine: 4, EndLine: 9, Score: 0.5},
			}},
		}},
	}}}
}

// mixedTraceState is the end-to-end fixture: one prior-history exchange, one
// pinned goal, one current-run plain exchange and one completed chain whose tool
// result is a structured anchor. The anchor also carries a FALLBACK Attrib, which
// mixed mode must drop in favor of the chosen alternatives' attribution.
//
// Costs under runeEstimator (1 token per rune; messageCost adds the tool-call
// and tool-name/id fields): System 3, history 2+2, goal 4, plain 2+2, assistant
// call 20, anchor envelope 10, joined anchor content 21. Ledger 66.
func mixedTraceState() State {
	anchor := mixedToolResult("c1", traceFallback, mixedTraceSet(), 4096)
	anchor.Attrib = &RetrievalAttribution{Sources: []RetrievedSource{
		{StableKey: traceFallbackKey, Source: "fallback.go"},
	}}
	return State{
		System: "SYS",
		Messages: []Message{
			history("user", "HQ"),      // 0 \ prior-history exchange: lane 2, subject ID "0"
			history("assistant", "HA"), // 1 /
			pinned("user", "GOAL"),     // 2   must-fit
			elastic("user", "PQ"),      // 3 \ current-run plain exchange: lane 0, ID "3"
			elastic("assistant", "PA"), // 4 /
			mixedAsstCall("c1"),        // 5 \ completed chain: lane 1, ID "5"
			anchor,                     // 6 /  structured anchor
		},
	}
}

// mixedOmitState is one completed chain whose single alternative costs 60 —
// affordable only with budget left over after the chain's own base footprint.
// outputCap selects which constraint bites.
func mixedOmitState(outputCap int) State {
	set := &ContextSet{Groups: []ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "big.go"},
			Rank:    5,
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: strings.Repeat("X", 60),
		}},
	}}}
	anchor := mixedToolResult("c1", traceFallback, set, outputCap)
	anchor.Attrib = &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: traceFallbackKey}}}
	return State{Messages: []Message{mixedAsstCall("c1"), anchor}}
}

// mixedSysTokens is the only reservation mixed assembly charges outside the
// units: the system prompt plus the materialized durable summary. Pinned
// MESSAGES are units and must NOT appear here — adding them would double-charge
// the pinned span, which is only observable at a budget tight enough that the
// inflated ledger changes a decision (see TestAssembleWithTraceExactFitBudget).
func mixedSysTokens(st State) int {
	lenR := func(s string) int { return len([]rune(s)) }
	n := lenR(st.System)
	if summary := strings.TrimSpace(st.DurableSummary); summary != "" {
		n += lenR(DurableSummaryPrompt(summary))
	}
	return n
}

// mixedAssembleOracle recomputes an assembled State's model-visible cost from
// the raw estimator INLINE — not through messageCost/totalTokens, which are the
// functions under test — so a drift between the trace's number and the state it
// describes shows up instead of cancelling on both sides.
func mixedAssembleOracle(out State) int {
	lenR := func(s string) int { return len([]rune(s)) }
	n := lenR(out.System)
	for _, msg := range out.Messages {
		n += lenR(msg.Content)
		for _, tc := range msg.ToolCalls {
			n += lenR(tc.ID) + lenR(tc.Type) + lenR(tc.Function.Name) + lenR(string(tc.Function.Arguments))
		}
		n += lenR(msg.ToolName) + lenR(msg.ToolCallID)
	}
	return n
}

// assertMixedLedger checks the trace's token accounting three ways: against the
// admission ledger (re-derived by running build+allocate on the SAME
// pre-materialization State), against an independent recompute of the returned
// State, and against the Used+Free==MaxTokens identity. The ledger and the
// recompute come from different code — a running delta ledger vs a fresh walk of
// the materialized copy — so materialization that emits uncharged content (an
// evicted chain, a differently joined anchor) breaks this even though each side
// is internally consistent.
//
// sysTokens is DERIVED here, never supplied by the caller: a hand-passed number
// makes the oracle and the implementation agree on that term by construction,
// which is exactly how a double-charged pinned span would hide.
func assertMixedLedger(t *testing.T, st State, out State, tr ContextAssemblyTrace) {
	t.Helper()
	sysTokens := mixedSysTokens(st)
	_, alloc, err := allocFixture(t, runeEstimator, st, tr.MaxTokens, sysTokens)
	if err != nil {
		t.Fatalf("ledger oracle: re-running allocation failed: %v", err)
	}
	if tr.EstimatedTokensUsed != alloc.used {
		t.Errorf("EstimatedTokensUsed = %d, want the admission ledger %d", tr.EstimatedTokensUsed, alloc.used)
	}
	if got := mixedAssembleOracle(out); tr.EstimatedTokensUsed != got {
		t.Errorf("EstimatedTokensUsed = %d, but the returned State costs %d", tr.EstimatedTokensUsed, got)
	}
	if tr.EstimatedTokensUsed+tr.EstimatedTokensFree != tr.MaxTokens {
		t.Errorf("used %d + free %d != MaxTokens %d", tr.EstimatedTokensUsed, tr.EstimatedTokensFree, tr.MaxTokens)
	}
	if tr.EstimatedTokensFree < 0 {
		t.Errorf("EstimatedTokensFree = %d, want >= 0 on a non-error trace", tr.EstimatedTokensFree)
	}
}

// assertTraceInvariants enforces spec 3.4's cross-cutting rules on a successful
// trace: the D6 per-subject rule and the selected = rendered + omitted partition,
// with the two counters RECOUNTED from the rows so a counter that disagrees with
// its own rows fails here.
func assertTraceInvariants(t *testing.T, tr ContextAssemblyTrace) {
	t.Helper()
	if tr.Subjects == nil {
		t.Error("successful mixed trace must carry a non-nil Subjects slice (Task 9's zero-trace guard keys on it)")
	}
	rendered, omitted := 0, 0
	for i, s := range tr.Subjects {
		where := fmt.Sprintf("row %d (%s/%s)", i, s.Subject.Domain, s.Subject.ID)
		if s.Omitted {
			omitted++
			if s.Decision != DecisionOmitted {
				t.Errorf("%s: omitted with decision %q, want %q", where, s.Decision, DecisionOmitted)
			}
			if s.OmissionReason == "" {
				t.Errorf("%s: omitted with no reason", where)
			}
			if s.EffectiveDepth != contextdepth.DepthInvalid {
				t.Errorf("%s: omitted with depth %v, want DepthInvalid", where, s.EffectiveDepth)
			}
			if s.EstimatedTokens != 0 || s.Bytes != 0 {
				t.Errorf("%s: omitted but costs tokens=%d bytes=%d", where, s.EstimatedTokens, s.Bytes)
			}
			if len(s.Representations) != 0 {
				t.Errorf("%s: omitted but carries representations %+v", where, s.Representations)
			}
			continue
		}
		rendered++
		if !s.EffectiveDepth.Valid() {
			t.Errorf("%s: retained with invalid depth %v", where, s.EffectiveDepth)
		}
		switch s.Decision {
		case DecisionBase, DecisionFloor, DecisionUpgrade:
		default:
			t.Errorf("%s: retained with decision %q, want base/floor/upgrade", where, s.Decision)
		}
		if s.OmissionReason != "" {
			t.Errorf("%s: retained but carries reason %q", where, s.OmissionReason)
		}
		if len(s.Representations) == 0 {
			t.Errorf("%s: retained with no representations", where)
		}
		if s.Representations == nil {
			t.Errorf("%s: nil Representations marshals as null", where)
		}
	}
	if tr.SelectedSubjects != len(tr.Subjects) {
		t.Errorf("SelectedSubjects = %d, want %d rows", tr.SelectedSubjects, len(tr.Subjects))
	}
	if tr.SelectedSubjects != tr.RenderedSubjects+tr.OmittedSubjects {
		t.Errorf("partition broken: selected %d != rendered %d + omitted %d",
			tr.SelectedSubjects, tr.RenderedSubjects, tr.OmittedSubjects)
	}
	if tr.RenderedSubjects != rendered || tr.OmittedSubjects != omitted {
		t.Errorf("counters disagree with the rows: rendered %d/%d omitted %d/%d",
			tr.RenderedSubjects, rendered, tr.OmittedSubjects, omitted)
	}
}

// wantRow is one expected trace row, flattened so a table entry reads as the
// whole assertion.
type wantRow struct {
	domain, id, callID string
	lane, rank         int
	omitted            bool
	depth              contextdepth.Depth
	reps               []contextdepth.RepresentationDesc
	tokens, bytes      int
	decision, reason   string
}

func assertRows(t *testing.T, got []ContextSubjectTrace, want []wantRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d subject rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		wantRef := contextdepth.SubjectRef{Domain: w.domain, ID: w.id}
		if g.Subject != wantRef {
			t.Errorf("row %d: Subject = %+v, want %+v", i, g.Subject, wantRef)
		}
		if g.ToolCallID != w.callID {
			t.Errorf("row %d: ToolCallID = %q, want %q", i, g.ToolCallID, w.callID)
		}
		if g.Lane != w.lane {
			t.Errorf("row %d (%s): Lane = %d, want %d", i, w.id, g.Lane, w.lane)
		}
		if g.Rank != w.rank {
			t.Errorf("row %d (%s): Rank = %d, want %d", i, w.id, g.Rank, w.rank)
		}
		if g.Omitted != w.omitted {
			t.Errorf("row %d (%s): Omitted = %v, want %v", i, w.id, g.Omitted, w.omitted)
		}
		if g.EffectiveDepth != w.depth {
			t.Errorf("row %d (%s): EffectiveDepth = %v, want %v", i, w.id, g.EffectiveDepth, w.depth)
		}
		if !slices.Equal(g.Representations, w.reps) {
			t.Errorf("row %d (%s): Representations = %+v, want %+v", i, w.id, g.Representations, w.reps)
		}
		if g.EstimatedTokens != w.tokens {
			t.Errorf("row %d (%s): EstimatedTokens = %d, want %d", i, w.id, g.EstimatedTokens, w.tokens)
		}
		if g.Bytes != w.bytes {
			t.Errorf("row %d (%s): Bytes = %d, want %d", i, w.id, g.Bytes, w.bytes)
		}
		if g.Decision != w.decision {
			t.Errorf("row %d (%s): Decision = %q, want %q", i, w.id, g.Decision, w.decision)
		}
		if g.OmissionReason != w.reason {
			t.Errorf("row %d (%s): OmissionReason = %q, want %q", i, w.id, g.OmissionReason, w.reason)
		}
	}
}

// traceRow finds one row by domain-qualified subject ID.
func traceRow(t *testing.T, tr ContextAssemblyTrace, domain, id string) ContextSubjectTrace {
	t.Helper()
	for _, s := range tr.Subjects {
		if s.Subject.Domain == domain && s.Subject.ID == id {
			return s
		}
	}
	t.Fatalf("no %s/%s row among %+v", domain, id, tr.Subjects)
	return ContextSubjectTrace{}
}

var (
	repL0Metadata = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata}
	repL2Verbatim = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}
	repL1Compact  = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationCompact}
)

// TestAssembleWithTraceMixedEndToEnd is the primary materialization assertion:
// the exact model-visible messages, the exact trace rows, and the ledger.
func TestAssembleWithTraceMixedEndToEnd(t *testing.T) {
	st := mixedTraceState()
	before := slices.Clone(st.Messages)
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, pressure, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 200})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}

	// Carry-forward from Task 6: mixedUnit.msgs ALIASES State.Messages, so a
	// materializer that rewrote Content in place would corrupt the caller's
	// canonical transcript — and spec 4.2 requires the original anchors (with
	// their fallback Content) survive in Result.Messages.
	if !reflect.DeepEqual(st.Messages, before) {
		t.Errorf("AssembleWithTrace mutated the input State:\n got %+v\nwant %+v", st.Messages, before)
	}
	if st.Messages[6].Context == nil || st.Messages[6].Content != traceFallback {
		t.Errorf("input anchor lost its set or fallback Content: %+v", st.Messages[6])
	}

	if out.System != "SYS" {
		t.Errorf("out.System = %q, want %q", out.System, "SYS")
	}
	// The joined separator is written literally here: anchor Content must be
	// exactly what anchorJoined charged, fragments in GROUP order.
	wantMsgs := []struct {
		role, content, callID string
		segment               Segment
		toolCalls             int
		attribKeys            []string
	}{
		{role: "user", content: "HQ", segment: Elastic},
		{role: "assistant", content: "HA", segment: Elastic},
		{role: "user", content: "GOAL", segment: Pinned},
		{role: "user", content: "PQ", segment: Elastic},
		{role: "assistant", content: "PA", segment: Elastic},
		{role: "assistant", content: "", segment: Elastic, toolCalls: 1},
		{
			role: "tool", content: traceCardContent + "\n" + traceEvidenceContent,
			callID: "c1", segment: Elastic, attribKeys: []string{"k2"},
		},
	}
	if len(out.Messages) != len(wantMsgs) {
		t.Fatalf("%d assembled messages, want %d: %+v", len(out.Messages), len(wantMsgs), out.Messages)
	}
	for i, w := range wantMsgs {
		g := out.Messages[i]
		if g.Role != w.role || g.Content != w.content || g.ToolCallID != w.callID || g.Segment != w.segment {
			t.Errorf("message %d = {role %q content %q callID %q segment %v}, want {%q %q %q %v}",
				i, g.Role, g.Content, g.ToolCallID, g.Segment, w.role, w.content, w.callID, w.segment)
		}
		if len(g.ToolCalls) != w.toolCalls {
			t.Errorf("message %d: %d tool calls, want %d", i, len(g.ToolCalls), w.toolCalls)
		}
		if g.Context != nil {
			t.Errorf("message %d: runtime Context must be stripped from the materialized copy", i)
		}
		var keys []string
		if g.Attrib != nil {
			for _, s := range g.Attrib.Sources {
				keys = append(keys, s.StableKey)
			}
		}
		if !slices.Equal(keys, w.attribKeys) {
			t.Errorf("message %d: attribution keys = %v, want %v (rebuilt from chosen alternatives only)",
				i, keys, w.attribKeys)
		}
	}
	// The fallback Content must be REPLACED, never appended alongside the
	// structured fragments, and the fallback attribution must not survive.
	anchorMsg := out.Messages[6]
	if strings.Contains(anchorMsg.Content, traceFallback) {
		t.Errorf("anchor Content %q still carries the fallback rendering", anchorMsg.Content)
	}
	if anchorMsg.Attrib != nil && slices.ContainsFunc(anchorMsg.Attrib.Sources,
		func(s RetrievedSource) bool { return s.StableKey == traceFallbackKey }) {
		t.Errorf("anchor Attrib kept the legacy fallback attribution: %+v", anchorMsg.Attrib.Sources)
	}
	// One retained call ID, one tool message: subjects share their anchor.
	tools := 0
	for _, msg := range out.Messages {
		if msg.Role == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Errorf("%d tool messages, want exactly one per retained call ID", tools)
	}

	assertTraceInvariants(t, tr)
	// Conversation rows keep Rank 0 (documented on the field): conversation order
	// IS the rank and the subject ID is the ordering key. Anchor rows carry the
	// producer's GroupDesc.Rank (7 and 3), never the loop index.
	assertRows(t, tr.Subjects, []wantRow{
		{domain: contextdepth.DomainConversation, id: "0", lane: 2, depth: contextdepth.DepthL2,
			reps: []contextdepth.RepresentationDesc{repL2Verbatim}, tokens: 4, bytes: 4, decision: DecisionBase},
		{domain: contextdepth.DomainConversation, id: "3", lane: 0, depth: contextdepth.DepthL2,
			reps: []contextdepth.RepresentationDesc{repL2Verbatim}, tokens: 4, bytes: 4, decision: DecisionBase},
		{domain: contextdepth.DomainConversation, id: "5", lane: 1, depth: contextdepth.DepthL2,
			reps: []contextdepth.RepresentationDesc{repL2Verbatim}, tokens: 30, bytes: 0, decision: DecisionBase},
		{domain: contextdepth.DomainRAG, id: "one.go", callID: "c1", lane: 1, rank: 7, depth: contextdepth.DepthL0,
			reps: []contextdepth.RepresentationDesc{repL0Metadata}, tokens: 8, bytes: 8, decision: DecisionBase},
		{domain: contextdepth.DomainRAG, id: "two.go", callID: "c1", lane: 1, rank: 3, depth: contextdepth.DepthL2,
			reps: []contextdepth.RepresentationDesc{repL2Verbatim}, tokens: 12, bytes: 12, decision: DecisionBase},
	})

	if tr.MaxTokens != 200 || tr.EstimatedTokensUsed != 66 || tr.EstimatedTokensFree != 134 {
		t.Errorf("trace totals = max %d used %d free %d, want 200/66/134",
			tr.MaxTokens, tr.EstimatedTokensUsed, tr.EstimatedTokensFree)
	}
	if tr.VerbatimShortfalls != 0 {
		t.Errorf("VerbatimShortfalls = %d, want 0 (MinVerbatim is 0)", tr.VerbatimShortfalls)
	}
	// Per-subject costs deliberately do NOT sum to the total: the system prompt,
	// the pinned goal and the anchor's "\n" separator are in the total and in no
	// row. Pinning both numbers keeps that documented property honest.
	rowSum := 0
	for _, s := range tr.Subjects {
		rowSum += s.EstimatedTokens
	}
	if rowSum != 58 {
		t.Errorf("row token sum = %d, want 58 (system 3 + goal 4 + separator 1 are in the total only)", rowSum)
	}
	assertMixedLedger(t, st, out, tr)

	if pressure.InputTokens != 66 || pressure.InputBudget != 200 {
		t.Errorf("pressure tokens/budget = %d/%d, want 66/200", pressure.InputTokens, pressure.InputBudget)
	}
	if pressure.Evicted != 0 || pressure.Compactions != 0 {
		t.Errorf("nothing was dropped, got evicted %d compactions %d", pressure.Evicted, pressure.Compactions)
	}
	if pressure.Level != LevelOK || pressure.Mitigation != MitigationNone {
		t.Errorf("pressure = %v/%v, want ok/none", pressure.Level, pressure.Mitigation)
	}
}

// TestAssembleWithTraceUpgradedSubject pins that a row describes the alternative
// the allocator SETTLED ON, not the one it started from. Every other fixture here
// admits alternative 0, so a row builder reading alts[0] instead of
// alts[s.chosen] would pass all of them. The unreachable MinVerbatim of 5 also
// puts a verbatim shortfall on the wire.
func TestAssembleWithTraceUpgradedSubject(t *testing.T) {
	// Ladder: "AA", "AAOOO", "AAEEEE", "AAOOOEEEE" (verbatim counts 0,0,1,1).
	// The floor pass takes index 2, the upgrade pass finishes at index 3.
	set := chainSet(5, ladderGroup("src.go", 2, "AA", "OOO", "EEEE"))
	st := State{Messages: []Message{
		mixedAsstCall("c1"),
		mixedToolResult("c1", traceFallback, set, 4096),
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 300})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	assertTraceInvariants(t, tr)
	assertMixedLedger(t, st, out, tr)

	if len(out.Messages) != 2 || out.Messages[1].Content != "AAOOOEEEE" {
		t.Fatalf("anchor Content = %q, want the last ladder rung %q", out.Messages[1].Content, "AAOOOEEEE")
	}
	assertRows(t, []ContextSubjectTrace{traceRow(t, tr, contextdepth.DomainRAG, "src.go")}, []wantRow{{
		domain: contextdepth.DomainRAG, id: "src.go", callID: "c1", lane: 1, rank: 2,
		depth: contextdepth.DepthL2,
		reps:  []contextdepth.RepresentationDesc{altAbstract, altOverview, altEvidence},
		// The chosen rung, not the cheapest one (which costs 2 tokens / 2 bytes).
		tokens: 9, bytes: 9, decision: DecisionUpgrade,
	}})
	// assistant call(20) + anchor envelope(10) + content(9).
	if tr.EstimatedTokensUsed != 39 {
		t.Errorf("EstimatedTokensUsed = %d, want 39", tr.EstimatedTokensUsed)
	}
	if tr.VerbatimShortfalls != 1 {
		t.Errorf("VerbatimShortfalls = %d, want 1 (MinVerbatim 5 is unreachable)", tr.VerbatimShortfalls)
	}
}

// attribLadder is a two-rung group whose alternatives differ ONLY in
// attribution: rung 0 is orientation with nil Attrib, rung 1 adds the verbatim
// evidence and the attribution that credits it. Nothing else in the suite has
// this shape, and without it anchorAttrib reading alts[0] instead of
// alts[s.chosen] emits the right bytes with no attribution at all.
func attribLadder(id, key string) ContextGroup {
	return ContextGroup{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: id},
			Rank:    1,
		},
		Alternatives: []ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract}},
			Content: "OR-" + id,
		}, {
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{altAbstract, altEvidence}},
			Content: "OR-" + id + "-EV",
			Attrib:  &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: key, Source: id}}},
		}},
	}
}

// TestAssembleWithTraceParallelAnchors is the two-anchor (parallel tool call)
// shape. It pins the two things a single-anchor fixture cannot:
//
//   - each anchor's Attrib comes from ITS OWN chosen alternative, so reading
//     alts[0] (no attribution) or the sibling anchor's subjects both fail;
//   - each row's ToolCallID is its own anchor's. ToolCallID is one third of the
//     assembly-wide subject identity (ToolCallID, Domain, ID), so a row that
//     borrowed anchors[0]'s call ID would collapse two distinct subjects into one.
func TestAssembleWithTraceParallelAnchors(t *testing.T) {
	st := State{Messages: []Message{
		mixedAsstCall("c1", "c2"),
		mixedToolResult("c1", traceFallback, chainSet(0, attribLadder("one.go", "K1")), 4096),
		mixedToolResult("c2", traceFallback, chainSet(0, attribLadder("two.go", "K2")), 4096),
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 300})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	assertTraceInvariants(t, tr)
	assertMixedLedger(t, st, out, tr)
	// assistant(40) + two envelopes(10 each) + two upgraded contents(12 each).
	if tr.EstimatedTokensUsed != 84 {
		t.Errorf("EstimatedTokensUsed = %d, want 84", tr.EstimatedTokensUsed)
	}

	// One tool message per call ID, each carrying its own evidence and its own
	// attribution key.
	wantAnchors := []struct{ callID, content, key string }{
		{"c1", "OR-one.go-EV", "K1"},
		{"c2", "OR-two.go-EV", "K2"},
	}
	if len(out.Messages) != len(wantAnchors)+1 {
		t.Fatalf("%d messages, want assistant + %d anchors: %+v", len(out.Messages), len(wantAnchors), out.Messages)
	}
	for i, w := range wantAnchors {
		msg := out.Messages[i+1]
		if msg.ToolCallID != w.callID || msg.Content != w.content {
			t.Errorf("anchor %d = callID %q content %q, want %q %q", i, msg.ToolCallID, msg.Content, w.callID, w.content)
		}
		var keys []string
		if msg.Attrib != nil {
			for _, s := range msg.Attrib.Sources {
				keys = append(keys, s.StableKey)
			}
		}
		if !slices.Equal(keys, []string{w.key}) {
			t.Errorf("anchor %d (%s): attribution keys = %v, want [%s] from its own chosen alternative",
				i, w.callID, keys, w.key)
		}
	}

	// Rows: the chain span, then one row per anchor subject, each stamped with
	// the call ID of the anchor it came from.
	assertRows(t, tr.Subjects, []wantRow{
		{domain: contextdepth.DomainConversation, id: "0", lane: 1, depth: contextdepth.DepthL2,
			reps: []contextdepth.RepresentationDesc{repL2Verbatim}, tokens: 60, bytes: 0, decision: DecisionBase},
		{domain: contextdepth.DomainRAG, id: "one.go", callID: "c1", lane: 1, rank: 1,
			depth: contextdepth.DepthL2, reps: []contextdepth.RepresentationDesc{altAbstract, altEvidence},
			tokens: 12, bytes: 12, decision: DecisionUpgrade},
		{domain: contextdepth.DomainRAG, id: "two.go", callID: "c2", lane: 1, rank: 1,
			depth: contextdepth.DepthL2, reps: []contextdepth.RepresentationDesc{altAbstract, altEvidence},
			tokens: 12, bytes: 12, decision: DecisionUpgrade},
	})
}

// TestContextAssemblyTraceRowsOwnTheirSlices: a consumer mutating a returned
// row must not reach back into package state or into the caller's ContextSet.
// Both would be cross-request corruption from a read-only-looking operation —
// spanRepresentations and summaryRepresentations are package vars shared by
// every assembly in the process, and an anchor row's descriptor belongs to the
// tool's set. Both assemblies share ONE State so the anchor path is covered too.
func TestContextAssemblyTraceRowsOwnTheirSlices(t *testing.T) {
	st := mixedTraceState()
	st.DurableSummary = "PRIOR" // exercise the summary row's clone as well
	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	budget := TokenBudget{Input: 300}

	// Snapshot the caller's own set, so the anchor clone site gets a DIRECT
	// assertion below. Without it that site is only caught indirectly, by the
	// second assembly failing validateContextSet on the descriptors this
	// mutation corrupted — a kill that depends on validation staying strict and
	// that cannot see a consumer who mutates a trace and never re-assembles.
	setBefore := [][]contextdepth.RepresentationDesc{}
	for _, g := range st.Messages[6].Context.Groups {
		for _, a := range g.Alternatives {
			setBefore = append(setBefore, slices.Clone(a.Desc.Representations))
		}
	}
	if len(setBefore) == 0 {
		t.Fatal("fixture anchor carries no alternatives; the set check would be vacuous")
	}

	_, _, first, err := m.AssembleWithTrace(context.Background(), st, 0, budget)
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	baseline := make([][]contextdepth.RepresentationDesc, len(first.Subjects))
	for i, s := range first.Subjects {
		if len(s.Representations) == 0 {
			t.Fatalf("row %d has no representations; the test would be vacuous", i)
		}
		baseline[i] = slices.Clone(s.Representations)
	}
	// A consumer zeroing what it was handed.
	for _, s := range first.Subjects {
		for j := range s.Representations {
			s.Representations[j] = contextdepth.RepresentationDesc{}
		}
	}

	// Checked HERE — after the mutation, before any re-assembly — so an aliased
	// anchor descriptor fails on the property itself rather than by tripping
	// validation on the next call.
	i := 0
	for gi, g := range st.Messages[6].Context.Groups {
		for ai, a := range g.Alternatives {
			if !slices.Equal(a.Desc.Representations, setBefore[i]) {
				t.Errorf("caller's set corrupted at group %d alternative %d: %+v, want %+v",
					gi, ai, a.Desc.Representations, setBefore[i])
			}
			i++
		}
	}

	_, _, second, err := m.AssembleWithTrace(context.Background(), st, 0, budget)
	if err != nil {
		t.Fatalf("second AssembleWithTrace: %v", err)
	}
	if len(second.Subjects) != len(baseline) {
		t.Fatalf("%d rows on the second assembly, want %d", len(second.Subjects), len(baseline))
	}
	for i, s := range second.Subjects {
		if !slices.Equal(s.Representations, baseline[i]) {
			t.Errorf("row %d (%s/%s): Representations = %+v after a consumer mutated an earlier trace, want %+v",
				i, s.Subject.Domain, s.Subject.ID, s.Representations, baseline[i])
		}
	}
}

// TestAssembleWithTraceExactFitBudget runs the end-to-end fixture at the
// tightest budget that retains everything. Peak ledger is 67, not the final 66:
// a chain's anchors are charged the omission placeholder BEFORE their cheaper
// alternatives replace it. At the peak, any reservation charged twice — a
// sysTokens that also added pinnedTokens, say — evicts something, which is the
// only place that double charge is observable.
func TestAssembleWithTraceExactFitBudget(t *testing.T) {
	st := mixedTraceState()
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 67})
	if err != nil {
		t.Fatalf("AssembleWithTrace at the peak budget: %v", err)
	}
	assertTraceInvariants(t, tr)
	assertMixedLedger(t, st, out, tr)
	if tr.OmittedSubjects != 0 {
		t.Errorf("%d subjects omitted at the exact-fit budget: %+v", tr.OmittedSubjects, tr.Subjects)
	}
	if tr.EstimatedTokensUsed != 66 || tr.EstimatedTokensFree != 1 {
		t.Errorf("used/free = %d/%d, want 66/1", tr.EstimatedTokensUsed, tr.EstimatedTokensFree)
	}
	if len(out.Messages) != 7 {
		t.Errorf("%d messages, want all 7 retained: %+v", len(out.Messages), out.Messages)
	}
}

// TestAssembleWithTraceOmittedSubject covers the three omission reasons an
// anchor can carry, the precharged placeholder, and the partition with an
// omitted row present.
func TestAssembleWithTraceOmittedSubject(t *testing.T) {
	tests := []struct {
		name       string
		outputCap  int
		budget     int
		wantReason string
		wantChain  bool // chain evicted: no messages at all
		wantUsed   int
	}{
		{"alternative does not fit the token budget", 4096, 60, OmitTokenBudget, false, 56},
		{"alternative breaches the anchor byte cap", 30, 200, OmitByteCap, false, 56},
		{"cap below the placeholder evicts the chain", 20, 200, OmitChainEvicted, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := mixedOmitState(tc.outputCap)
			m := ContextManager{Mixed: true, Estimate: runeEstimator}
			out, pressure, tr, err := m.AssembleWithTrace(context.Background(), st,
				0, TokenBudget{Input: tc.budget})
			if err != nil {
				t.Fatalf("AssembleWithTrace: %v", err)
			}
			assertTraceInvariants(t, tr)
			assertMixedLedger(t, st, out, tr)
			if tr.EstimatedTokensUsed != tc.wantUsed {
				t.Errorf("EstimatedTokensUsed = %d, want %d", tr.EstimatedTokensUsed, tc.wantUsed)
			}

			row := traceRow(t, tr, contextdepth.DomainRAG, "big.go")
			if !row.Omitted || row.OmissionReason != tc.wantReason {
				t.Errorf("subject row = omitted %v reason %q, want true %q", row.Omitted, row.OmissionReason, tc.wantReason)
			}
			if row.Rank != 5 {
				t.Errorf("omitted row Rank = %d, want the producer's 5", row.Rank)
			}
			chain := traceRow(t, tr, contextdepth.DomainConversation, "0")
			if chain.Omitted != tc.wantChain {
				t.Errorf("chain span row omitted = %v, want %v", chain.Omitted, tc.wantChain)
			}

			if tc.wantChain {
				if len(out.Messages) != 0 {
					t.Fatalf("evicted chain still materialized %+v", out.Messages)
				}
				if pressure.Evicted != 1 || pressure.Compactions != 1 {
					t.Errorf("evicted chain: pressure evicted %d compactions %d, want 1/1",
						pressure.Evicted, pressure.Compactions)
				}
				return
			}
			if len(out.Messages) != 2 {
				t.Fatalf("%d messages, want assistant + anchor: %+v", len(out.Messages), out.Messages)
			}
			anchor := out.Messages[1]
			if anchor.Content != omittedObservation {
				t.Errorf("all-omitted anchor Content = %q, want the precharged placeholder %q",
					anchor.Content, omittedObservation)
			}
			if anchor.Attrib != nil {
				t.Errorf("all-omitted anchor kept attribution %+v; nothing was chosen", anchor.Attrib)
			}
			if anchor.Context != nil {
				t.Error("all-omitted anchor kept its runtime Context")
			}
		})
	}
}

// TestAssembleWithTraceEvictedChainContributesNothing is the Task 7
// carry-forward: allocateChain assigns the placeholder to every anchor BEFORE
// its fit check, so an evicted chain's anchors hold content that was never
// charged. Materializing it would emit model-visible text with no budget behind
// it and break the ledger identity.
func TestAssembleWithTraceEvictedChainContributesNothing(t *testing.T) {
	st := mixedOmitState(20) // cap below the placeholder => the chain is evicted
	st.System = "SYS"
	st.Messages = append([]Message{elastic("user", "PQ"), elastic("assistant", "PA")}, st.Messages...)
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, pressure, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 200})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	assertTraceInvariants(t, tr)
	// SYS(3) + the retained plain exchange(4). The evicted chain adds nothing.
	if tr.EstimatedTokensUsed != 7 {
		t.Errorf("EstimatedTokensUsed = %d, want 7 (system + the retained plain exchange only)", tr.EstimatedTokensUsed)
	}
	assertMixedLedger(t, st, out, tr)

	if len(out.Messages) != 2 {
		t.Fatalf("%d messages, want only the retained plain exchange: %+v", len(out.Messages), out.Messages)
	}
	for i, msg := range out.Messages {
		if msg.Role == "tool" || len(msg.ToolCalls) > 0 {
			t.Errorf("message %d belongs to the evicted chain: %+v", i, msg)
		}
		for _, banned := range []string{traceFallback, omittedObservation, "X"} {
			if strings.Contains(msg.Content, banned) {
				t.Errorf("message %d content %q leaked evicted-chain text %q", i, msg.Content, banned)
			}
		}
	}
	if pressure.Evicted != 1 {
		t.Errorf("pressure.Evicted = %d, want 1", pressure.Evicted)
	}
}

// TestAssembleWithTraceStripsContextOnUnvalidatedSpans is the Task 6
// carry-forward: validateContextSet runs for completed chains only, so a set
// riding a PINNED span or an UNRESOLVED chain reaches assembly unvalidated.
// Those spans are charged verbatim and never allocated — correct for costing —
// but spec 4.2 still requires the materialized copy carry no runtime Context.
// Both sets here are malformed, which also proves validation does not run on
// them: an assembler that validated every set would fail this test.
func TestAssembleWithTraceStripsContextOnUnvalidatedSpans(t *testing.T) {
	bad := setWith(func(s *ContextSet) { s.Groups[0].Desc.Subject.ID = "" })
	pinnedMsg := pinned("system", "PINNED-CTX")
	pinnedMsg.Context = bad
	st := State{System: "SYS", Messages: []Message{
		pinnedMsg,
		mixedAsstCall("c1", "c2"), // two calls, one result => unresolved chain
		mixedToolResult("c1", "UNRESOLVED-RESULT", bad, 4096),
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 200})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	wantContent := []string{"PINNED-CTX", "", "UNRESOLVED-RESULT"}
	if len(out.Messages) != len(wantContent) {
		t.Fatalf("%d messages, want %d: %+v", len(out.Messages), len(wantContent), out.Messages)
	}
	for i, want := range wantContent {
		if out.Messages[i].Content != want {
			t.Errorf("message %d Content = %q, want %q unchanged", i, out.Messages[i].Content, want)
		}
		if out.Messages[i].Context != nil {
			t.Errorf("message %d kept its runtime Context on a non-allocated span", i)
		}
	}
	if st.Messages[0].Context == nil || st.Messages[2].Context == nil {
		t.Error("the caller's State lost its sets; only the materialized copy is stripped")
	}
	assertTraceInvariants(t, tr)
	// SYS(3) + pinned(10) + unresolved chain(40 calls + 27 result).
	if tr.EstimatedTokensUsed != 80 {
		t.Errorf("EstimatedTokensUsed = %d, want 80", tr.EstimatedTokensUsed)
	}
	assertMixedLedger(t, st, out, tr)
	if len(tr.Subjects) != 0 {
		t.Errorf("must-fit spans are not subjects; got rows %+v", tr.Subjects)
	}
	if tr.Subjects == nil {
		t.Error("a successful mixed trace must carry [] rather than null Subjects")
	}
}

// TestAssembleWithTraceHistoryLane pins that units are built from the
// PRE-materialization State. materializeDurableSummary prepends a pinned message
// at index 0, which zeroes firstPinnedIndex and would reclassify every
// prior-history exchange as a current-run plain one (lane 0) with its subject ID
// shifted by one.
func TestAssembleWithTraceHistoryLane(t *testing.T) {
	st := State{System: "SYS", DurableSummary: "PRIOR", Messages: []Message{
		history("user", "HQ"),       // 0 \ prior history: ID "0"
		history("assistant", "HA"),  // 1 /
		history("user", "HQ2"),      // 2 \ prior history: ID "2"
		history("assistant", "HA2"), // 3 /
		pinned("user", "GOAL"),      // 4
		mixedAsstCall("c1"),         // 5 \ completed chain: ID "5"
		mixedToolResult("c1", traceFallback, mixedTraceSet(), 4096), // 6 /
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}

	out, _, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 300})
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	assertTraceInvariants(t, tr)

	for _, id := range []string{"0", "2"} {
		row := traceRow(t, tr, contextdepth.DomainConversation, id)
		if row.Lane != 2 {
			t.Errorf("history subject %q: Lane = %d, want 2", id, row.Lane)
		}
	}
	// The IDs a materialize-first builder would produce must NOT appear.
	for _, s := range tr.Subjects {
		if s.Subject.Domain == contextdepth.DomainConversation && (s.Subject.ID == "1" || s.Subject.ID == "3") {
			t.Errorf("subject ID %q is a post-materialization index; units must come from the caller-visible State", s.Subject.ID)
		}
	}
	if row := traceRow(t, tr, contextdepth.DomainConversation, "5"); row.Lane != 1 {
		t.Errorf("chain subject: Lane = %d, want 1", row.Lane)
	}

	summaryPrompt := DurableSummaryPrompt("PRIOR")
	summary := traceRow(t, tr, contextdepth.DomainConversation, durableSummarySubjectID)
	wantSummary := wantRow{
		domain: contextdepth.DomainConversation, id: durableSummarySubjectID,
		lane: 0, depth: contextdepth.DepthL1,
		reps:   []contextdepth.RepresentationDesc{repL1Compact},
		tokens: len([]rune(summaryPrompt)), bytes: len(summaryPrompt), decision: DecisionBase,
	}
	assertRows(t, []ContextSubjectTrace{summary}, []wantRow{wantSummary})
	if tr.Subjects[0].Subject.ID != durableSummarySubjectID {
		t.Errorf("first row = %+v, want the pinned durable summary", tr.Subjects[0].Subject)
	}

	if len(out.Messages) == 0 || out.Messages[0].Content != summaryPrompt || out.Messages[0].Segment != Pinned {
		t.Fatalf("assembled head = %+v, want the pinned durable summary", out.Messages)
	}
	// The summary reservation is charged once: system(3) + prompt(36) + goal(4)
	// + chain envelope(30) + anchor content(21) + two history spans(4+6).
	if want := 3 + len([]rune(summaryPrompt)); mixedSysTokens(st) != want {
		t.Errorf("fixture sysTokens = %d, want %d", mixedSysTokens(st), want)
	}
	if tr.EstimatedTokensUsed != 104 {
		t.Errorf("EstimatedTokensUsed = %d, want 104", tr.EstimatedTokensUsed)
	}
	assertMixedLedger(t, st, out, tr)
}

// TestAssembleWithTraceMustFitPressure: an unresolved tool chain is a must-fit
// reservation, so its overflow reports the FULL reserved cost — the pinned subset
// alone would under-report what did not fit (spec 5).
// The Cause rows additionally pin the three-way must-fit attribution: an
// unresolved tool chain is tool_output, not the pinned span it is not part of.
func TestAssembleWithTraceMustFitPressure(t *testing.T) {
	// The unresolved chain costs 51: assistant calls(40) + the one answered
	// result(11). Only c1 is answered, so the whole group is non-droppable.
	unresolvedTail := []Message{
		mixedAsstCall("c1", "c2"),
		mixedToolResult("c1", "R", validSet(), 4096),
	}
	mustFitState := func(pinnedContent string) State {
		return State{Messages: append([]Message{pinned("system", pinnedContent)}, unresolvedTail...)}
	}
	// pinnedSpanWithTailState is the two-BASIS trap. chainAt bundles the Elastic
	// tool result under the Pinned assistant into ONE span and classifyGroup calls
	// the whole span pinned, so the unit reserves 120 (call 20 + result 100) while
	// only 20 of it is a pinned MESSAGE. Weighing whole-span unresolved cost (51)
	// against the pinned-message subtotal reports tool_output — blaming a 51-token
	// tool tail for a 120-token pinned span, the very mis-attribution this
	// function exists to prevent, pointing the other way.
	pinnedSpanWithTailState := func() State {
		asst := mixedAsstCall("p1")
		asst.Segment = Pinned
		return State{Messages: append([]Message{
			asst,
			mixedToolResult("p1", strings.Repeat("R", 90), nil, 0),
		}, unresolvedTail...)}
	}
	tests := []struct {
		name             string
		st               State
		toolSchemaTokens int
		budget           int
		wantTokens       int
		pinnedOnly       int
		wantCause        PressureCause
	}{
		{"unresolved chain dominates", mustFitState("SYS-PROMPT"), 0, 30, 61, 10, CauseToolOutput},
		{"tool schemas counted too", mustFitState("SYS-PROMPT"), 5, 30, 66, 15, CauseToolOutput},
		{"pinned span dominates", mustFitState(strings.Repeat("P", 60)), 0, 100, 111, 60, CausePinned},
		{"tool schemas dominate", mustFitState("SYS-PROMPT"), 500, 520, 561, 510, CauseToolSchema},
		{"pinned span bundling an elastic tool tail dominates", pinnedSpanWithTailState(), 0, 100, 171, 20, CausePinned},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			m := ContextManager{Mixed: true, Estimate: runeEstimator}
			_, pressure, tr, err := m.AssembleWithTrace(context.Background(), st,
				tc.toolSchemaTokens, TokenBudget{Input: tc.budget})
			if !errors.Is(err, ErrContextExhausted) {
				t.Fatalf("err = %v, want ErrContextExhausted", err)
			}
			if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
				t.Errorf("error path must return a zero trace, got %+v", tr)
			}
			if pressure.InputTokens != tc.wantTokens {
				t.Errorf("InputTokens = %d, want %d (pinned + unresolved chain + schemas, not the pinned subset %d)",
					pressure.InputTokens, tc.wantTokens, tc.pinnedOnly)
			}
			if pressure.InputBudget != tc.budget {
				t.Errorf("InputBudget = %d, want %d", pressure.InputBudget, tc.budget)
			}
			if pressure.Level != LevelCritical || pressure.Mitigation != MitigationHalt {
				t.Errorf("pressure = %v/%v, want critical/halt", pressure.Level, pressure.Mitigation)
			}
			if pressure.Cause != tc.wantCause {
				t.Errorf("Cause = %v, want %v (attribution must name the bucket that actually overflowed)",
					pressure.Cause, tc.wantCause)
			}
		})
	}
}

// TestAssembleWithTracePinnedOverflow keeps the pinned pre-check ahead of
// allocation on the mixed path, matching legacy behavior.
func TestAssembleWithTracePinnedOverflow(t *testing.T) {
	st := State{System: "SYS", Messages: []Message{
		pinned("user", "A-VERY-LONG-GOAL"),
		mixedAsstCall("c1"),
		mixedToolResult("c1", traceFallback, validSet(), 4096),
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	_, pressure, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 5})
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("err = %v, want ErrContextExhausted", err)
	}
	if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
		t.Errorf("error path must return a zero trace, got %+v", tr)
	}
	if want := m.pinnedTokens(materializeDurableSummary(st), 0); pressure.InputTokens != want {
		t.Errorf("InputTokens = %d, want the pinned cost %d", pressure.InputTokens, want)
	}
	if pressure.Level != LevelCritical || pressure.Mitigation != MitigationHalt {
		t.Errorf("pressure = %v/%v, want critical/halt", pressure.Level, pressure.Mitigation)
	}
}

// TestAssembleWithTraceRejectsMixedCompactor: Mixed plus a custom Compactor is a
// configuration error, unconditionally. The no-anchor row is the load-bearing
// one — checking the compactor AFTER the anchor short-circuit would make the
// error intermittent and data-dependent, appearing only once some tool happened
// to attach a structured payload.
func TestAssembleWithTraceRejectsMixedCompactor(t *testing.T) {
	anchored := mixedTraceState()
	bare := mixedTraceState()
	bare.Messages[6].Context = nil

	tests := []struct {
		name      string
		mixed     bool
		compactor Compactor
		st        State
		wantErr   error
	}{
		{"mixed with a custom compactor and anchors", true, RecencyCompactor{Estimate: runeEstimator}, anchored, ErrMixedCompactor},
		{"mixed with a custom compactor and NO anchors", true, RecencyCompactor{Estimate: runeEstimator}, bare, ErrMixedCompactor},
		{"legacy with a custom compactor", false, RecencyCompactor{Estimate: runeEstimator}, anchored, nil},
		{"mixed with the default compactor", true, nil, anchored, nil},
		{"mixed with the default compactor and no anchors", true, nil, bare, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ContextManager{Mixed: tc.mixed, Compactor: tc.compactor, Estimate: runeEstimator}
			_, _, tr, err := m.AssembleWithTrace(context.Background(), tc.st, 0, TokenBudget{Input: 200})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
					t.Errorf("error path must return a zero trace, got %+v", tr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// TestAssembleWithTraceLegacyPath: both ways into legacy assembly return a ZERO
// trace and messages byte-identical to a manager configured with an explicit
// RecencyCompactor — including the untouched fallback Content, the retained
// runtime Context and the fallback attribution, none of which legacy mode
// rewrites.
func TestAssembleWithTraceLegacyPath(t *testing.T) {
	anchored := mixedTraceState()
	bare := mixedTraceState()
	bare.Messages[6].Context = nil

	tests := []struct {
		name       string
		m          ContextManager
		st         State
		wantAnchor bool // input carried a structured payload
	}{
		{"mixed off with a structured anchor present", ContextManager{Estimate: runeEstimator}, anchored, true},
		{"mixed on with no structured anchor", ContextManager{Mixed: true, Estimate: runeEstimator}, bare, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			legacy := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
			wantOut, wantPressure, wantErr := legacy.Assemble(context.Background(), tc.st, 0, TokenBudget{Input: 200})
			if wantErr != nil {
				t.Fatalf("legacy oracle failed: %v", wantErr)
			}

			out, pressure, tr, err := tc.m.AssembleWithTrace(context.Background(), tc.st, 0, TokenBudget{Input: 200})
			if err != nil {
				t.Fatalf("AssembleWithTrace: %v", err)
			}
			if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
				t.Errorf("legacy assembly must return a zero trace, got %+v", tr)
			}
			if !reflect.DeepEqual(out, wantOut) {
				t.Errorf("legacy state diverged:\n got %+v\nwant %+v", out, wantOut)
			}
			if pressure != wantPressure {
				t.Errorf("pressure = %+v, want %+v", pressure, wantPressure)
			}
			// Content pin: legacy never joins alternatives, never strips Context
			// and never rebuilds Attrib.
			anchor := out.Messages[len(out.Messages)-1]
			if anchor.Content != traceFallback {
				t.Errorf("legacy anchor Content = %q, want the fallback %q", anchor.Content, traceFallback)
			}
			if (anchor.Context != nil) != tc.wantAnchor {
				t.Errorf("legacy anchor Context present = %v, want %v (legacy leaves it alone)",
					anchor.Context != nil, tc.wantAnchor)
			}
			if anchor.Attrib == nil || anchor.Attrib.Sources[0].StableKey != traceFallbackKey {
				t.Errorf("legacy anchor Attrib = %+v, want the fallback attribution", anchor.Attrib)
			}
		})
	}
}

// TestAssembleWithTraceMalformedSet: a hand-built set reaching a COMPLETED chain
// is validated, and the error names the offending call.
func TestAssembleWithTraceMalformedSet(t *testing.T) {
	bad := setWith(func(s *ContextSet) { s.Groups[0].Alternatives = nil })
	st := State{Messages: []Message{
		mixedAsstCall("c9"),
		mixedToolResult("c9", traceFallback, bad, 512),
	}}
	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	_, pressure, tr, err := m.AssembleWithTrace(context.Background(), st, 0, TokenBudget{Input: 200})
	if err == nil {
		t.Fatal("malformed ContextSet accepted")
	}
	for _, want := range []string{`"c9"`, "no alternatives"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if !reflect.DeepEqual(tr, ContextAssemblyTrace{}) {
		t.Errorf("error path must return a zero trace, got %+v", tr)
	}
	if pressure != (Pressure{}) {
		t.Errorf("validation failure carries no pressure, got %+v", pressure)
	}
}

// TestAssembleDiscardsTrace: Assemble is AssembleWithTrace minus the trace.
func TestAssembleDiscardsTrace(t *testing.T) {
	st := mixedTraceState()
	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	budget := TokenBudget{Input: 200}

	wantOut, wantPressure, tr, err := m.AssembleWithTrace(context.Background(), st, 0, budget)
	if err != nil {
		t.Fatalf("AssembleWithTrace: %v", err)
	}
	if len(tr.Subjects) == 0 {
		t.Fatal("fixture no longer produces a mixed trace; the comparison would be vacuous")
	}
	out, pressure, err := m.Assemble(context.Background(), st, 0, budget)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !reflect.DeepEqual(out, wantOut) {
		t.Errorf("Assemble state diverged from AssembleWithTrace:\n got %+v\nwant %+v", out, wantOut)
	}
	if pressure != wantPressure {
		t.Errorf("Assemble pressure = %+v, want %+v", pressure, wantPressure)
	}
}

// TestContextAssemblyTraceIsContentFree: traces carry descriptors, costs and
// fixed vocabulary — never rendered block text. Slice fields marshal [], not
// null.
func TestContextAssemblyTraceIsContentFree(t *testing.T) {
	m := ContextManager{Mixed: true, Estimate: runeEstimator}
	rendered := mixedTraceState()
	omitted := mixedOmitState(4096)

	for _, tc := range []struct {
		name   string
		st     State
		budget int
	}{
		{"rendered subjects", rendered, 200},
		{"omitted subject", omitted, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, tr, err := m.AssembleWithTrace(context.Background(), tc.st, 0, TokenBudget{Input: tc.budget})
			if err != nil {
				t.Fatalf("AssembleWithTrace: %v", err)
			}
			blob, err := json.Marshal(tr)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(blob)
			for _, banned := range []string{
				traceCardContent, traceEvidenceContent, traceFallback, traceFallbackKey,
				omittedObservation, "HQ", "GOAL", "PQ", strings.Repeat("X", 10),
			} {
				if strings.Contains(got, banned) {
					t.Errorf("trace leaked content %q: %s", banned, got)
				}
			}
			if strings.Contains(got, "null") {
				t.Errorf("nil slice marshaled as null: %s", got)
			}
			if !strings.Contains(got, `"Subjects":[`) {
				t.Errorf("Subjects must marshal as an array: %s", got)
			}
		})
	}
}

// structuredTool returns a flat fallback plus a one-group ContextSet, so a run
// through agent.New really carries structure into the next assembly.
type structuredTool struct{}

func (structuredTool) Spec() ToolSpec {
	return ToolSpec{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (structuredTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (structuredTool) Invoke(_ context.Context, _ json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: traceFallback, Context: &ContextSet{Groups: []ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			Rank:    1,
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: traceCardContent,
		}},
	}}}}, nil
}

// toolThenAnswerCaller records every request it is given, calls the tool once,
// then answers.
type toolThenAnswerCaller struct{ reqs []provider.ChatRequest }

func (c *toolThenAnswerCaller) Chat(_ context.Context, req provider.ChatRequest,
	_ func(provider.ChatResponse) error) (ModelResult, error) {
	c.reqs = append(c.reqs, req)
	if len(c.reqs) == 1 {
		return ModelResult{Response: provider.ChatResponse{
			ToolCalls: []provider.ToolCall{tc("lookup", `{}`)},
		}}, nil
	}
	return ModelResult{Response: provider.ChatResponse{Content: "final", Done: true}}, nil
}

// TestMixedThroughAgentNew is the constructor test: New must store the
// ContextManager UNNORMALIZED. Installing the default compactor there makes
// every orchestrator-built mixed manager look like it carries a custom one, and
// the run dies on ErrMixedCompactor — a defect invisible to any test that calls
// ContextManager directly.
func TestMixedThroughAgentNew(t *testing.T) {
	caller := &toolThenAnswerCaller{}
	o := New(caller, ContextManager{Mixed: true, Estimate: runeEstimator})

	res, err := o.Run(context.Background(), Request{
		Goal:   "q",
		Budget: Budget{InputCeiling: 4096},
		Tools:  []Tool{structuredTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "final" || res.StopReason != Completed {
		t.Fatalf("answer = %q stop = %v", res.Answer, res.StopReason)
	}
	if len(caller.reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(caller.reqs))
	}
	// Second turn: the model-visible anchor must be the structured rendering,
	// proving mixed assembly actually ran through the constructed orchestrator.
	var toolMsgs []string
	for _, msg := range caller.reqs[1].Messages {
		if msg.Role == "tool" {
			toolMsgs = append(toolMsgs, msg.Content)
		}
	}
	if !slices.Equal(toolMsgs, []string{traceCardContent}) {
		t.Errorf("model-visible tool messages = %q, want %q (mixed assembly did not run)",
			toolMsgs, []string{traceCardContent})
	}
	// The canonical transcript keeps the fallback rendering (spec 4.2).
	var canonical []string
	for _, msg := range res.Messages {
		if msg.Role == "tool" {
			canonical = append(canonical, msg.Content)
		}
	}
	if !slices.Equal(canonical, []string{traceFallback}) {
		t.Errorf("Result.Messages tool content = %q, want the fallback %q", canonical, []string{traceFallback})
	}
}
