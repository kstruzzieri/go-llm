package agent

import (
	"context"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// lowWaterMgr prices messages exactly as an Orchestrator's manager does in
// these tests: runeEstimator plus one #430 frame envelope per tool message.
var lowWaterMgr = ContextManager{Estimate: runeEstimator, frameToolResults: true}

// fixedRunCost is what every request of a Run without a caller System pays
// before any transcript message: the effective system prompt (trust
// contract plus interceptor addendum) and the tool schemas.
func fixedRunCost(t *testing.T, addendum string, tools ...Tool) int {
	t.Helper()
	reg, err := newToolRegistry(tools)
	if err != nil {
		t.Fatalf("newToolRegistry: %v", err)
	}
	return runeEstimator(withToolTrustContract("")+addendum) + runeEstimator(toolSchemaString(reg.providerSpecs()))
}

// chainCost is one tool chain as the Orchestrator's manager prices it: the
// assistant call plus its framed observation carrying content. Under the
// additive runeEstimator it is also what mixed assembly charges for an anchor
// rendered as content (envelope plus content).
func chainCost(c provider.ToolCall, content string) int {
	asst := assistantMessage(provider.ChatResponse{ToolCalls: []provider.ToolCall{c}})
	return lowWaterMgr.messageCost(asst) + lowWaterMgr.messageCost(toolObservation(c, ToolResult{Content: content}))
}

// lowWaterOrchestrator is newTestOrchestrator with CompactionLowWater set. Its
// Compactor is a RecencyCompactor value, which counts as built-in assembly.
func lowWaterOrchestrator(mc ModelCaller, lowWater float64, opts ...Option) *Orchestrator {
	return New(mc, ContextManager{
		Compactor:          RecencyCompactor{Estimate: runeEstimator},
		Estimate:           runeEstimator,
		CompactionLowWater: lowWater,
	}, opts...)
}

// shape is each message as role plus its first eight content bytes, for
// readable failure output over long fixture contents.
func shape(st State) []string {
	out := make([]string, 0, len(st.Messages))
	for _, m := range st.Messages {
		out = append(out, m.Role+":"+m.Content[:min(8, len(m.Content))])
	}
	return out
}

func eventCount(events []EventRecord, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// nthCall is the i-th scripted call: ID ci, distinct arguments so the repeat
// guard never stops the Run.
func nthCall(name string, i int) provider.ToolCall {
	return call(fmt.Sprintf("c%d", i), name, fmt.Sprintf(`{"n":%d}`, i))
}

// toolScript is n tool calls c1..cn to name (nthCall), then a final answer.
func toolScript(name string, n int) []ModelResult {
	var script []ModelResult
	for i := 1; i <= n; i++ {
		script = append(script, toolCallResponse(nthCall(name, i)))
	}
	return append(script, finalAnswer("done"))
}

// queuedSets returns a lookup tool that answers its k-th call with sets[k].
func queuedSets(sets ...*ContextSet) Tool {
	next := 0
	return dynamicGroupedTool{set: func() *ContextSet {
		s := sets[next]
		next++
		return s
	}}
}

func oneAltSet(id, content string) *ContextSet {
	return &ContextSet{Groups: []ContextGroup{oneAltGroup(id, 1, content, altAbstract)}}
}

// normalizedRequest replaces every tool frame key with a fixed placeholder,
// so requests from two Runs (whose keys are random) compare byte for byte.
func normalizedRequest(t *testing.T, req provider.ChatRequest) provider.ChatRequest {
	t.Helper()
	req.Messages = slices.Clone(req.Messages)
	for i, m := range req.Messages {
		if m.Role == "tool" {
			req.Messages[i].Content = strings.ReplaceAll(m.Content, extractToolFrameKey(t, m.Content), "KEYKEYKEYKEY")
		}
	}
	return req
}

func TestRecencyCompactorLowWaterDropsToFraction(t *testing.T) {
	// A 10-token pinned goal and twelve 10-token elastic groups: 130 tokens.
	msgs := []Message{pinned("user", strings.Repeat("G", 10))}
	for i := range 12 {
		msgs = append(msgs, elastic("assistant", fmt.Sprintf("%02d%s", i, strings.Repeat("a", 8))))
	}
	st := State{Messages: msgs}
	rc := RecencyCompactor{Estimate: runeEstimator}
	if total := rc.total(st); total != 130 {
		t.Fatalf("fixture total = %d, want 130", total)
	}

	out, rep, err := rc.Compact(context.Background(), st, TokenBudget{Input: 100, LowWaterFraction: 0.75})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if rep.TokensAfter > 75 {
		t.Fatalf("TokensAfter = %d, want <= 75 (the low-water mark)", rep.TokensAfter)
	}
	// Exactly to the mark, oldest first: six drops reach 70, five leave 80.
	if rep.DroppedCount != 6 || rep.TokensAfter != 70 {
		t.Errorf("report = %+v, want 6 dropped and 70 after", rep)
	}
	if got, want := msgContents(out), msgContents(State{Messages: append([]Message{msgs[0]}, msgs[7:]...)}); !slices.Equal(got, want) {
		t.Errorf("kept %q, want the goal and the six newest %q", got, want)
	}
}

// firstToolIDChanges counts requests whose first retained tool message is a
// different call than the previous request's. Each is a cut-point move, and
// with it a prompt-cache miss for everything after the system prompt.
func firstToolIDChanges(reqs []provider.ChatRequest) int {
	changes, prev := 0, ""
	for _, req := range reqs {
		tools := toolMessagesOf(req)
		if len(tools) == 0 {
			continue
		}
		if prev != "" && tools[0].ToolCallID != prev {
			changes++
		}
		prev = tools[0].ToolCallID
	}
	return changes
}

func cutPointRun(t *testing.T, lowWater float64) (changes, compactions int) {
	t.Helper()
	const goal = "q"
	script := toolScript("bulky", 12)
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
	// Five of the widest chains fit beside the contract and goal, so several
	// chains are retained before the sixth forces the first compaction.
	ceiling := fixedRunCost(t, "", bulkyTool{}) + runeEstimator(goal) +
		5*chainCost(nthCall("bulky", 12), strings.Repeat("X", 400))
	res, err := lowWaterOrchestrator(mc, lowWater).Run(context.Background(), Request{
		Goal: goal, MaxSteps: len(script), Tools: []Tool{bulkyTool{}},
		Budget: Budget{InputCeiling: ceiling},
	}, nil)
	if err != nil {
		t.Fatalf("Run(low-water %v): %v", lowWater, err)
	}
	if res.StopReason != Completed || len(mc.reqs) != len(script) {
		t.Fatalf("Run(low-water %v) stopped %v after %d requests, want completed after %d", lowWater, res.StopReason, len(mc.reqs), len(script))
	}
	return firstToolIDChanges(mc.reqs), eventCount(res.Events, "compaction")
}

func TestRunCompactionCutPointStable(t *testing.T) {
	sticky, stickyCompactions := cutPointRun(t, 0.75)
	base, baseCompactions := cutPointRun(t, 0)
	t.Logf("first retained tool call changed in %d requests at low-water 0.75 (%d compactions) and %d at 0 (%d compactions), of 13",
		sticky, stickyCompactions, base, baseCompactions)
	if stickyCompactions == 0 || baseCompactions == 0 {
		t.Fatalf("compactions = %d (0.75), %d (0); the fixture must compact", stickyCompactions, baseCompactions)
	}
	if sticky > 3 {
		t.Errorf("cut point moved %d times at low-water 0.75, want <= 3", sticky)
	}
	if base <= sticky {
		t.Errorf("cut point moved %d times at 0 and %d at 0.75; the low-water mark must move it less", base, sticky)
	}
}

func TestLowWaterNeverHidesPinnedOrUnresolved(t *testing.T) {
	t.Run("run with a pinned goal above the mark", func(t *testing.T) {
		bulky := strings.Repeat("X", 400)
		chain := chainCost(nthCall("bulky", 1), bulky)
		room := chain + chain/2 // one chain fits, two do not
		// The goal alone outweighs three rooms, so the system prompt plus goal
		// sit above 75% of the state budget: the mark is unreachable.
		goal := strings.Repeat("g", 4*room)
		script := toolScript("bulky", 4)
		mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
		res, err := lowWaterOrchestrator(mc, 0.75).Run(context.Background(), Request{
			Goal: goal, MaxSteps: len(script), Tools: []Tool{bulkyTool{}},
			Budget: Budget{InputCeiling: fixedRunCost(t, "", bulkyTool{}) + runeEstimator(goal) + room},
		}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.StopReason != Completed || len(mc.reqs) != len(script) {
			t.Fatalf("stopped %v after %d requests, want completed after %d", res.StopReason, len(mc.reqs), len(script))
		}
		if eventCount(res.Events, "compaction") == 0 {
			t.Fatal("never compacted; the fixture must exercise the unreachable mark")
		}
		for k, req := range mc.reqs {
			goals := 0
			for _, m := range req.Messages {
				if m.Role == "user" && m.Content == goal {
					goals++
				}
			}
			if goals != 1 {
				t.Errorf("request %d carries the goal %d times, want 1", k, goals)
			}
			// The newest observation is the tail the next answer depends on;
			// the just-enough fit keeps it and only it.
			if k == 0 {
				continue
			}
			tools := toolMessagesOf(req)
			if len(tools) != 1 || tools[0].ToolCallID != fmt.Sprintf("c%d", k) {
				t.Errorf("request %d tool messages = %d (first %v), want only c%d", k, len(tools), tools, k)
			}
		}
	})

	t.Run("assembly with an unresolved tail above the mark", func(t *testing.T) {
		// Goal 400 + unresolved tail 450 = 850 must-fit; three 50-token
		// completed chains make 1000. Input 960 drops one chain to fit, and
		// the 720 mark is unreachable, so low-water must return that same fit.
		msgs := []Message{gateAnchor(pinned("user", strings.Repeat("G", 400)))} // gate: mixed on, legacy inert
		for i := 1; i <= 3; i++ {
			id := fmt.Sprintf("c%d", i)
			msgs = append(msgs, mixedAsstCall(id), mixedToolResult(id, strings.Repeat("r", 20), nil, 0))
		}
		msgs = append(msgs, mixedAsstCall("t1", "t2"), mixedToolResult("t1", strings.Repeat("T", 400), nil, 0))
		st := State{Messages: msgs}
		for _, mgr := range []struct {
			name string
			m    ContextManager
		}{{"legacy", ContextManager{Estimate: runeEstimator}}, {"mixed", ContextManager{Mixed: true, Estimate: runeEstimator}}} {
			t.Run(mgr.name, func(t *testing.T) {
				in := visibleState(st, nil)
				base, _, _, err := mgr.m.AssembleWithTrace(context.Background(), in, 0, TokenBudget{Input: 960})
				if err != nil {
					t.Fatalf("fraction 0: %v", err)
				}
				lw := mgr.m
				lw.CompactionLowWater = 0.75
				low, pressure, _, err := lw.AssembleWithTrace(context.Background(), in, 0, TokenBudget{Input: 960})
				if err != nil {
					t.Fatalf("fraction 0.75: %v", err)
				}
				if pressure.Compactions == 0 {
					t.Fatal("nothing dropped; the fixture must compact")
				}
				if got, want := msgContents(low), msgContents(base); !slices.Equal(got, want) {
					t.Errorf("low-water kept %q, want the fraction-0 fit %q", shape(low), shape(base))
				}
				hidden := hideEvicted(nil, in, low, len(st.Messages))
				for i, m := range st.Messages {
					tail := i >= len(st.Messages)-2
					if (m.Segment == Pinned || tail) && i < len(hidden) && hidden[i] {
						t.Errorf("message %d (%s %q) hidden; pinned messages and unresolved tails never are", i, m.Role, m.Content[:min(8, len(m.Content))])
					}
				}
				if len(hidden) < 3 || !hidden[1] || !hidden[2] {
					t.Errorf("hidden = %v, want the dropped c1 chain (messages 1-2) hidden", hidden)
				}
			})
		}
	})

	t.Run("a pinned message missing from the assembly stays visible", func(t *testing.T) {
		in := visibleState(State{Messages: []Message{pinned("user", "G"), elastic("assistant", "A")}}, nil)
		hidden := hideEvicted(nil, in, State{}, 2)
		if !slices.Equal(hidden, []bool{false, true}) {
			t.Errorf("hidden = %v, want [false true]: only Elastic messages are hidden", hidden)
		}
	})
}

func TestMixedLowWaterFallsBackToFullBudget(t *testing.T) {
	content := strings.Repeat("y", 300)
	chain := chainCost(nthCall("lookup", 1), content)
	base := chainCost(nthCall("lookup", 1), omittedObservation)
	contract := runeEstimator(ToolTrustContract)
	schema := fixedRunCost(t, "", queuedSets()) - contract
	// One full chain fits beside the must-fit units and an older chain's
	// base does not, so every step past the second evicts. The system
	// prompt plus goal are 85% of the state budget, above the 75% mark.
	free := chain + base/2
	stateBudget := free * 100 / 15
	goal := strings.Repeat("g", stateBudget-free-contract)

	run := func(lowWater float64) ([]provider.ChatRequest, Result) {
		sets := []*ContextSet{oneAltSet("a.go", content), oneAltSet("b.go", content), oneAltSet("c.go", content)}
		script := toolScript("lookup", len(sets))
		mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
		res, err := New(mc, ContextManager{Mixed: true, Estimate: runeEstimator, CompactionLowWater: lowWater}).Run(context.Background(), Request{
			Goal: goal, MaxSteps: len(script), Tools: []Tool{queuedSets(sets...)},
			Budget: Budget{InputCeiling: stateBudget + schema},
		}, nil)
		if err != nil {
			t.Fatalf("Run(low-water %v): %v", lowWater, err)
		}
		if res.StopReason != Completed || len(mc.reqs) != len(script) {
			t.Fatalf("Run(low-water %v) stopped %v after %d requests", lowWater, res.StopReason, len(mc.reqs))
		}
		return mc.reqs, res
	}
	baseReqs, baseRes := run(0)
	lowReqs, _ := run(0.75)
	if eventCount(baseRes.Events, "compaction") == 0 {
		t.Fatal("the fraction-0 Run never compacted; the fixture must evict")
	}
	if len(lowReqs) != len(baseReqs) {
		t.Fatalf("%d requests at 0.75, %d at 0", len(lowReqs), len(baseReqs))
	}
	for k := range baseReqs {
		if got, want := normalizedRequest(t, lowReqs[k]), normalizedRequest(t, baseReqs[k]); !reflect.DeepEqual(got, want) {
			t.Errorf("request %d differs from the fraction-0 request:\n got %+v\nwant %+v", k, got.Messages, want.Messages)
		}
	}
}

// TestLowWaterKeepsNewestObservation covers the window pinned < mark <
// pinned + newest group: dropping to the mark would take the observation the
// model asked for on the previous step and has not read, and sticky hiding
// would keep it out for good. Every request after a tool step must carry
// that step's calls and results.
func TestLowWaterKeepsNewestObservation(t *testing.T) {
	t.Run("prior history is never the kept group", func(t *testing.T) {
		// Step 0 of a session with history and no tool chain yet. The
		// materialized durable summary is pinned at index 0, but it
		// does not move the history boundary, so the history exchanges
		// stay prior history. They must not count as the newest
		// current-run group, or the mark becomes unreachable and the
		// step falls back to the just-enough fit.
		for _, tc := range []struct{ name, summary string }{
			{"without a durable summary", ""},
			{"with a durable summary", "earlier work"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				const h, g = 100, 10 // each history exchange is 2h, the goal g
				st := State{DurableSummary: tc.summary, Messages: []Message{
					history("user", strings.Repeat("a", h)), history("assistant", strings.Repeat("A", h)),
					history("user", strings.Repeat("b", h)), history("assistant", strings.Repeat("B", h)),
					pinned("user", strings.Repeat("G", g)),
				}}
				s, kept := 0, 1 // summary cost; kept messages: the goal
				if tc.summary != "" {
					s, kept = runeEstimator(DurableSummaryPrompt(tc.summary)), 2 // and the summary
				}
				// One dropped exchange fits Input exactly; the mark needs both.
				input := s + 2*h + g
				m := ContextManager{Estimate: runeEstimator, CompactionLowWater: 0.75}
				out, pressure, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: input})
				if err != nil {
					t.Fatalf("Assemble: %v", err)
				}
				if pressure.Evicted != 2 || out.Messages[len(out.Messages)-1].Content != strings.Repeat("G", g) || len(out.Messages) != kept {
					t.Errorf("evicted %d, kept %q; want both history exchanges dropped to reach the mark", pressure.Evicted, shape(out))
				}
			})
		}
	})

	bulky := strings.Repeat("X", 400)
	t.Run("groups before the goal are never the kept group", func(t *testing.T) {
		// Two completed chains ahead of the goal (60 each, goal 10, Input
		// 90): the just-enough fit drops one, and the 67 mark needs both.
		// Keeping the newer one would make the mark unreachable.
		st := State{Messages: []Message{
			mixedAsstCall("c0"), mixedToolResult("c0", strings.Repeat("0", 30), nil, 0),
			mixedAsstCall("c1"), mixedToolResult("c1", strings.Repeat("1", 30), nil, 0),
			gateAnchor(pinned("user", strings.Repeat("G", 10))), // gate: mixed on, legacy inert
		}}
		for _, m := range []ContextManager{
			{Estimate: runeEstimator, CompactionLowWater: 0.75},
			{Mixed: true, Estimate: runeEstimator, CompactionLowWater: 0.75},
		} {
			out, pressure, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: 90})
			if err != nil {
				t.Fatalf("Assemble (mixed %t): %v", m.Mixed, err)
			}
			if pressure.Evicted != 2 || len(out.Messages) != 1 {
				t.Errorf("mixed %t: evicted %d, kept %q; want both chains before the goal dropped", m.Mixed, pressure.Evicted, shape(out))
			}
		}
	})
	t.Run("legacy", func(t *testing.T) {
		chain := chainCost(nthCall("bulky", 1), bulky)
		script := toolScript("bulky", 4)
		mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
		res, err := lowWaterOrchestrator(mc, 0.75).Run(context.Background(), Request{
			Goal: "q", MaxSteps: len(script), Tools: []Tool{bulkyTool{}},
			Budget: Budget{InputCeiling: fixedRunCost(t, "", bulkyTool{}) + 1 + chain + chain/2},
		}, nil)
		assertNewestObservations(t, mc.reqs, res, err, len(script), func(k int) map[string]string {
			return map[string]string{fmt.Sprintf("c%d", k): bulky}
		})
	})
	t.Run("mixed", func(t *testing.T) {
		// Each step calls a structured lookup (keeping mixed assembly on) and
		// bulky together; the pair is one group whose base is mostly bulky.
		alt := strings.Repeat("s", 10)
		var sets []*ContextSet
		var script []ModelResult
		for i := 1; i <= 4; i++ {
			sets = append(sets, oneAltSet(fmt.Sprintf("f%d.go", i), alt))
			args := fmt.Sprintf(`{"n":%d}`, i)
			script = append(script, toolCallResponse(call(fmt.Sprintf("c%d", i), "lookup", args), call(fmt.Sprintf("d%d", i), "bulky", args)))
		}
		script = append(script, finalAnswer("done"))
		tools := []Tool{queuedSets(sets...), bulkyTool{}}
		pair := []provider.ToolCall{call("c1", "lookup", `{"n":1}`), call("d1", "bulky", `{"n":1}`)}
		group := lowWaterMgr.messageCost(assistantMessage(provider.ChatResponse{ToolCalls: pair})) +
			lowWaterMgr.messageCost(toolObservation(pair[0], ToolResult{Content: alt})) +
			lowWaterMgr.messageCost(toolObservation(pair[1], ToolResult{Content: bulky}))
		mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
		res, err := New(mc, ContextManager{Mixed: true, Estimate: runeEstimator, CompactionLowWater: 0.75}).Run(context.Background(), Request{
			Goal: "q", MaxSteps: len(script), Tools: tools,
			Budget: Budget{InputCeiling: fixedRunCost(t, "", tools...) + 1 + group + group/2},
		}, nil)
		assertNewestObservations(t, mc.reqs, res, err, len(script), func(k int) map[string]string {
			return map[string]string{fmt.Sprintf("c%d", k): alt, fmt.Sprintf("d%d", k): bulky}
		})
	})
}

// assertNewestObservations checks a completed, compacting Run in which
// request k (k >= 1) carries want(k): each tool call ID with its unframed
// result.
func assertNewestObservations(t *testing.T, reqs []provider.ChatRequest, res Result, err error, n int, want func(k int) map[string]string) {
	t.Helper()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed || len(reqs) != n {
		t.Fatalf("stopped %v after %d requests, want completed after %d", res.StopReason, len(reqs), n)
	}
	if eventCount(res.Events, "compaction") == 0 {
		t.Fatal("never compacted; the fixture must compact")
	}
	for k := 1; k < len(reqs); k++ {
		got := map[string]string{}
		for _, m := range toolMessagesOf(reqs[k]) {
			got[m.ToolCallID] = frameInner(t, m.Content, extractToolFrameKey(t, m.Content))
		}
		for id, content := range want(k) {
			if got[id] != content {
				t.Errorf("request %d: %s result missing or wrong (tool messages %v); the newest observation must be kept", k, id, slices.Sorted(maps.Keys(got)))
			}
		}
	}
}

// TestMixedLowWaterAllocatesFreshAtTheMark pins that the low-water pass is a
// fresh allocation at the mark. allocateMixed mutates its units (omitted
// spans, evicted chains, chosen alternatives), so a pass over the full-budget
// pass's units would keep that pass's omissions: here the history span the
// full pass omitted, which the mark admits once the older chain c0 is
// evicted. The newest chain c1 is kept at the mark (it holds the observation
// the model has not read yet).
func TestMixedLowWaterAllocatesFreshAtTheMark(t *testing.T) {
	st := State{Messages: []Message{
		history("user", strings.Repeat("q", 20)), history("assistant", strings.Repeat("a", 20)), // span: 40
		gateAnchor(pinned("user", strings.Repeat("G", 100))), // must-fit: 100
		mixedAsstCall("c0"), mixedToolResult("c0", strings.Repeat("o", 70), nil, 0), // chain: 100
		mixedAsstCall("c1"), mixedToolResult("c1", strings.Repeat("n", 30), nil, 0), // chain: 60
	}}
	// Goal and both chains fit, the span does not. At the 210 mark c1 and the
	// span fit beside the goal and c0 does not.
	const full = 280
	mark, _ := lowWaterMark(full, 0.75)
	mixed := ContextManager{Mixed: true, Estimate: runeEstimator}
	atFull, _, err := mixed.Assemble(context.Background(), st, 0, TokenBudget{Input: full})
	if err != nil {
		t.Fatalf("fraction 0: %v", err)
	}
	atMark, _, err := mixed.Assemble(context.Background(), st, 0, TokenBudget{Input: mark})
	if err != nil {
		t.Fatalf("fraction 0 at the mark: %v", err)
	}
	lw := mixed
	lw.CompactionLowWater = 0.75
	low, _, err := lw.Assemble(context.Background(), st, 0, TokenBudget{Input: full})
	if err != nil {
		t.Fatalf("fraction 0.75: %v", err)
	}
	if got, want := msgContents(low), msgContents(atMark); !slices.Equal(got, want) || slices.Equal(got, msgContents(atFull)) {
		t.Errorf("low-water kept %q, want a fresh allocation at the mark %q (the full budget keeps %q)", shape(low), shape(atMark), shape(atFull))
	}
	if !slices.Contains(msgContents(low), strings.Repeat("n", 30)) {
		t.Errorf("low-water kept %q, want the newest chain c1", shape(low))
	}
}

func TestMixedEvictionStickyWithLowWater(t *testing.T) {
	small := strings.Repeat("s", 30)
	env := chainCost(nthCall("lookup", 1), "")
	base := env + runeEstimator(omittedObservation)
	// free fits a small chain plus two bases. B fills it alone, so the older
	// A is evicted beside it; once a small C arrives, B's content no longer
	// fits, B shrinks to its base, and A's base fits again. Only the sticky
	// hidden set keeps A out.
	free := env + runeEstimator(small) + 2*base + 10
	big := strings.Repeat("B", free-env)
	contract := runeEstimator(ToolTrustContract)
	const goal = "q"
	sets := []*ContextSet{oneAltSet("a.go", small), oneAltSet("b.go", big), oneAltSet("c.go", small)}
	script := toolScript("lookup", len(sets))
	tool := queuedSets(sets...)
	schema := fixedRunCost(t, "", tool) - contract
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
	res, err := New(mc, ContextManager{Mixed: true, Estimate: runeEstimator, CompactionLowWater: 0.75}).Run(context.Background(), Request{
		Goal: goal, MaxSteps: len(script), Tools: []Tool{tool},
		Budget: Budget{InputCeiling: contract + schema + runeEstimator(goal) + free},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed || len(mc.reqs) != len(script) {
		t.Fatalf("stopped %v after %d requests, want completed after %d", res.StopReason, len(mc.reqs), len(script))
	}
	seen, evicted := map[string]bool{}, map[string]bool{}
	for k, req := range mc.reqs {
		present := map[string]bool{}
		for _, m := range toolMessagesOf(req) {
			present[m.ToolCallID] = true
			if evicted[m.ToolCallID] {
				t.Errorf("request %d: %s reappeared after it was evicted", k, m.ToolCallID)
			}
		}
		for id := range seen {
			if !present[id] {
				evicted[id] = true
			}
		}
		for id := range present {
			seen[id] = true
		}
	}
	if !evicted["c1"] {
		t.Fatalf("evicted = %v; the fixture must evict c1", evicted)
	}
}

func TestMixedAlternativeChurnCharacterized(t *testing.T) {
	const goal = "q"
	var sets []*ContextSet
	for i := range 6 {
		sets = append(sets, &ContextSet{Groups: []ContextGroup{ladderGroup(fmt.Sprintf("f%d.go", i), 1,
			strings.Repeat("a", 20), strings.Repeat("o", 40), strings.Repeat("e", 60), strings.Repeat("v", 60))}})
	}
	script := toolScript("lookup", len(sets))
	tool := queuedSets(sets...)
	top := chainCost(nthCall("lookup", 1), strings.Repeat("x", 180)) // a fully upgraded chain
	// Room for three fully upgraded chains: free budget shrinks as chains
	// accumulate, so earlier anchors are re-rendered at lower alternatives.
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
	res, err := New(mc, ContextManager{Mixed: true, Estimate: runeEstimator, CompactionLowWater: 0.75}).Run(context.Background(), Request{
		Goal: goal, MaxSteps: len(script), Tools: []Tool{tool},
		Budget: Budget{InputCeiling: fixedRunCost(t, "", tool) + runeEstimator(goal) + 3*top},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed || len(mc.reqs) != len(script) {
		t.Fatalf("stopped %v after %d requests, want completed after %d", res.StopReason, len(mc.reqs), len(script))
	}
	churn := 0
	prev := map[string]string{}
	for _, req := range mc.reqs {
		changed := false
		cur := map[string]string{}
		for _, m := range toolMessagesOf(req) {
			cur[m.ToolCallID] = m.Content
			if old, ok := prev[m.ToolCallID]; ok && old != m.Content {
				changed = true
			}
		}
		if changed {
			churn++
		}
		prev = cur
	}
	t.Logf("an earlier anchor's rendered bytes changed in %d of %d requests", churn, len(mc.reqs))
	// This pins a known limit (plan Deviation 3): low-water and sticky
	// eviction do not stop allocateMixed from re-choosing alternatives as
	// free budget moves. If this reaches 0, update Deviation 3.
	if churn == 0 {
		t.Errorf("no alternative churn; the known limit is gone, so update Deviation 3")
	}
}

func TestRunSystemMessageIdenticalAcrossSteps(t *testing.T) {
	const goal = "q"
	stub := &scopedStub{name: "sc"}
	addendum := " [canary:x]" // scopedStub's first ForRun addendum
	script := toolScript("bulky", 5)
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: script}}
	chain := chainCost(nthCall("bulky", 1), strings.Repeat("X", 400))
	res, err := lowWaterOrchestrator(mc, 0.75, WithInterceptors(stub)).Run(context.Background(), Request{
		Goal: goal, MaxSteps: len(script), Tools: []Tool{bulkyTool{}},
		Budget: Budget{InputCeiling: fixedRunCost(t, addendum, bulkyTool{}) + runeEstimator(goal) + 2*chain},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(mc.reqs) != len(script) || eventCount(res.Events, "compaction") == 0 {
		t.Fatalf("%d requests, %d compactions; want %d requests with compaction active", len(mc.reqs), eventCount(res.Events, "compaction"), len(script))
	}
	first := mc.reqs[0].Messages[0]
	if first.Role != "system" || !strings.HasSuffix(first.Content, addendum) {
		t.Fatalf("request 0 opens with %s %q, want the system prompt ending in the addendum", first.Role, first.Content)
	}
	for k, req := range mc.reqs[1:] {
		if !reflect.DeepEqual(req.Messages[0], first) {
			t.Errorf("request %d system message = %q, want byte-identical to request 0's %q", k+1, req.Messages[0].Content, first.Content)
		}
	}
}

// TestLegacyDropsHistoryBeforeChainsWithDurableSummary pins the documented
// drop order on the path every Golem session past its first cross-turn
// compression takes: assembleLegacy materializes the durable summary as a
// pinned system message at index 0, and the prior-turn exchanges before the
// goal must still drop before any current-run tool chain.
func TestLegacyDropsHistoryBeforeChainsWithDurableSummary(t *testing.T) {
	summary := DurableSummaryPrompt("earlier work")
	st := State{DurableSummary: "earlier work", Messages: []Message{
		history("user", strings.Repeat("a", 50)), history("assistant", strings.Repeat("A", 50)), // 100
		history("user", strings.Repeat("b", 50)), history("assistant", strings.Repeat("B", 50)), // 100
		pinned("user", strings.Repeat("G", 10)),
		mixedAsstCall("c1"), mixedToolResult("c1", strings.Repeat("1", 30), nil, 0), // 60
		mixedAsstCall("c2"), mixedToolResult("c2", strings.Repeat("2", 30), nil, 0), // 60
	}}
	// One token over: the just-enough fit drops one exchange, and the 75%
	// mark is reached by dropping both, with both chains kept.
	input := runeEstimator(summary) + 330 - 1
	for _, tc := range []struct {
		lowWater float64
		want     []string
	}{
		{0, []string{summary, strings.Repeat("b", 50), strings.Repeat("B", 50), strings.Repeat("G", 10), "", strings.Repeat("1", 30), "", strings.Repeat("2", 30)}},
		{0.75, []string{summary, strings.Repeat("G", 10), "", strings.Repeat("1", 30), "", strings.Repeat("2", 30)}},
	} {
		t.Run(fmt.Sprint(tc.lowWater), func(t *testing.T) {
			m := ContextManager{Estimate: runeEstimator, CompactionLowWater: tc.lowWater}
			out, _, err := m.Assemble(context.Background(), st, 0, TokenBudget{Input: input})
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			if got := msgContents(out); !slices.Equal(got, tc.want) {
				t.Errorf("kept %q, want %q: prior-turn history drops before current-run chains", shape(out), shape(State{Messages: messagesOf(tc.want)}))
			}
		})
	}
}

// messagesOf wraps contents as messages, for shape in failure output.
func messagesOf(contents []string) []Message {
	out := make([]Message, len(contents))
	for i, c := range contents {
		out[i] = elastic("", c)
	}
	return out
}

// TestStickyEvictionNeedsBuiltInAssembly pins ruling P2-a: hiding needs a
// low-water mark and built-in assembly, whichever form the built-in
// compactor takes.
func TestStickyEvictionNeedsBuiltInAssembly(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    ContextManager
		want bool
	}{
		{"no low-water", ContextManager{}, false},
		{"nil compactor", ContextManager{CompactionLowWater: 0.75}, true},
		{"RecencyCompactor value", ContextManager{Compactor: RecencyCompactor{}, CompactionLowWater: 0.75}, true},
		{"RecencyCompactor pointer", ContextManager{Compactor: &RecencyCompactor{}, CompactionLowWater: 0.75}, true},
		{"mixed", ContextManager{Mixed: true, CompactionLowWater: 0.75}, true},
		{"custom compactor", ContextManager{Compactor: passthroughCompactor{}, CompactionLowWater: 0.75}, false},
	} {
		if got := tc.m.stickyEviction(); got != tc.want {
			t.Errorf("%s: stickyEviction = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestRunValidatesCompactionLowWater(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, ""},
		{0.75, ""},
		{-0.1, "agent: compaction low-water -0.1 outside (0,1)"},
		{1, "agent: compaction low-water 1 outside (0,1)"},
		{1.5, "agent: compaction low-water 1.5 outside (0,1)"},
		{math.NaN(), "agent: compaction low-water NaN outside (0,1)"},
	} {
		t.Run(fmt.Sprint(tc.v), func(t *testing.T) {
			mc := &scriptedCaller{responses: []ModelResult{finalAnswer("ok")}}
			_, err := lowWaterOrchestrator(mc, tc.v).Run(context.Background(), Request{Goal: "q"}, nil)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Run: %v, want no error", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("Run error = %v, want %q", err, tc.want)
			case tc.want != "" && mc.calls != 0:
				t.Fatalf("%d model calls, want none before the check", mc.calls)
			}
		})
	}
}
