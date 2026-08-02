package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// mixedTinyEchoCase is a hand-computable case: fixture_echo attaches no
// ContextSet, so BOTH arms take the legacy assembly path and, under the
// generous control budget, retain everything — the emission numbers are
// derivable by hand. Shape matches TestBuildMixedStateRawTokens (raw 39).
func mixedTinyEchoCase(id string, control bool) mixedCase {
	return mixedCase{
		ID: id, Control: control,
		Stratum: "conversation_only", AnswerHome: "conversation",
		ScenarioFamily: "fam-tiny", TwinGroup: "tw-tiny",
		System: "sys prompt!",
		Events: []mixedEvent{
			{Turn: &mixedTurn{Role: "user", Content: "abcd"}},
			{ToolCall: &mixedToolCall{CallID: "c1", Tool: "fixture_echo",
				Args: json.RawMessage(`{"content":"12345678"}`)}},
			{Turn: &mixedTurn{Role: "user", Content: "q"}},
		},
		Golden: Golden{FinalAnswerCriteria: "crit"},
	}
}

// mixedPressuredCase is a fully valid join case whose early filler exchanges
// make the registered f=0.6 budget bind: both arms must shed, the tool-borne
// evidence sits near the end where recency compaction spares it.
func mixedPressuredCase(id string) mixedCase {
	c := mixedStateCase(id)
	filler1 := "Earlier we digressed at length about venue logistics. " +
		strings.Repeat("The catering plan covers coffee, tea, and pastries for the morning session. ", 20)
	filler2 := "Understood. " +
		strings.Repeat("I will coordinate the room bookings and the projector checks before Friday. ", 20)
	pre := []mixedEvent{
		{Turn: &mixedTurn{Role: "user", Content: filler1}},
		{Turn: &mixedTurn{Role: "assistant", Content: filler2}},
	}
	c.Events = append(pre, c.Events...)
	return c
}

func buildMixedArmsT(t *testing.T, built mixedBuiltState, budget int) (mixedArm, mixedArm) {
	t.Helper()
	legacy, mixed, err := buildMixedArms(context.Background(), built, budget)
	if err != nil {
		t.Fatalf("buildMixedArms: %v", err)
	}
	return legacy, mixed
}

// mixedSynthState builds a user-role message per content string; the last one
// plays the final question in gate unit tests.
func mixedSynthState(contents ...string) agent.State {
	st := agent.State{System: "sys"}
	for _, c := range contents {
		st.Messages = append(st.Messages, agent.Message{
			ChatMessage: provider.ChatMessage{Role: "user", Content: c},
		})
	}
	return st
}

func TestMixedCaseBudgetFormula(t *testing.T) {
	// The registered constants, pinned by literal through their names.
	if mixedBudgetFraction != 0.6 {
		t.Errorf("mixedBudgetFraction = %v; want 0.6", mixedBudgetFraction)
	}
	if mixedEnvelopeTokens != 8 {
		t.Errorf("mixedEnvelopeTokens = %d; want 8", mixedEnvelopeTokens)
	}
	if mixedMinViableSlack != 64 {
		t.Errorf("mixedMinViableSlack = %d; want 64", mixedMinViableSlack)
	}

	// est("s!!!") = 1, est("qq") = 1 => minViable = 1 + 1 + 64 = 66.
	built := mixedBuiltState{
		State: agent.State{System: "s!!!", Messages: []agent.Message{
			{ChatMessage: provider.ChatMessage{Role: "user", Content: "qq"}},
		}},
		RawStateTokens: 1000,
	}
	if got := mixedCaseBudget(built, false); got != 600 {
		t.Errorf("budget(raw=1000) = %d; want 600 (round(0.6*1000))", got)
	}
	built.RawStateTokens = 100
	if got := mixedCaseBudget(built, false); got != 66 {
		t.Errorf("budget(raw=100) = %d; want minViable 66 (0.6*100=60 loses)", got)
	}
	built.RawStateTokens = 1000
	if got := mixedCaseBudget(built, true); got != 1064 {
		t.Errorf("control budget(raw=1000) = %d; want 1064 (raw + slack)", got)
	}
}

func TestMixedArmsEqualBudgetAndEmission(t *testing.T) {
	c := mixedTinyEchoCase("emit-1", true)
	built := buildMixedStateT(t, c)
	if built.RawStateTokens != 39 {
		t.Fatalf("RawStateTokens = %d; want 39 (hand-computed)", built.RawStateTokens)
	}
	budget := mixedCaseBudget(built, c.Control)
	if budget != 103 {
		t.Fatalf("control budget = %d; want 103 (39 + 64)", budget)
	}
	legacy, mixed := buildMixedArmsT(t, built, budget)

	traces := []Trace{
		mixedArmTrace(c, built, budget, legacy),
		mixedArmTrace(c, built, budget, mixed),
	}
	wantIDs := []string{"emit-1-legacy", "emit-1-mixed"}
	wantModes := []AssemblyMode{AssemblyLegacy, AssemblyMixed}
	for i, tr := range traces {
		if tr.ID != wantIDs[i] {
			t.Errorf("trace %d ID = %q; want %q", i, tr.ID, wantIDs[i])
		}
		if tr.Source != "assembly-corpus-mixed" {
			t.Errorf("trace %d Source = %q; want assembly-corpus-mixed", i, tr.Source)
		}
		if tr.System != built.State.System {
			t.Errorf("trace %d System = %q; want the assembled system", i, tr.System)
		}
		if !reflect.DeepEqual(tr.Golden, c.Golden) {
			t.Errorf("trace %d Golden = %+v; want the case golden", i, tr.Golden)
		}
		if err := validateTrace(tr); err != nil {
			t.Errorf("trace %d fails validateTrace: %v", i, err)
		}
		ae := tr.AssemblyEval
		if ae == nil {
			t.Fatalf("trace %d has no AssemblyEval", i)
		}
		if ae.PairID != "emit-1" || ae.Mode != wantModes[i] {
			t.Errorf("trace %d pair/mode = %q/%q; want emit-1/%q", i, ae.PairID, ae.Mode, wantModes[i])
		}
		if ae.Budget != 103 {
			t.Errorf("trace %d Budget = %d; want 103 (identical across both arms)", i, ae.Budget)
		}
		if ae.RawStateTokens != 39 {
			t.Errorf("trace %d RawStateTokens = %d; want 39", i, ae.RawStateTokens)
		}
		if ae.StateDigest != built.StateDigest || ae.StateDigest == "" {
			t.Errorf("trace %d StateDigest = %q; want the built digest %q", i, ae.StateDigest, built.StateDigest)
		}
		if !reflect.DeepEqual(ae.CandidateIDs, built.CandidateIDs) {
			t.Errorf("trace %d CandidateIDs = %v; want %v", i, ae.CandidateIDs, built.CandidateIDs)
		}
		if !reflect.DeepEqual(ae.Subjects, built.Subjects) {
			t.Errorf("trace %d Subjects = %v; want %v", i, ae.Subjects, built.Subjects)
		}
		if ae.Stratum != "conversation_only" || ae.AnswerHome != "conversation" ||
			ae.ScenarioFamily != "fam-tiny" || ae.TwinGroup != "tw-tiny" || !ae.Control {
			t.Errorf("trace %d metadata = %+v; want stratum/home/family/twin/control carried", i, ae)
		}
		// est("sys prompt!")=3 + "abcd"=1 + assistant ""=0 + "12345678"=2 + "q"=1.
		if ae.EstimatedPromptTokens != 7 {
			t.Errorf("trace %d EstimatedPromptTokens = %d; want 7 (hand-computed)", i, ae.EstimatedPromptTokens)
		}
		if ae.PressureLevel != "ok" {
			t.Errorf("trace %d PressureLevel = %q; want ok (control budget is generous)", i, ae.PressureLevel)
		}
		if ae.ShedMessages != 0 || ae.ShedBytes != 0 || ae.OmittedSubjects != 0 {
			t.Errorf("trace %d shed/omitted = %d/%d/%d; want all zero under the control budget",
				i, ae.ShedMessages, ae.ShedBytes, ae.OmittedSubjects)
		}
		// Turn mapping: assistant declares the call with ID+Name+Args, tool
		// answers it with ToolCallID+Name+Content.
		if len(tr.Turns) != 4 {
			t.Fatalf("trace %d turns = %d; want 4", i, len(tr.Turns))
		}
		asst := tr.Turns[1]
		if asst.Role != "assistant" || len(asst.ToolCalls) != 1 ||
			asst.ToolCalls[0].ID != "c1" || asst.ToolCalls[0].Name != "fixture_echo" ||
			string(asst.ToolCalls[0].Arguments) != `{"content":"12345678"}` {
			t.Errorf("trace %d assistant turn = %+v; want the mapped c1/fixture_echo call with args", i, asst)
		}
		tool := tr.Turns[2]
		if tool.Role != "tool" || tool.ToolCallID != "c1" || tool.Name != "fixture_echo" || tool.Content != "12345678" {
			t.Errorf("trace %d tool turn = %+v; want role tool answering c1", i, tool)
		}
	}
}

func TestMixedPressureEvidenceGate(t *testing.T) {
	// Unpressured tiny case: the minViable-floored budget retains everything,
	// so a NON-control case must be rejected loudly...
	un := mixedConvCase("unpressured-1")
	built := buildMixedStateT(t, un)
	legacy, mixed := buildMixedArmsT(t, built, mixedCaseBudget(built, false))
	err := mixedPressureGate(un.ID, false, legacy, mixed)
	if err == nil || !strings.Contains(err.Error(), "unpressured-1") || !strings.Contains(err.Error(), "pressure") {
		t.Errorf("unpressured non-control err = %v; want a pressure-evidence error naming the case", err)
	}
	// ...while the SAME retained-everything shape is exempt for a control case.
	if err := mixedPressureGate(un.ID, true, legacy, mixed); err != nil {
		t.Errorf("control exemption: err = %v; want nil", err)
	}

	// A genuinely pressured case passes AND records per-arm shed evidence.
	p := mixedPressuredCase("pressured-1")
	if err := validateMixedCase(p); err != nil {
		t.Fatalf("pressured fixture case invalid: %v", err)
	}
	pbuilt := buildMixedStateT(t, p)
	budget := mixedCaseBudget(pbuilt, false)
	if budget >= pbuilt.RawStateTokens {
		t.Fatalf("budget %d >= raw %d; fixture not actually pressured", budget, pbuilt.RawStateTokens)
	}
	pl, pm := buildMixedArmsT(t, pbuilt, budget)
	if err := mixedPressureGate(p.ID, false, pl, pm); err != nil {
		t.Fatalf("pressured case gate err = %v; want nil", err)
	}
	for _, arm := range []mixedArm{pl, pm} {
		if arm.shedMessages == 0 && arm.shedBytes == 0 {
			t.Errorf("%s arm recorded no shed evidence (messages=%d bytes=%d); want > 0",
				arm.mode, arm.shedMessages, arm.shedBytes)
		}
	}
	// The recorded evidence must reach the emitted traces, not just the arms.
	for _, arm := range []mixedArm{pl, pm} {
		ae := mixedArmTrace(p, pbuilt, budget, arm).AssemblyEval
		if ae.ShedMessages != arm.shedMessages || ae.ShedBytes != arm.shedBytes {
			t.Errorf("%s trace shed = %d/%d; want the arm's recorded %d/%d",
				arm.mode, ae.ShedMessages, ae.ShedBytes, arm.shedMessages, arm.shedBytes)
		}
		if ae.PressureLevel == "" {
			t.Errorf("%s trace PressureLevel is empty; want the arm's pressure tier", arm.mode)
		}
	}
}

// TestMixedControlSilentTruncation covers the control-hole fix: the control
// budget is raw+slack on the REGISTERED basis, but agent's messageCost also
// prices tool-call args — which for fixture_echo duplicate the echoed content
// — so a fat echo makes agent's total exceed the budget and BOTH arms shed
// identically. The control branch of the pressure gate must catch that
// instead of exempting it, or truncated control traces emit silently.
func TestMixedControlSilentTruncation(t *testing.T) {
	big := strings.Repeat("padding words for the fat echo control case. ", 60)
	args, err := json.Marshal(map[string]string{"content": big})
	if err != nil {
		t.Fatal(err)
	}
	fat := mixedTinyEchoCase("fat-ctl", true)
	fat.Events[1].ToolCall.Args = args
	built := buildMixedStateT(t, fat)
	budget := mixedCaseBudget(built, true)
	legacy, mixed := buildMixedArmsT(t, built, budget)
	// Precondition for the test to mean anything: the arms really shed.
	if legacy.shedMessages == 0 && legacy.shedBytes == 0 {
		t.Fatalf("fat-echo legacy arm did not shed; fixture not fat enough")
	}
	gerr := mixedPressureGate(fat.ID, true, legacy, mixed)
	if gerr == nil || !strings.Contains(gerr.Error(), "control") || !strings.Contains(gerr.Error(), "shed") {
		t.Errorf("fat-echo control: err = %v; want a control-arm-shed error", gerr)
	}
}

// TestMixedControlAnchorFree covers the control-with-anchors misdiagnosis
// fix on both ends: author-time rejection, and (validation bypassed) the
// control-differ error carrying the first differing message index.
func TestMixedControlAnchorFree(t *testing.T) {
	for _, tool := range []mixedToolCall{
		{CallID: "c-r", Tool: "retrieve", Args: json.RawMessage(`{"query":"beta gate"}`)},
		{CallID: "c-m", Tool: "agent_memory_search", Args: json.RawMessage(`{"query":"standup"}`)},
	} {
		c := withMixedToolCall(mixedControlCase("ctl-anchor"), tool)
		err := validateMixedCase(c)
		if err == nil || !strings.Contains(err.Error(), "anchor-free") {
			t.Errorf("control + %s: err = %v; want the anchor-free rejection", tool.Tool, err)
		}
	}

	// Validation bypassed (direct build): mixed assembly rewrites the anchor
	// content even at the generous control budget, so the control-differ gate
	// fires and names the first differing message index.
	c := withMixedToolCall(mixedControlCase("ctl-anchor-direct"), mixedToolCall{
		CallID: "c-m1", Tool: "agent_memory_search", Args: json.RawMessage(`{"query":"standup wednesdays"}`),
	})
	built := buildMixedStateT(t, c)
	legacy, mixed := buildMixedArmsT(t, built, mixedCaseBudget(built, true))
	err := mixedArmsDifferGate(c.ID, true, legacy.state, mixed.state)
	if err == nil || !strings.Contains(err.Error(), "first at message 3") ||
		!strings.Contains(err.Error(), "ctl-anchor-direct") {
		t.Errorf("control-with-anchor direct build: err = %v; want a control-differ error carrying index 3 (the tool message)", err)
	}
}

func TestMixedArmsDifferGate(t *testing.T) {
	// Control integration: no structured anchors, generous budget => both arms
	// identical, gate passes and asserts it.
	c := mixedTinyEchoCase("differ-ctl", true)
	built := buildMixedStateT(t, c)
	legacy, mixed := buildMixedArmsT(t, built, mixedCaseBudget(built, true))
	if !mixedArmMessagesEqual(legacy.state, mixed.state) {
		t.Fatalf("control arms differ; want identical (both take the legacy path)")
	}
	if err := mixedArmsDifferGate(c.ID, true, legacy.state, mixed.state); err != nil {
		t.Errorf("identical control arms: err = %v; want nil", err)
	}

	// Unit: a control case with differing arms is a fixture bug.
	a, b := mixedSynthState("same", "q"), mixedSynthState("other", "q")
	err := mixedArmsDifferGate("differ-ctl", true, a, b)
	if err == nil || !strings.Contains(err.Error(), "control") {
		t.Errorf("differing control arms: err = %v; want a control-arms-differ error", err)
	}
	// Unit: non-control identical arms carry no assembly contrast.
	err = mixedArmsDifferGate("differ-nc", false, a, a)
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Errorf("identical non-control arms: err = %v; want an identical-arms error", err)
	}

	// Integration: a pressured NON-control case with no tool calls sheds in
	// both arms, but both arms run the legacy assembler (no anchors), so they
	// come out identical and the gate must reject.
	nc := mixedConvCase("differ-nc-int")
	filler := strings.Repeat("An unrelated aside about scheduling and room capacity limits. ", 25)
	nc.Events = append([]mixedEvent{
		{Turn: &mixedTurn{Role: "user", Content: "Aside: " + filler}},
		{Turn: &mixedTurn{Role: "assistant", Content: "Noted. " + filler}},
	}, nc.Events...)
	nbuilt := buildMixedStateT(t, nc)
	nbudget := mixedCaseBudget(nbuilt, false)
	if nbudget >= nbuilt.RawStateTokens {
		t.Fatalf("budget %d >= raw %d; integration case not pressured", nbudget, nbuilt.RawStateTokens)
	}
	nl, nm := buildMixedArmsT(t, nbuilt, nbudget)
	if err := mixedPressureGate(nc.ID, false, nl, nm); err != nil {
		t.Fatalf("integration case should shed in both arms: %v", err)
	}
	err = mixedArmsDifferGate(nc.ID, false, nl.state, nm.state)
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Errorf("anchor-free non-control arms: err = %v; want an identical-arms error", err)
	}
}

func TestMixedQuestionSurvivalGate(t *testing.T) {
	const q = "What is the gate?"
	tests := []struct {
		name    string
		st      agent.State
		wantErr bool
	}{
		{"exact final question passes", mixedSynthState("ctx", q), false},
		{"question plus trailing text fails identity", mixedSynthState("ctx", q+" tail"), true},
		{"question embedded after prefix fails identity", mixedSynthState("ctx", "Context: "+q), true},
		{"empty state fails", agent.State{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mixedQuestionSurvivalGate("qs-1", AssemblyLegacy, tt.st, q)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v; wantErr = %t", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "qs-1") {
				t.Errorf("err = %v; must name the case", err)
			}
		})
	}
	// Wrong final role: same content, role assistant.
	st := mixedSynthState("ctx", q)
	st.Messages[len(st.Messages)-1].Role = "assistant"
	if err := mixedQuestionSurvivalGate("qs-1", AssemblyMixed, st, q); err == nil {
		t.Errorf("assistant-role final turn: err = nil; want a survival error")
	}
}

func TestMixedEvidenceReachability(t *testing.T) {
	var buf bytes.Buffer

	// A literal that appears ONLY in the final question message reaches
	// neither arm: the scan must exclude the question.
	legacy := mixedSynthState("other text", "the answer is port 7443")
	mixed := mixedSynthState("other text", "the answer is port 7443")
	err := mixedEvidenceGate("reach-1", []mixedEvidence{{Domain: "memory", Literal: "port 7443"}}, legacy, mixed, &buf)
	if err == nil || !strings.Contains(err.Error(), "reach-1") ||
		!strings.Contains(err.Error(), "port 7443") || !strings.Contains(err.Error(), "neither") {
		t.Errorf("question-only literal: err = %v; want reaches-neither naming case and literal", err)
	}

	// Join anchors split across arms: each reaches one arm, no arm holds both.
	legacy = mixedSynthState("holds port 7443 here", "q")
	mixed = mixedSynthState("holds retry ceiling 6 here", "q")
	buf.Reset()
	err = mixedEvidenceGate("reach-2", []mixedEvidence{
		{Domain: "memory", Literal: "port 7443"},
		{Domain: "rag", Literal: "retry ceiling 6"},
	}, legacy, mixed, &buf)
	if err == nil || !strings.Contains(err.Error(), "co-occur") ||
		!strings.Contains(err.Error(), "port 7443") || !strings.Contains(err.Error(), "retry ceiling 6") {
		t.Errorf("split anchors: err = %v; want a co-occurrence error listing anchor placements", err)
	}

	// One anchor in both arms, one in legacy only: co-occurs in legacy, so the
	// gate passes with a single-arm note on the writer.
	legacy = mixedSynthState("port 7443 and retry ceiling 6", "q")
	mixed = mixedSynthState("port 7443 only", "q")
	buf.Reset()
	err = mixedEvidenceGate("reach-3", []mixedEvidence{
		{Domain: "memory", Literal: "port 7443"},
		{Domain: "rag", Literal: "retry ceiling 6"},
	}, legacy, mixed, &buf)
	if err != nil {
		t.Errorf("legacy-co-occurring anchors: err = %v; want nil", err)
	}
	note := buf.String()
	if !strings.Contains(note, "reach-3") || !strings.Contains(note, "retry ceiling 6") ||
		!strings.Contains(note, "legacy arm only") {
		t.Errorf("single-arm note = %q; want it naming case, literal, and the legacy arm", note)
	}

	// Case-SENSITIVE scan: a re-cased occurrence is not a reach.
	legacy = mixedSynthState("PORT 7443", "q")
	mixed = mixedSynthState("PORT 7443", "q")
	buf.Reset()
	err = mixedEvidenceGate("reach-4", []mixedEvidence{{Domain: "memory", Literal: "port 7443"}}, legacy, mixed, &buf)
	if err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("re-cased occurrence: err = %v; want reaches-neither (anchors are verbatim)", err)
	}

	// All anchors in both arms: pass, no notes.
	legacy = mixedSynthState("port 7443", "q")
	mixed = mixedSynthState("port 7443", "q")
	buf.Reset()
	if err := mixedEvidenceGate("reach-5", []mixedEvidence{{Domain: "memory", Literal: "port 7443"}}, legacy, mixed, &buf); err != nil {
		t.Errorf("both-arm reach: err = %v; want nil", err)
	}
	if buf.Len() != 0 {
		t.Errorf("both-arm reach wrote notes: %q; want none", buf.String())
	}
}

// --- Wave 2 (round-2 consult): build-side hardening tests. ---

// Synthetic message constructors for gate unit tests.
func mixedTestUser(content string) agent.Message {
	return agent.Message{ChatMessage: provider.ChatMessage{Role: "user", Content: content}}
}

func mixedTestTool(callID, content string) agent.Message {
	return agent.Message{ChatMessage: provider.ChatMessage{
		Role: "tool", Content: content, ToolCallID: callID, ToolName: "retrieve",
	}}
}

func mixedTestAsst(callID, args string) agent.Message {
	return agent.Message{ChatMessage: provider.ChatMessage{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{{ID: callID, Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(args)}}},
	}}
}

func mixedTestState(msgs ...agent.Message) agent.State {
	return agent.State{System: "sys", Messages: msgs}
}

func TestMixedPressureTargetGate(t *testing.T) {
	target := mixedEvidence{Domain: "memory", Literal: "port 7443"}
	full := mixedTestState(
		mixedTestUser("filler about logistics"),
		mixedTestTool("c1", "carrier holds port 7443 today"),
		mixedTestUser("q"),
	)

	// Unchanged carrier in both arms: pressure theater, named loudly.
	err := mixedPressureTargetGate("pt-1", target, full, full, full)
	if err == nil || !strings.Contains(err.Error(), "pressure theater") ||
		!strings.Contains(err.Error(), "pt-1") || !strings.Contains(err.Error(), "port 7443") {
		t.Errorf("unchanged carrier: err = %v; want a pressure-theater error naming case and target", err)
	}

	// Shedding ONLY answer-irrelevant filler while the carrier survives
	// unchanged is exactly the theater the gate exists to kill.
	fillerShed := mixedTestState(
		mixedTestTool("c1", "carrier holds port 7443 today"),
		mixedTestUser("q"),
	)
	err = mixedPressureTargetGate("pt-2", target, full, fillerShed, fillerShed)
	if err == nil || !strings.Contains(err.Error(), "pressure theater") {
		t.Errorf("filler-only shed: err = %v; want a pressure-theater error", err)
	}

	// Re-rendered carrier (same ToolCallID, different Content) in ONE arm
	// passes — direction-neutral in both directions.
	rerendered := mixedTestState(
		mixedTestUser("filler about logistics"),
		mixedTestTool("c1", "compact card: gateway port"),
		mixedTestUser("q"),
	)
	if err := mixedPressureTargetGate("pt-3", target, full, full, rerendered); err != nil {
		t.Errorf("re-rendered carrier in mixed arm: err = %v; want nil", err)
	}
	if err := mixedPressureTargetGate("pt-4", target, full, rerendered, full); err != nil {
		t.Errorf("re-rendered carrier in legacy arm: err = %v; want nil", err)
	}

	// Dropped carrier in one arm passes.
	droppedCarrier := mixedTestState(mixedTestUser("filler about logistics"), mixedTestUser("q"))
	if err := mixedPressureTargetGate("pt-5", target, full, full, droppedCarrier); err != nil {
		t.Errorf("dropped carrier: err = %v; want nil", err)
	}

	// No carrier in the full State at all: loud fixture-bug error.
	err = mixedPressureTargetGate("pt-6", mixedEvidence{Domain: "memory", Literal: "absent-literal"}, full, full, full)
	if err == nil || !strings.Contains(err.Error(), "pt-6") || !strings.Contains(err.Error(), "no ") {
		t.Errorf("carrier-free target: err = %v; want a loud no-carrier fixture-bug error", err)
	}
}

// TestMixedPressureTargetGateEndToEnd proves the gate is wired into the real
// build path: a pressured, otherwise-valid case whose registered target rides
// a late conversation turn that BOTH assemblers retain byte-identical (only
// the early filler sheds) must fail the whole build as pressure theater.
func TestMixedPressureTargetGateEndToEnd(t *testing.T) {
	p := mixedPressuredCase("theater-1")
	// The target must be answer-relevant (W5), so the retained conversation
	// turn is registered as a conversation anchor; stale_vs_fresh admits the
	// join home and its topline quota (2) is met by the padding case below.
	p.Stratum = "stale_vs_fresh"
	p.RequiredEvidence = append(p.RequiredEvidence, mixedEvidence{Domain: "conversation", Literal: "gateway settings yesterday"})
	p.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "gateway settings yesterday"}
	p = withMixedFacts(p)
	if err := validateMixedCase(p); err != nil {
		t.Fatalf("theater fixture case invalid: %v", err)
	}
	pad := mixedPressuredCase("theater-2")
	pad.Stratum = "stale_vs_fresh"
	pad = withMixedFacts(pad)
	raw, err := json.Marshal(mixedFixtureFor(p, pad))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	err = runMixedFixture(context.Background(), raw, t.TempDir(), &buf)
	if err == nil || !strings.Contains(err.Error(), "pressure theater") || !strings.Contains(err.Error(), "theater-1") {
		t.Fatalf("runMixedFixture = %v; want the pressure-theater rejection naming theater-1", err)
	}
}

func TestMixedArmContainsToolCallArgs(t *testing.T) {
	// An anchor visible ONLY through assistant tool-call arguments reaches.
	st := mixedTestState(mixedTestAsst("c1", `{"query":"port 7443"}`), mixedTestUser("q"))
	if !mixedArmContains(st, "port 7443") {
		t.Errorf("anchor in tool-call args not seen by mixedArmContains")
	}
	// The final-message exclusion applies to its args too.
	stFinal := mixedTestState(mixedTestUser("x"), mixedTestAsst("c1", `{"query":"port 7443"}`))
	if mixedArmContains(stFinal, "port 7443") {
		t.Errorf("anchor in the FINAL message's tool-call args must not count")
	}
	// Case-sensitive: a re-cased arg occurrence is not a reach.
	stCase := mixedTestState(mixedTestAsst("c1", `{"query":"PORT 7443"}`), mixedTestUser("q"))
	if mixedArmContains(stCase, "port 7443") {
		t.Errorf("re-cased arg occurrence must not count (anchors are verbatim)")
	}
	// Integration: the evidence gate accepts an args-only reach.
	var buf bytes.Buffer
	if err := mixedEvidenceGate("args-reach", []mixedEvidence{{Domain: "memory", Literal: "port 7443"}}, st, st, &buf); err != nil {
		t.Errorf("args-only reach through the evidence gate: err = %v; want nil", err)
	}
}

func TestMixedArmsDeepDiff(t *testing.T) {
	q := mixedTestUser("q")

	// Identical role+content, differing tool-call ID: the arms now DIFFER.
	a := mixedTestState(mixedTestAsst("c1", `{"query":"x"}`), q)
	b := mixedTestState(mixedTestAsst("c2", `{"query":"x"}`), q)
	if mixedArmMessagesEqual(a, b) {
		t.Errorf("tool-call ID difference not detected by the deep compare")
	}
	if idx, field := mixedArmsFirstDiff(a, b); idx != 0 || field != "tool_calls" {
		t.Errorf("first diff = (%d, %q); want (0, tool_calls)", idx, field)
	}

	// Differing tool-call argument bytes.
	c := mixedTestState(mixedTestAsst("c1", `{"query":"y"}`), q)
	if idx, field := mixedArmsFirstDiff(a, c); idx != 0 || field != "tool_calls" {
		t.Errorf("args diff = (%d, %q); want (0, tool_calls)", idx, field)
	}

	// Differing ToolCallID on a tool message.
	ta := mixedTestState(mixedTestTool("c1", "out"), q)
	tb := mixedTestState(mixedTestTool("c2", "out"), q)
	if idx, field := mixedArmsFirstDiff(ta, tb); idx != 0 || field != "tool_call_id" {
		t.Errorf("tool_call_id diff = (%d, %q); want (0, tool_call_id)", idx, field)
	}

	// Differing ToolName.
	tn := mixedTestState(mixedTestTool("c1", "out"), q)
	tn.Messages[0].ToolName = "agent_memory_search"
	if idx, field := mixedArmsFirstDiff(ta, tn); idx != 0 || field != "tool_name" {
		t.Errorf("tool_name diff = (%d, %q); want (0, tool_name)", idx, field)
	}

	// Identical states still compare equal, and the prefix case reports the
	// shorter length.
	if !mixedArmMessagesEqual(a, a) {
		t.Errorf("identical states compare unequal")
	}
	if idx, field := mixedArmsFirstDiff(a, mixedTestState(a.Messages[0])); idx != 1 || field != "presence" {
		t.Errorf("prefix diff = (%d, %q); want (1, presence)", idx, field)
	}

	// The gate error names the differing field.
	err := mixedArmsDifferGate("dd-1", true, ta, tb)
	if err == nil || !strings.Contains(err.Error(), "tool_call_id") {
		t.Errorf("control-differ error = %v; want it naming the tool_call_id field", err)
	}
}

func TestMixedArmSubsequenceGate(t *testing.T) {
	u1 := mixedTestUser("early context")
	asst := mixedTestAsst("c1", `{"query":"x"}`)
	tool := mixedTestTool("c1", "tool output text")
	q := mixedTestUser("q")
	full := mixedTestState(u1, asst, tool, q)

	tests := []struct {
		name      string
		assembled agent.State
		wantErr   bool
	}{
		{"identity is a subsequence", mixedTestState(u1, asst, tool, q), false},
		{"dropping a message keeps the subsequence", mixedTestState(asst, tool, q), false},
		{"anchor rewrite (same ToolCallID) is legal", mixedTestState(u1, asst, mixedTestTool("c1", "rewritten"), q), false},
		{"reordering rejected", mixedTestState(asst, u1, tool, q), true},
		{"duplication rejected", mixedTestState(u1, u1, asst, tool, q), true},
		{"synthesized message rejected", mixedTestState(mixedTestUser("not in the full state"), q), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mixedArmSubsequenceGate("sub-1", AssemblyMixed, full, tt.assembled)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v; wantErr = %t", err, tt.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), "sub-1") ||
				!strings.Contains(err.Error(), "reordered or duplicated") ||
				!strings.Contains(err.Error(), string(AssemblyMixed))) {
				t.Errorf("err = %v; must name the case, the arm, and the invariant", err)
			}
		})
	}
}

func TestMixedToplineEmission(t *testing.T) {
	// Validation: topline_facts is illegal on control cases.
	ctl := mixedControlCase("top-ctl")
	ctl.ToplineFacts = "some facts"
	if err := validateMixedCase(ctl); err == nil || !strings.Contains(err.Error(), "topline_facts") {
		t.Errorf("control topline_facts: err = %v; want a topline_facts rejection", err)
	}
	// Validation: the facts must contain every required literal, case-sensitively.
	nc := mixedConvCase("top-missing")
	nc.ToplineFacts = "facts that never mention the FLAG-ALPHA-7 gate verbatim in lowercase"
	if err := validateMixedCase(nc); err == nil || !strings.Contains(err.Error(), "flag-alpha-7") {
		t.Errorf("missing literal: err = %v; want a containment rejection naming the literal", err)
	}

	// Emission: the exact prompt shape, pinned.
	c := mixedConvCase("top-1")
	c.ScenarioFamily = "fam-top"
	c.TwinGroup = "tw-top"
	c.ToplineFacts = "For the beta rollout the team chose flag-alpha-7 as the gate."
	if err := validateMixedCase(c); err != nil {
		t.Fatalf("topline fixture case invalid: %v", err)
	}
	tr := mixedToplineTrace(c)
	if tr.ID != "top-1-topline" || tr.Source != "assembly-corpus-mixed" {
		t.Errorf("trace identity = %q/%q; want top-1-topline/assembly-corpus-mixed", tr.ID, tr.Source)
	}
	if tr.System != c.System {
		t.Errorf("System = %q; want the case system", tr.System)
	}
	want := "Facts:\nFor the beta rollout the team chose flag-alpha-7 as the gate.\n\nQuestion: Which gate did we settle on for the beta rollout?"
	if len(tr.Turns) != 1 || tr.Turns[0].Role != "user" || tr.Turns[0].Content != want {
		t.Errorf("turns = %+v; want one user turn with exactly:\n%s", tr.Turns, want)
	}
	if err := validateTrace(tr); err != nil {
		t.Errorf("topline trace fails validateTrace: %v", err)
	}
	ae := tr.AssemblyEval
	if ae.PairID != "top-1" || ae.Mode != AssemblyTopline {
		t.Errorf("pair/mode = %q/%q; want top-1/topline", ae.PairID, ae.Mode)
	}
	if ae.Stratum != "conversation_only" || ae.AnswerHome != "conversation" ||
		ae.ScenarioFamily != "fam-top" || ae.TwinGroup != "tw-top" {
		t.Errorf("carried metadata = %+v; want stratum/home/family/twin", ae)
	}
	if len(ae.CandidateIDs) != 0 || ae.StateDigest != "" || ae.Budget != 0 ||
		ae.RawStateTokens != 0 || ae.ShedMessages != 0 || ae.ShedBytes != 0 ||
		ae.OmittedSubjects != 0 || ae.PressureLevel != "" || ae.Control {
		t.Errorf("topline excluded fields not empty: %+v", ae)
	}
	est := func(s string) int { return (len(s) + 3) / 4 }
	if wantTok := est(c.System) + est(want); ae.EstimatedPromptTokens != wantTok {
		t.Errorf("EstimatedPromptTokens = %d; want %d", ae.EstimatedPromptTokens, wantTok)
	}
}

func TestMixedBuildDeterminism(t *testing.T) {
	// Two pressured cases in stale_vs_fresh (topline quota 2, which the W3
	// exact-quota floor now enforces at author time): the registered rule
	// selects both families, so both carry topline_facts.
	p := mixedPressuredCase("det-press")
	p.Stratum = "stale_vs_fresh" // permits the join answer_home
	p.ScenarioFamily = "fam-press"
	p.ToplineFacts = "The gateway listens on port 7443 and its retry ceiling 6 is enforced."
	p2 := mixedPressuredCase("det-press2")
	p2.Stratum = "stale_vs_fresh"
	p2.ScenarioFamily = "fam-press2"
	p2.ToplineFacts = p.ToplineFacts
	ctl := mixedControlCase("det-ctl")
	fixture := mixedFixtureFor(p, p2, ctl)
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	run := func(dir string) map[string][]byte {
		t.Helper()
		var buf bytes.Buffer
		if err := runMixedFixture(context.Background(), raw, dir, &buf); err != nil {
			t.Fatalf("runMixedFixture: %v\noutput:\n%s", err, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "case det-press gated: budget=") {
			t.Errorf("per-case gate summary missing from output:\n%s", out)
		}
		if !strings.Contains(out, "wrote 8 trace(s)") {
			t.Errorf("trailing wrote-count line missing from output:\n%s", out)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		files := map[string][]byte{}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			files[e.Name()] = data
		}
		return files
	}

	dir1, dir2 := t.TempDir(), t.TempDir()
	files1, files2 := run(dir1), run(dir2)

	wantNames := []string{
		"det-press-legacy.json", "det-press-mixed.json", "det-press-topline.json",
		"det-press2-legacy.json", "det-press2-mixed.json", "det-press2-topline.json",
		"det-ctl-legacy.json", "det-ctl-mixed.json", assemblyManifestName,
	}
	if len(files1) != len(wantNames) {
		names := make([]string, 0, len(files1))
		for n := range files1 {
			names = append(names, n)
		}
		t.Fatalf("output files = %v; want exactly %v", names, wantNames)
	}
	for _, name := range wantNames {
		a, okA := files1[name]
		b, okB := files2[name]
		if !okA || !okB {
			t.Fatalf("file %s missing (run1=%t run2=%t)", name, okA, okB)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("file %s differs between two builds of the same fixture", name)
		}
	}

	// Every emitted trace must load and validate through the normal loader.
	for _, name := range wantNames {
		if name == assemblyManifestName {
			continue
		}
		if _, err := loadTraces([]string{filepath.Join(dir1, name)}); err != nil {
			t.Errorf("emitted trace %s fails loadTraces: %v", name, err)
		}
	}

	// Preflight refuses an unmanifested stray JSON file in the output dir.
	stray := filepath.Join(dir1, "stray-legacy.json")
	if err := os.WriteFile(stray, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	var buf bytes.Buffer
	err = runMixedFixture(context.Background(), raw, dir1, &buf)
	if err == nil || !strings.Contains(err.Error(), "refuse") {
		t.Fatalf("stray file: err = %v; want a preflight refusal", err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatalf("remove stray: %v", err)
	}

	// Rebuild after removing the stray succeeds and stays byte-identical.
	files3 := run(dir1)
	for _, name := range wantNames {
		if !bytes.Equal(files1[name], files3[name]) {
			t.Errorf("file %s changed across rebuilds in the same dir", name)
		}
	}
}
