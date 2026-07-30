package agent

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// gateAnchor is the minimal thing that turns mixed assembly ON without
// perturbing any lane's allocation: an EMPTY ContextSet riding a PINNED
// message. hasStructuredAnchor only tests Context != nil, and a set on a pinned
// span never reaches validateContextSet, never becomes a mixedAnchor and never
// has its Content reallocated — only completed tool chains build anchors. So the
// message is charged and rendered identically by both paths.
//
// That is what makes a legacy-vs-mixed CONTENT comparison meaningful: every
// other message in these fixtures is charged verbatim by both paths too, so the
// only thing that can differ between the two outputs is WHICH groups survived.
func gateAnchor(m Message) Message {
	m.Context = &ContextSet{}
	return m
}

// msgContents is the assembled transcript as the model would read it.
func msgContents(st State) []string {
	out := make([]string, 0, len(st.Messages))
	for _, m := range st.Messages {
		out = append(out, m.Content)
	}
	return out
}

// plainLaneState: a pinned head (so nothing before it is prior history) followed
// by four equal-cost current-run exchanges. Each exchange costs 4 under
// runeEstimator ("U0"+"A0"), the pinned head 4.
func plainLaneState() State {
	msgs := []Message{gateAnchor(pinned("system", "GATE"))}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, elastic("user", fmt.Sprintf("U%d", i)), elastic("assistant", fmt.Sprintf("A%d", i)))
	}
	return State{Messages: msgs}
}

// chainLaneState: a pinned head followed by four equal-cost COMPLETED tool
// chains. The results carry no ContextSet on purpose — a structured anchor would
// make legacy render its fallback Content while mixed renders an allocated
// alternative, and the two transcripts would differ for a reason that has
// nothing to do with retention order. Each chain costs 34 under runeEstimator:
// call envelope 20 ("c0"+"function"+"retrieve"+"{}"), assistant content 2,
// result 12 ("R0"+"retrieve"+"c0").
func chainLaneState() State {
	msgs := []Message{gateAnchor(pinned("system", "GATE"))}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		call := mixedAsstCall(id)
		call.Content = fmt.Sprintf("g%d", i)
		msgs = append(msgs, call, mixedToolResult(id, fmt.Sprintf("R%d", i), nil, 0))
	}
	return State{Messages: msgs}
}

// historyLaneState: ten equal-cost prior-session exchanges (6 each) ahead of the
// pinned goal, which is what classifyGroup requires to call them groupHistory.
// This is the lane golem drives every turn through Request.History.
func historyLaneState() State {
	var msgs []Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, history("user", fmt.Sprintf("H%dq", i)), history("assistant", fmt.Sprintf("H%da", i)))
	}
	return State{Messages: append(msgs, gateAnchor(pinned("user", "GOAL")))}
}

// TestMixedAssemblyRetainsSameMessagesAsRecencyCompactor pins the single most
// user-visible property of mixed assembly: WHICH messages the model receives
// when the transcript does not fit.
//
// Mixed assembly REPLACES RecencyCompactor whenever a transcript carries
// structured payloads, so its retention policy has to be the same policy.
// RecencyCompactor drops oldest-first and therefore keeps the NEWEST members of
// each kind; mixed assembly must admit newest-first to land on the same set. An
// allocator that walked its lanes in forward State order would keep the OLDEST
// instead — a pressured `golem -progressive` session would hand the model its
// stalest prior exchanges and discard the most recent ones.
//
// The ledger and trace-shape suites cannot see this: every one of them balances
// identically whichever end of the lane got dropped. Only content can tell.
//
// Each case therefore asserts twice, and both assertions are on CONTENT:
//
//  1. mixed == legacy, with the legacy side produced by actually RUNNING the
//     legacy path on the same State and budget. This compares two policies
//     rather than restating one.
//  2. mixed == a hand-written newest-suffix literal. Independent of legacy, so
//     the case still fails if BOTH paths were inverted together.
//
// The literals are also the output-ORDER pin. Admission runs newest-first, so
// the history case admits H9 before H8 — yet want lists H8 ahead of H9, because
// materialization walks units forward. A materializer that emitted in admission
// order would fail assertion 2 while still satisfying assertion 1.
func TestMixedAssemblyRetainsSameMessagesAsRecencyCompactor(t *testing.T) {
	cases := []struct {
		lane   string
		st     State
		budget int
		want   []string
	}{{
		lane: "plain exchanges (lane 0)",
		st:   plainLaneState(),
		// GATE(4) + the two newest exchanges(4 each). Total is 20, so two of the
		// four exchanges must go.
		budget: 4 + 2*4,
		want:   []string{"GATE", "U2", "A2", "U3", "A3"},
	}, {
		lane: "completed tool chains (lane 1)",
		st:   chainLaneState(),
		// GATE(4) + the two newest chains(34 each). Total is 140.
		budget: 4 + 2*34,
		want:   []string{"GATE", "g2", "R2", "g3", "R3"},
	}, {
		lane: "prior history (lane 2)",
		st:   historyLaneState(),
		// GOAL(4) + the two newest exchanges(6 each). Total is 64, so eight of
		// the ten exchanges must go.
		budget: 4 + 2*6,
		want:   []string{"H8q", "H8a", "H9q", "H9a", "GOAL"},
	}}

	for _, tc := range cases {
		t.Run(tc.lane, func(t *testing.T) {
			budget := TokenBudget{Input: tc.budget}

			legacyOut, legacyP, err := ContextManager{Estimate: runeEstimator}.
				Assemble(context.Background(), tc.st, 0, budget)
			if err != nil {
				t.Fatalf("legacy Assemble: %v", err)
			}
			mixedOut, mixedP, err := ContextManager{Mixed: true, Estimate: runeEstimator}.
				Assemble(context.Background(), tc.st, 0, budget)
			if err != nil {
				t.Fatalf("mixed Assemble: %v", err)
			}

			// Without this the whole test could pass on a budget that evicts
			// nothing — the two policies agree trivially when neither runs.
			if legacyP.Evicted == 0 || mixedP.Evicted == 0 {
				t.Fatalf("budget %d evicted nothing (legacy %d, mixed %d); the fixture must not fit",
					tc.budget, legacyP.Evicted, mixedP.Evicted)
			}

			got, legacy := msgContents(mixedOut), msgContents(legacyOut)
			if !slices.Equal(got, legacy) {
				t.Errorf("mixed retained %q, RecencyCompactor retained %q; mixed assembly must keep the same messages as the path it replaces",
					got, legacy)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("mixed retained %q, want %q (the NEWEST members of the lane)", got, tc.want)
			}
		})
	}
}
