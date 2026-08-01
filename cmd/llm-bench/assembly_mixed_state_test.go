package main

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/rag"
)

// mixedStateCase is the canonical Task 4 builder case: mixedJoinCase's three
// turns plus one call per tool, inserted in declared order before the final
// question turn.
func mixedStateCase(id string) mixedCase {
	c := mixedJoinCase(id)
	c = withMixedToolCall(c, mixedToolCall{
		CallID: "call-r1", Tool: "retrieve",
		Args: json.RawMessage(`{"query":"retry ceiling","k":2}`),
	})
	c = withMixedToolCall(c, mixedToolCall{
		CallID: "call-m1", Tool: "agent_memory_search",
		Args: json.RawMessage(`{"query":"gateway port"}`),
	})
	c = withMixedToolCall(c, mixedToolCall{
		CallID: "call-e1", Tool: "fixture_echo",
		Args: json.RawMessage(`{"content":"echoed note"}`),
	})
	return c
}

func buildMixedStateT(t *testing.T, c mixedCase) mixedBuiltState {
	t.Helper()
	built, err := buildMixedState(context.Background(), c)
	if err != nil {
		t.Fatalf("buildMixedState: %v", err)
	}
	return built
}

func TestBuildMixedStateShape(t *testing.T) {
	c := mixedStateCase("shape-1")
	if err := validateMixedCase(c); err != nil {
		t.Fatalf("fixture case invalid: %v", err)
	}
	built := buildMixedStateT(t, c)
	st := built.State

	if st.System != c.System {
		t.Errorf("System = %q; want %q", st.System, c.System)
	}
	if st.DurableSummary != "" {
		t.Errorf("DurableSummary = %q; want empty", st.DurableSummary)
	}
	wantRoles := []string{"user", "assistant", "assistant", "tool", "assistant", "tool", "assistant", "tool", "user"}
	if len(st.Messages) != len(wantRoles) {
		t.Fatalf("message count = %d; want %d", len(st.Messages), len(wantRoles))
	}
	for i, want := range wantRoles {
		if st.Messages[i].Role != want {
			t.Errorf("message %d role = %q; want %q", i, st.Messages[i].Role, want)
		}
	}
	if st.Messages[0].Content != c.Events[0].Turn.Content || st.Messages[1].Content != c.Events[1].Turn.Content {
		t.Errorf("turn contents not preserved in declared order")
	}
	final := st.Messages[len(st.Messages)-1]
	if final.Content != "What port and retry limit does the gateway use?" {
		t.Errorf("final message content = %q; want the question turn", final.Content)
	}
	if final.Segment != agent.Pinned {
		t.Errorf("final message segment = %v; want Pinned (production initState pins the goal)", final.Segment)
	}
	for i, m := range st.Messages[:len(st.Messages)-1] {
		if m.Segment != agent.Elastic {
			t.Errorf("message %d segment = %v; want Elastic", i, m.Segment)
		}
	}

	pairs := []struct {
		asst, tool int
		callID     string
		toolName   string
		wantCtx    bool
	}{
		{2, 3, "call-r1", "retrieve", true},
		{4, 5, "call-m1", "agent_memory_search", true},
		{6, 7, "call-e1", "fixture_echo", false},
	}
	for _, p := range pairs {
		asst := st.Messages[p.asst]
		if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != p.callID ||
			asst.ToolCalls[0].Function.Name != p.toolName {
			t.Errorf("assistant message %d tool calls = %+v; want one %s/%s call", p.asst, asst.ToolCalls, p.callID, p.toolName)
		}
		tool := st.Messages[p.tool]
		if tool.ToolCallID != p.callID || tool.ToolName != p.toolName {
			t.Errorf("tool message %d ids = %q/%q; want %q/%q", p.tool, tool.ToolCallID, tool.ToolName, p.callID, p.toolName)
		}
		if (tool.Context != nil) != p.wantCtx {
			t.Errorf("tool message %d Context non-nil = %v; want %v", p.tool, tool.Context != nil, p.wantCtx)
		}
		if tool.OutputCap != 64<<10 {
			t.Errorf("tool message %d OutputCap = %d; want default %d", p.tool, tool.OutputCap, 64<<10)
		}
		if tool.Content == "" {
			t.Errorf("tool message %d content is empty", p.tool)
		}
	}
	if got := st.Messages[7].Content; got != "echoed note" {
		t.Errorf("fixture_echo content = %q; want %q", got, "echoed note")
	}

	wantSubjects := []string{"(call-r1|rag|pkg/gw/gw.go)", "(call-m1|memory|rec-1)"}
	if !reflect.DeepEqual(built.Subjects, wantSubjects) {
		t.Errorf("Subjects = %v; want %v", built.Subjects, wantSubjects)
	}
	wantCandidates := []string{assemblyChunkID("pkg/gw/gw.go", c.RagSources[0].Content), "rec-1"}
	sort.Strings(wantCandidates)
	if !reflect.DeepEqual(built.CandidateIDs, wantCandidates) {
		t.Errorf("CandidateIDs = %v; want %v", built.CandidateIDs, wantCandidates)
	}

	// cap_stress: the fixture output_cap overrides the production default for
	// both the message's OutputCap and its capped flat Content; the structured
	// payload stays attached uncapped (production capOutput caps Content only).
	capBuilt := buildMixedStateT(t, mixedCapStressCase("shape-cap"))
	var capTool *agent.Message
	for i := range capBuilt.State.Messages {
		if capBuilt.State.Messages[i].Role == "tool" {
			capTool = &capBuilt.State.Messages[i]
		}
	}
	if capTool == nil {
		t.Fatalf("cap_stress build has no tool message")
	}
	if capTool.OutputCap != 96 {
		t.Errorf("cap_stress OutputCap = %d; want the fixture override 96", capTool.OutputCap)
	}
	if len(capTool.Content) == 0 || len(capTool.Content) > 96 {
		t.Errorf("cap_stress content length = %d; want 1..96", len(capTool.Content))
	}
	if capTool.Context == nil {
		t.Errorf("cap_stress Context = nil; want the structured payload attached uncapped")
	}

	// Determinism: two independent builds are deep-equal with equal digests.
	again := buildMixedStateT(t, c)
	if !reflect.DeepEqual(built, again) {
		t.Errorf("two builds of the same case differ")
	}
	if built.StateDigest == "" || built.StateDigest != again.StateDigest {
		t.Errorf("digests differ across builds: %q vs %q", built.StateDigest, again.StateDigest)
	}
}

func TestBuildMixedStateUsesRealProducers(t *testing.T) {
	ctx := context.Background()
	c := mixedStateCase("real-1")
	built := buildMixedStateT(t, c)
	var retrieveMsg, memMsg *agent.Message
	for i := range built.State.Messages {
		m := &built.State.Messages[i]
		if m.Role != "tool" {
			continue
		}
		switch m.ToolCallID {
		case "call-r1":
			retrieveMsg = m
		case "call-m1":
			memMsg = m
		}
	}
	if retrieveMsg == nil || memMsg == nil {
		t.Fatalf("missing tool messages: retrieve=%v memory=%v", retrieveMsg != nil, memMsg != nil)
	}

	// Oracle 1: rag's own progressive-groups render over the SAME seeding and
	// the same query/k the fixture declares, bridged exactly as agent/tools
	// bridges it. The builder may not hand-roll Context.
	rig, err := seedAssemblyRig(ctx, c.RagSources)
	if err != nil {
		t.Fatalf("seedAssemblyRig: %v", err)
	}
	defer func() { _ = rig.Close() }()
	results, err := rig.retr.Retrieve(ctx, "retry ceiling", 2)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	content, _, groups, err := rig.retr.RenderProgressiveWithGroups(ctx, rag.ProgressiveRenderRequest{
		Results: results, MaxTokens: 2048, MaxBytes: agenttools.RetrieveOutputCap,
	})
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}
	if retrieveMsg.Content != content {
		t.Errorf("retrieve Content diverges from the rag oracle render:\ngot:\n%s\nwant:\n%s", retrieveMsg.Content, content)
	}
	expected := &agent.ContextSet{MinVerbatim: 1}
	for _, g := range groups {
		cg := agent.ContextGroup{Desc: g.Desc}
		for _, a := range g.Alternatives {
			ca := agent.ContextAlternative{Desc: a.Desc, Content: a.Content}
			if len(a.RenderedEvidence) > 0 {
				attrib := &agent.RetrievalAttribution{}
				for _, ev := range a.RenderedEvidence {
					attrib.Sources = append(attrib.Sources, agent.RetrievedSource{
						StableKey: ev.StableKey, Source: ev.Source,
						StartLine: ev.StartLine, EndLine: ev.EndLine, Score: ev.Score,
					})
				}
				ca.Attrib = attrib
			}
			cg.Alternatives = append(cg.Alternatives, ca)
		}
		expected.Groups = append(expected.Groups, cg)
	}
	if !reflect.DeepEqual(retrieveMsg.Context, expected) {
		t.Errorf("retrieve Context diverges from the rag-bridged oracle:\ngot %+v\nwant %+v", retrieveMsg.Context, expected)
	}

	// Oracle 2: the real agent_memory_search tool invoked directly over the
	// same seeded records.
	memStore, memDB, ws, err := seedMixedMemoryStore(ctx, c.MemoryRecords)
	if err != nil {
		t.Fatalf("seedMixedMemoryStore: %v", err)
	}
	defer func() { _ = memDB.Close() }()
	if ws != "" {
		t.Fatalf("derived workspace = %q; want empty for this fixture", ws)
	}
	oracle := agenttools.AgentMemorySearch{
		S: memStore, WorkspaceID: ws,
		SessionID: func() string { return mixedMemorySessionID },
		Now:       func() time.Time { return time.Unix(mixedMemoryEpoch, 0).UTC() },
	}
	out, err := oracle.Invoke(ctx, json.RawMessage(`{"query":"gateway port"}`))
	if err != nil || out.IsError {
		t.Fatalf("oracle Invoke: err=%v isError=%v content=%q", err, out.IsError, out.Content)
	}
	if memMsg.Content != out.Content {
		t.Errorf("memory Content diverges from the real tool's flat rendering:\ngot %q\nwant %q", memMsg.Content, out.Content)
	}
	if !reflect.DeepEqual(memMsg.Context, out.Context) {
		t.Errorf("memory Context diverges from the real tool's projection")
	}
}

// TestMixedCapContent pins the capOutput-equivalent truncation, including the
// UTF-8 rune-boundary backup a plain s[:limit] would get wrong.
func TestMixedCapContent(t *testing.T) {
	tests := []struct {
		s     string
		limit int
		want  string
	}{
		{"héllo", 2, "h"},  // limit splits é; back up to the rune boundary
		{"héllo", 3, "hé"}, // limit lands exactly after é
		{"abc", 0, "abc"},  // 0 = uncapped
		{"abc", 3, "abc"},  // at the limit: unchanged
	}
	for _, tt := range tests {
		if got := mixedCapContent(tt.s, tt.limit); got != tt.want {
			t.Errorf("mixedCapContent(%q, %d) = %q; want %q", tt.s, tt.limit, got, tt.want)
		}
	}
}

// TestBuildMixedStateWorkingMemoryVisible proves the working-kind seeding
// path end to end: a session-scoped working record (workspace set) comes back
// through the REAL search tool, i.e. the builder's session injection keeps
// working records visible to the tool's session.
func TestBuildMixedStateWorkingMemoryVisible(t *testing.T) {
	c := withMixedToolCall(mixedConvCase("work-mem"), mixedToolCall{
		CallID: "call-w1", Tool: "agent_memory_search",
		Args: json.RawMessage(`{"query":"standup wednesdays"}`),
	})
	built := buildMixedStateT(t, c)
	var toolMsg *agent.Message
	for i := range built.State.Messages {
		if built.State.Messages[i].Role == "tool" {
			toolMsg = &built.State.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message in built State")
	}
	if !strings.Contains(toolMsg.Content, "standup moved to 09:30 on wednesdays") {
		t.Errorf("working record content not returned by the real search tool; got %q", toolMsg.Content)
	}
	if toolMsg.Context == nil || len(toolMsg.Context.Groups) != 1 ||
		toolMsg.Context.Groups[0].Desc.Subject.ID != "rec-1" {
		t.Errorf("Context = %+v; want one memory group for rec-1", toolMsg.Context)
	}
}

func TestBuildMixedStateProductionCaps(t *testing.T) {
	built := buildMixedStateT(t, mixedStateCase("caps-1"))
	msgs := built.State.Messages
	if got := msgs[3].OutputCap; got != agenttools.RetrieveOutputCap {
		t.Errorf("retrieve OutputCap = %d; want tools.RetrieveOutputCap %d", got, agenttools.RetrieveOutputCap)
	}
	// Wiring pins, by literal (golem construction values; see the consts).
	if got := mixedRetrieveK; got != 5 {
		t.Errorf("mixedRetrieveK = %d; want 5 (golem wiring)", got)
	}
	if got := mixedRetrieveMaxTokens; got != 2048 {
		t.Errorf("mixedRetrieveMaxTokens = %d; want 2048 (golem wiring)", got)
	}
	// Coupling pins, by literal: mixedDefaultOutputCap restates agent's
	// unexported defaultOutputCap (agent/types.go), and RetrieveOutputCap
	// matches it by that package's own contract.
	if got := mixedDefaultOutputCap; got != 64<<10 {
		t.Errorf("mixedDefaultOutputCap = %d; want 64<<10", got)
	}
	if got := agenttools.RetrieveOutputCap; got != 64<<10 {
		t.Errorf("tools.RetrieveOutputCap = %d; want 64<<10", got)
	}
	if msgs[5].OutputCap != mixedDefaultOutputCap {
		t.Errorf("agent_memory_search OutputCap = %d; want the normalized default %d", msgs[5].OutputCap, mixedDefaultOutputCap)
	}
	if msgs[7].OutputCap != mixedDefaultOutputCap {
		t.Errorf("fixture_echo OutputCap = %d; want the normalized default %d", msgs[7].OutputCap, mixedDefaultOutputCap)
	}
}

func TestBuildMixedStatePinnedEpochs(t *testing.T) {
	c := mixedStateCase("epoch-1")
	a := buildMixedStateT(t, c)
	b := buildMixedStateT(t, c)
	line := time.Unix(assemblyFixedEpoch, 0).UTC().Format(time.RFC3339)
	retrA, retrB := a.State.Messages[3].Content, b.State.Messages[3].Content
	if !strings.Contains(retrA, line) {
		t.Errorf("retrieve content missing the pinned-epoch RFC3339 line %q:\n%s", line, retrA)
	}
	if retrA != retrB {
		t.Errorf("retrieve content differs across builds")
	}
	if a.State.Messages[5].Content != b.State.Messages[5].Content {
		t.Errorf("memory content differs across builds")
	}
	// The memory tool renders CreatedAt as a LOCAL-time date (recordLine,
	// agent/tools/agent_memory.go), so the rendered date is only build-machine
	// stable because mixedMemoryEpoch sits at noon UTC. Assert the noon date
	// literal: under the midnight assemblyFixedEpoch this line reads
	// "2025-07-26" on any US-timezone machine.
	if mem := a.State.Messages[5].Content; !strings.Contains(mem, " · 2025-07-27 · ") {
		t.Errorf("memory content missing the noon-UTC pinned date 2025-07-27 (created_at must be pinned to mixedMemoryEpoch, not midnight UTC):\n%s", mem)
	}
}

// TestMixedMemoryEpochDateStable pins the property that makes committed mixed
// traces build-machine independent WITHOUT mutating the process timezone
// (time.Local is process-wide and set once, so per-test TZ switching is not
// reliable): for every fixed offset from UTC-11 through UTC+11, the local
// calendar date at mixedMemoryEpoch — computed arithmetically as the UTC date
// of (epoch + offset) — must equal the UTC calendar date. A midnight-UTC
// epoch fails this at any negative offset (UTC-11 lands on 2025-07-26).
func TestMixedMemoryEpochDateStable(t *testing.T) {
	// time.UnixMilli mirrors how the record store loads created_at
	// (memory/record_store.go) before recordLine formats it.
	want := time.UnixMilli(mixedMemoryEpoch * 1000).UTC().Format("2006-01-02")
	if want != "2025-07-27" {
		t.Fatalf("mixedMemoryEpoch UTC date = %s; want 2025-07-27 (assemblyFixedEpoch's date at noon)", want)
	}
	for off := -11; off <= 11; off++ {
		got := time.UnixMilli((mixedMemoryEpoch + int64(off)*3600) * 1000).UTC().Format("2006-01-02")
		if got != want {
			t.Errorf("UTC%+dh renders date %s; want %s (mixedMemoryEpoch is not date-stable across the supported UTC-11..+11 build-zone band; UTC+12..+14 sit outside the band and are unsupported by design)", off, got, want)
		}
	}
}

func TestBuildMixedStateDigest(t *testing.T) {
	c := mixedStateCase("digest-1")
	base := buildMixedStateT(t, c)
	if base.StateDigest == "" || base.StateDigest != mixedStateDigest(base.State) {
		t.Fatalf("StateDigest = %q; want the digest of the built State", base.StateDigest)
	}
	if again := buildMixedStateT(t, c); again.StateDigest != base.StateDigest {
		t.Fatalf("digest unstable across builds: %q vs %q", again.StateDigest, base.StateDigest)
	}
	mutations := []struct {
		name   string
		mutate func(*agent.State)
	}{
		{"message content", func(st *agent.State) { st.Messages[0].Content += "x" }},
		{"tool call id", func(st *agent.State) { st.Messages[3].ToolCallID = "call-zz" }},
		{"output cap", func(st *agent.State) { st.Messages[3].OutputCap++ }},
		{"alternative content", func(st *agent.State) {
			alts := st.Messages[3].Context.Groups[0].Alternatives
			alts[len(alts)-1].Content += "x"
		}},
	}
	for _, m := range mutations {
		fresh := buildMixedStateT(t, c)
		m.mutate(&fresh.State)
		if got := mixedStateDigest(fresh.State); got == base.StateDigest {
			t.Errorf("%s mutation did not change the digest", m.name)
		}
	}
}

func TestBuildMixedStateRawTokens(t *testing.T) {
	c := mixedCase{
		ID:     "tok-1",
		System: "sys prompt!", // 11 bytes -> 3 tokens
		Events: []mixedEvent{
			{Turn: &mixedTurn{Role: "user", Content: "abcd"}}, // 1 token + envelope
			{ToolCall: &mixedToolCall{CallID: "c1", Tool: "fixture_echo",
				Args: json.RawMessage(`{"content":"12345678"}`)}}, // assistant 0 + envelope, tool 2 + envelope
			{Turn: &mixedTurn{Role: "user", Content: "q"}}, // 1 token + envelope
		},
	}
	built := buildMixedStateT(t, c)
	// Hand-computed, NOT via the helper: est(s) = (len+3)/4, envelope 8 per
	// message: 3 + (1+8) + (0+8) + (2+8) + (1+8) = 39.
	if built.RawStateTokens != 39 {
		t.Errorf("RawStateTokens = %d; want 39", built.RawStateTokens)
	}
}

func TestBuildMixedStateErrors(t *testing.T) {
	tests := []struct {
		name string
		tc   mixedToolCall
		want []string
	}{
		{
			name: "retrieve rejects a missing query",
			tc:   mixedToolCall{CallID: "bad-r", Tool: "retrieve", Args: json.RawMessage(`{"k":1}`)},
			want: []string{"err-case", "bad-r", "query is required"},
		},
		{
			name: "memory search rejects a non-string query",
			tc:   mixedToolCall{CallID: "bad-m", Tool: "agent_memory_search", Args: json.RawMessage(`{"query":5}`)},
			want: []string{"err-case", "bad-m", "invalid arguments"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := withMixedToolCall(mixedJoinCase("err-case"), tt.tc)
			_, err := buildMixedState(context.Background(), c)
			if err == nil {
				t.Fatalf("err = nil; want a loud tool error naming the case and call_id")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v; missing %q", err, want)
				}
			}
		})
	}
}
