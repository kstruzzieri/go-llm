package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// mixedAnchorSet is a two-group structured payload whose Rank values (7, 3)
// deliberately differ from their group indices, so a builder that took rank
// from the loop index cannot pass.
func mixedAnchorSet() *ContextSet {
	s := validSet()
	s.MinVerbatim = 2
	s.Groups[0].Desc.Rank = 7
	g := s.Groups[0]
	g.Desc.Subject.ID = "pkg/other.go"
	g.Desc.Rank = 3
	s.Groups = append(s.Groups, g)
	return s
}

// mixedAsstCall builds one assistant message calling the retrieve tool once
// per id. Costs under runeEstimator: 8 ("function") + 8 ("retrieve") + 2
// ("{}") + len(id) per call.
func mixedAsstCall(ids ...string) Message {
	msg := Message{ChatMessage: provider.ChatMessage{Role: "assistant"}, Segment: Elastic}
	for _, id := range ids {
		msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
			ID: id, Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{}`)},
		})
	}
	return msg
}

// mixedToolResult builds one tool-role result. A non-nil set makes it a
// structured anchor; outputCap mirrors the normalized Effect.OutputCap.
func mixedToolResult(callID, content string, set *ContextSet, outputCap int) Message {
	return Message{
		ChatMessage: provider.ChatMessage{
			Role: "tool", ToolName: "retrieve", ToolCallID: callID, Content: content,
		},
		Segment:   Elastic,
		Context:   set,
		OutputCap: outputCap,
	}
}

func TestBuildMixedUnitsGrouping(t *testing.T) {
	set := mixedAnchorSet()
	st := State{Messages: []Message{
		history("user", "HIST-Q"),                         // 0 \ prior-history exchange
		history("assistant", "HIST-A"),                    // 1 / (ends before the first pinned)
		history("user", "HIST2-QQ"),                       // 2 \ second prior-history exchange:
		history("assistant", "HIST2-AA"),                  // 3 / unit index 1, message index 2
		pinned("system", "SYS-PROMPT"),                    // 4   pinned
		elastic("user", "PLAIN-Q"),                        // 5 \ current-run plain exchange
		elastic("assistant", "PLAIN-A"),                   // 6 /
		mixedAsstCall("c1"),                               // 7 \ completed chain, structured result
		mixedToolResult("c1", "FLAT-FALLBACK", set, 4096), // 8 /
		mixedAsstCall("c2"),                               // 9 \ completed chain, no Context
		mixedToolResult("c2", "PLAIN-RESULT", nil, 0),     // 10 /
		mixedAsstCall("c3"),                               // 11  unresolved chain (no result)
	}}

	m := ContextManager{Estimate: runeEstimator}
	units, err := m.buildMixedUnits(st)
	if err != nil {
		t.Fatalf("buildMixedUnits: %v", err)
	}

	// Lanes are asserted as LITERALS: deriving them from the constants under
	// test would survive a lanePlain/laneHistory swap.
	want := []struct {
		kind       mixedUnitKind
		msgs       int
		baseTokens int
		lane       int    // -1 => must-fit (pinned/unresolved): no span subject
		convID     string // conversation subject ID: decimal FIRST-message index
	}{
		{unitHistorySpan, 2, 12, 2, "0"}, // HIST-Q(6) + HIST-A(6)
		{unitHistorySpan, 2, 16, 2, "2"}, // HIST2-QQ(8) + HIST2-AA(8); unit 1, message 2
		{unitPinned, 1, 10, -1, ""},      // SYS-PROMPT(10)
		{unitPlainSpan, 2, 14, 0, "5"},   // PLAIN-Q(7) + PLAIN-A(7); unit 3, message 5
		{unitChain, 2, 30, 1, "7"},       // call(20) + ENVELOPE(10); full result cost would be 43
		{unitChain, 2, 42, 1, "9"},       // call(20) + unstructured result verbatim(22)
		{unitUnresolved, 1, 20, -1, ""},  // call(20)
	}
	if len(units) != len(want) {
		t.Fatalf("got %d units, want %d", len(units), len(want))
	}
	for i, w := range want {
		u := units[i]
		if u.kind != w.kind {
			t.Errorf("unit %d: kind = %d, want %d", i, u.kind, w.kind)
		}
		if len(u.msgs) != w.msgs {
			t.Errorf("unit %d: %d messages, want %d (spans are atomic)", i, len(u.msgs), w.msgs)
		}
		if u.baseTokens != w.baseTokens {
			t.Errorf("unit %d: baseTokens = %d, want %d", i, u.baseTokens, w.baseTokens)
		}
		if w.lane < 0 {
			if u.subject != nil {
				t.Errorf("unit %d: unexpected span subject %+v", i, u.subject)
			}
			continue
		}
		s := u.subject
		if s == nil {
			t.Fatalf("unit %d: missing span subject", i)
		}
		if s.lane != w.lane {
			t.Errorf("unit %d: lane = %d, want %d", i, s.lane, w.lane)
		}
		wantRef := contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: w.convID}
		if s.ref != wantRef {
			t.Errorf("unit %d: ref = %+v, want %+v", i, s.ref, wantRef)
		}
		if !s.span || s.spanTokens != w.baseTokens || s.chosen != -1 || s.toolCallID != "" {
			t.Errorf("unit %d: span subject = %+v", i, s)
		}
	}

	// Span atomicity: the exchange messages stay together, in input order.
	if units[0].msgs[0].Content != "HIST-Q" || units[0].msgs[1].Content != "HIST-A" {
		t.Errorf("history span = %+v", units[0].msgs)
	}
	if units[3].msgs[0].Content != "PLAIN-Q" || units[3].msgs[1].Content != "PLAIN-A" {
		t.Errorf("plain span = %+v", units[3].msgs)
	}

	// Structured anchor extraction.
	if len(units[4].anchors) != 1 {
		t.Fatalf("structured chain: %d anchors, want 1", len(units[4].anchors))
	}
	a := units[4].anchors[0]
	if a.callID != "c1" || a.msgIdx != 1 || a.cap != 4096 || a.minVerbatim != 2 {
		t.Errorf("anchor = %+v, want callID c1 msgIdx 1 cap 4096 minVerbatim 2", a)
	}
	if a.set != st.Messages[8].Context {
		t.Errorf("anchor set = %p, want the message's own set %p", a.set, st.Messages[8].Context)
	}
	wantSubjects := []struct {
		id   string
		rank int
	}{{"pkg/doc.go", 7}, {"pkg/other.go", 3}}
	if len(a.subjects) != len(wantSubjects) {
		t.Fatalf("anchor: %d subjects, want %d", len(a.subjects), len(wantSubjects))
	}
	for i, ws := range wantSubjects {
		s := a.subjects[i]
		wantRef := contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: ws.id}
		if s.ref != wantRef {
			t.Errorf("subject %d: ref = %+v, want %+v", i, s.ref, wantRef)
		}
		if s.rank != ws.rank {
			t.Errorf("subject %d: rank = %d, want %d (from GroupDesc.Rank, not the loop index)", i, s.rank, ws.rank)
		}
		if s.lane != 1 {
			t.Errorf("subject %d: lane = %d, want 1", i, s.lane)
		}
		if s.toolCallID != "c1" || s.chosen != -1 || s.span || s.spanTokens != 0 {
			t.Errorf("subject %d = %+v", i, s)
		}
		if len(s.alts) != 1 || s.alts[0].Content != set.Groups[i].Alternatives[0].Content {
			t.Errorf("subject %d: alts = %+v", i, s.alts)
		}
	}

	// A completed chain with no Context, and an unresolved chain, yield no
	// anchors. The unstructured chain still carries its chain-level subject
	// (asserted in the table above) — its span would otherwise be evictable
	// with no subject to record the eviction against.
	if len(units[5].anchors) != 0 || len(units[6].anchors) != 0 {
		t.Errorf("unstructured chains must yield no anchors: %+v %+v", units[5].anchors, units[6].anchors)
	}
	// The structured chain carries the chain subject IN ADDITION to one
	// subject per group.
	if units[4].subject == nil || len(units[4].anchors[0].subjects) != 2 {
		t.Errorf("structured chain: chain subject %+v, anchor subjects %d, want non-nil and 2",
			units[4].subject, len(units[4].anchors[0].subjects))
	}
}

// TestBuildMixedUnitsChainWithTwoAnchors covers the parallel-tools shape one
// assistant message answered by two structured results — and pins that the
// SAME subject ref reached through two different calls is legal (duplicates
// are a within-set rule; a cross-anchor seen map would reject valid input).
func TestBuildMixedUnitsChainWithTwoAnchors(t *testing.T) {
	setA, setB := validSet(), validSet() // identical subject ref, different calls
	st := State{Messages: []Message{
		mixedAsstCall("c1", "c2"),                // 20 + 20
		mixedToolResult("c1", "AAA", setA, 4096), // envelope 10 ("retrieve" + "c1")
		mixedToolResult("c2", "BBBB", setB, 512), // envelope 10
	}}

	m := ContextManager{Estimate: runeEstimator}
	units, err := m.buildMixedUnits(st)
	if err != nil {
		t.Fatalf("buildMixedUnits: %v", err)
	}
	if len(units) != 1 || units[0].kind != unitChain {
		t.Fatalf("units = %+v, want one unitChain", units)
	}
	if units[0].baseTokens != 60 {
		t.Errorf("baseTokens = %d, want 60 (two calls + two envelopes; verbatim would be 67)", units[0].baseTokens)
	}
	want := []struct {
		callID string
		msgIdx int
		cap    int
		set    *ContextSet
	}{{"c1", 1, 4096, setA}, {"c2", 2, 512, setB}}
	if len(units[0].anchors) != len(want) {
		t.Fatalf("%d anchors, want %d", len(units[0].anchors), len(want))
	}
	for i, w := range want {
		a := units[0].anchors[i]
		if a.callID != w.callID || a.msgIdx != w.msgIdx || a.cap != w.cap || a.set != w.set {
			t.Errorf("anchor %d: callID %q msgIdx %d cap %d set %p, want %q %d %d %p",
				i, a.callID, a.msgIdx, a.cap, a.set, w.callID, w.msgIdx, w.cap, w.set)
		}
		if len(a.subjects) != 1 || a.subjects[0].toolCallID != w.callID {
			t.Errorf("anchor %d subjects = %+v", i, a.subjects)
		}
	}
	if units[0].anchors[0].subjects[0].ref != units[0].anchors[1].subjects[0].ref {
		t.Fatal("fixture no longer exercises the cross-call duplicate subject")
	}
	cs := units[0].subject
	if cs == nil {
		t.Fatal("chain has no chain-level subject")
	}
	wantRef := contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: "0"}
	if cs.ref != wantRef || cs.lane != 1 || !cs.span || cs.spanTokens != 60 || cs.chosen != -1 {
		t.Errorf("chain subject = %+v, want ref %+v lane 1 span spanTokens 60 chosen -1", cs, wantRef)
	}
}

func TestBuildMixedUnitsHistoryWithSummaryBoundary(t *testing.T) {
	st := State{
		DurableSummary: "PRIOR SUMMARY",
		Messages: []Message{
			history("user", "HIST-Q"),        // 0 \ prior history
			history("assistant", "HIST-A"),   // 1 /
			history("user", "HIST2-QQ"),      // 2 \ second exchange: unit 1, message 2
			history("assistant", "HIST2-AA"), // 3 /
			pinned("system", "SYS-PROMPT"),   // 4
			elastic("user", "PLAIN-Q"),       // 5 \ current-run plain exchange
			elastic("assistant", "PLAIN-A"),  // 6 /
		},
	}

	m := ContextManager{Estimate: runeEstimator}
	units, err := m.buildMixedUnits(st)
	if err != nil {
		t.Fatalf("buildMixedUnits: %v", err)
	}
	// Materializing first would prepend a pinned message, zero
	// firstPinnedIndex, and reclassify both history exchanges as plain (and
	// shift their IDs to "1" and "3"). The per-unit assertions run BEFORE the
	// unit count so the count Fatalf cannot mask them.
	if len(units) < 2 {
		t.Fatalf("got %d units, want at least the two history exchanges", len(units))
	}
	// IDs are the FIRST-MESSAGE indices: unit 1 sits at message 2, so a builder
	// numbering by unit would report "1" here.
	for i, wantID := range []string{"0", "2"} {
		u := units[i]
		if u.kind != unitHistorySpan {
			t.Errorf("unit %d: kind = %d, want unitHistorySpan (%d)", i, u.kind, unitHistorySpan)
			continue
		}
		if u.subject.lane != 2 {
			t.Errorf("unit %d: history lane = %d, want 2", i, u.subject.lane)
		}
		if u.subject.ref.ID != wantID {
			t.Errorf("unit %d: history subject ID = %q, want %q (pre-materialization index)", i, u.subject.ref.ID, wantID)
		}
	}
	if units[0].msgs[0].Content != "HIST-Q" {
		t.Errorf("unit 0 first message = %q, want the caller-visible history head", units[0].msgs[0].Content)
	}
	if len(units) != 4 {
		t.Errorf("got %d units, want 4 (summary is materialized by the caller, not here)", len(units))
	}
}

// TestMixedUnitForUnhandledKind pins the fail-closed default arm. It is
// unreachable through buildMixedUnits today — classifyGroup returns one of five
// kinds — so the group is constructed directly rather than adding a sixth kind
// to compactor.go. Without the arm a future kind would silently land at
// unitPinned (never droppable) with baseTokens 0 (invisible to the ledger).
func TestMixedUnitForUnhandledKind(t *testing.T) {
	m := ContextManager{Estimate: runeEstimator}
	g := compactionGroup{msgs: []Message{elastic("user", "FUTURE")}, tokens: 6, kind: compactionGroupKind(99)}
	u, err := m.mixedUnitFor(g, 7)
	if err == nil {
		t.Fatalf("mixedUnitFor accepted kind 99: %+v", u)
	}
	for _, want := range []string{"unhandled group kind 99", "at message 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestBuildMixedUnitsChainBijection(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want string
	}{{
		name: "blank assistant tool-call ID",
		msgs: []Message{mixedAsstCall(""), mixedToolResult("", "R", nil, 0)},
		want: "assistant call with blank tool-call ID",
	}, {
		name: "duplicate assistant tool-call IDs",
		msgs: []Message{
			mixedAsstCall("c1", "c1"),
			mixedToolResult("c1", "R", nil, 0),
			mixedToolResult("c1", "R", nil, 0),
		},
		want: `duplicate tool-call ID "c1" in one assistant message`,
	}, {
		name: "blank result call ID",
		msgs: []Message{mixedAsstCall("c1"), mixedToolResult("", "R", nil, 0)},
		want: `tool result with unknown or blank call ID ""`,
	}, {
		name: "unknown result call ID",
		msgs: []Message{mixedAsstCall("c1"), mixedToolResult("zz", "R", nil, 0)},
		want: `tool result with unknown or blank call ID "zz"`,
	}, {
		name: "two results for one call ID",
		msgs: []Message{
			mixedAsstCall("c1", "c2"),
			mixedToolResult("c1", "R", nil, 0),
			mixedToolResult("c1", "R", nil, 0),
		},
		want: `multiple tool results for call ID "c1"`,
	}}

	m := ContextManager{Estimate: runeEstimator}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			units, err := m.buildMixedUnits(State{Messages: tc.msgs})
			if err == nil {
				t.Fatalf("buildMixedUnits: no error, got %d units", len(units))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBuildMixedUnitsValidation(t *testing.T) {
	bad := setWith(func(s *ContextSet) { s.Groups[0].Desc.Subject.ID = "" })
	st := State{Messages: []Message{
		mixedAsstCall("c9"),
		mixedToolResult("c9", "FLAT", bad, 512),
	}}

	m := ContextManager{Estimate: runeEstimator}
	_, err := m.buildMixedUnits(st)
	if err == nil {
		t.Fatal("buildMixedUnits: malformed ContextSet accepted")
	}
	for _, want := range []string{`"c9"`, "blank subject ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}
