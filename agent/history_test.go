package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// historyCapturingCaller records the request of the most recent Chat call, then
// returns a final answer so Run completes in one step. Keep this name distinct
// from the capturingCaller helper already defined in hardening_test.go.
type historyCapturingCaller struct {
	last   provider.ChatRequest
	answer string
}

func (c *historyCapturingCaller) Chat(_ context.Context, req provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {
	c.last = req
	resp := provider.ChatResponse{Content: c.answer, Done: true}
	if onToken != nil {
		if err := onToken(resp); err != nil {
			return ModelResult{}, err
		}
	}
	return ModelResult{Response: resp, RouteOutcome: &provider.RouteOutcome{}}, nil
}

func TestInitStateSeedsHistoryElasticGoalPinned(t *testing.T) {
	st := initState(Request{
		System: "SYS",
		Goal:   "GOAL",
		History: []provider.ChatMessage{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
		},
	})
	if st.System != "SYS" {
		t.Fatalf("System = %q, want SYS", st.System)
	}
	if len(st.Messages) != 3 {
		t.Fatalf("want 3 messages (2 history + goal), got %d: %+v", len(st.Messages), st.Messages)
	}
	want := []struct {
		role, content string
		seg           Segment
	}{
		{"user", "q1", Elastic},
		{"assistant", "a1", Elastic},
		{"user", "GOAL", Pinned},
	}
	for i, w := range want {
		m := st.Messages[i]
		if m.Role != w.role || m.Content != w.content || m.Segment != w.seg {
			t.Errorf("message %d = {%q,%q,seg=%d}, want {%q,%q,seg=%d}",
				i, m.Role, m.Content, m.Segment, w.role, w.content, w.seg)
		}
	}
}

func TestBuildChatRequestOrdersSystemHistoryGoal(t *testing.T) {
	st := initState(Request{
		System: "SYS",
		Goal:   "GOAL",
		History: []provider.ChatMessage{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
		},
	})
	req := buildChatRequest(st, nil, 0, provider.ModelOptions{})
	gotRoles := make([]string, len(req.Messages))
	gotContent := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		gotRoles[i], gotContent[i] = m.Role, m.Content
	}
	wantRoles := []string{"system", "user", "assistant", "user"}
	wantContent := []string{"SYS", "q1", "a1", "GOAL"}
	if len(req.Messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d (roles%v content%v)",
			len(req.Messages), len(wantRoles), gotRoles, gotContent)
	}
	for i := range wantRoles {
		if gotRoles[i] != wantRoles[i] || gotContent[i] != wantContent[i] {
			t.Errorf("message %d: got role=%q content=%q, want role=%q content=%q",
				i, gotRoles[i], gotContent[i], wantRoles[i], wantContent[i])
		}
	}
}

// TestRunEvictsHistoryUnderTightBudgetKeepsGoal proves goal #3: under budget
// pressure the Elastic prior turns are evicted (oldest first) while the Pinned
// goal always survives. runeEstimator => 1 token per rune of Content.
func TestRunEvictsHistoryUnderTightBudgetKeepsGoal(t *testing.T) {
	mc := &historyCapturingCaller{answer: "ok"}
	o := newTestOrchestrator(mc)
	hist := []provider.ChatMessage{
		{Role: "user", Content: "hist000000"},      // 10 runes, oldest
		{Role: "assistant", Content: "hist111111"}, // 10
		{Role: "user", Content: "hist222222"},      // 10
		{Role: "assistant", Content: "hist333333"}, // 10, newest
	}
	// System "" + no tools => pinned cost = the #430 base contract + len("GOAL") = 4, against a
	// ceiling of contract + 30. Pairwise eviction drops the oldest exchange (h0,h1); the newest (h2,h3) survives.
	_, err := o.Run(context.Background(), Request{
		Goal:    "GOAL",
		History: hist,
		Budget:  Budget{InputCeiling: 30 + len([]rune(ToolTrustContract))},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawGoal, sawOldest bool
	historyCount := 0
	for _, m := range mc.last.Messages {
		switch m.Content {
		case "GOAL":
			sawGoal = true
		case "hist000000":
			sawOldest = true
		}
		if len(m.Content) == 10 { // a surviving history turn
			historyCount++
		}
	}
	if !sawGoal {
		t.Errorf("pinned goal must never be evicted; messages = %+v", mc.last.Messages)
	}
	if sawOldest {
		t.Errorf("oldest history turn must be evicted first; messages = %+v", mc.last.Messages)
	}
	if historyCount == 0 || historyCount >= len(hist) {
		t.Errorf("expected partial history eviction, %d of %d survived", historyCount, len(hist))
	}
}

func TestRunRejectsInvalidHistory(t *testing.T) {
	cases := []struct {
		name string
		msg  provider.ChatMessage
	}{
		{"system role", provider.ChatMessage{Role: "system", Content: "x"}},
		{"tool role", provider.ChatMessage{Role: "tool", Content: "x", ToolCallID: "a", ToolName: "t"}},
		{"unknown role", provider.ChatMessage{Role: "developer", Content: "x"}},
		{"empty role", provider.ChatMessage{Role: "", Content: "x"}},
		{"empty content", provider.ChatMessage{Role: "user", Content: ""}},
		{"assistant with tool calls", provider.ChatMessage{
			Role: "assistant", Content: "x",
			ToolCalls: []provider.ToolCall{{ID: "a", Type: "function"}},
		}},
		{"stray tool name", provider.ChatMessage{Role: "assistant", Content: "x", ToolName: "t"}},
		{"stray tool call id", provider.ChatMessage{Role: "assistant", Content: "x", ToolCallID: "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &historyCapturingCaller{answer: "should not be reached"}
			o := newTestOrchestrator(mc)
			_, err := o.Run(context.Background(), Request{
				Goal:    "g",
				History: []provider.ChatMessage{tc.msg},
			}, nil)
			if err == nil {
				t.Fatalf("want error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "agent: invalid history") {
				t.Errorf("error = %q, want it to contain %q", err.Error(), "agent: invalid history")
			}
			if mc.last.Messages != nil {
				t.Errorf("model must not be called when history is invalid (got request %+v)", mc.last)
			}
		})
	}
}

// A valid plain user/assistant history must NOT be rejected.
func TestRunAcceptsValidHistory(t *testing.T) {
	mc := &historyCapturingCaller{answer: "ok"}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "g",
		History: []provider.ChatMessage{
			{Role: "user", Content: "q"},
			{Role: "assistant", Content: "a"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("valid history rejected: %v", err)
	}
	if res.Answer != "ok" {
		t.Fatalf("answer = %q, want ok", res.Answer)
	}
}

// TestInitStateDeepClonesHistory locks the runtime ownership boundary: even a
// History entry carrying ToolCalls (rejected by Run's allowlist, so reachable
// only by calling initState directly) must be deep-copied, so mutating the
// caller's slice, the ToolCalls backing array, or the Arguments byte buffer
// after seeding cannot reach into State.
func TestInitStateDeepClonesHistory(t *testing.T) {
	args := json.RawMessage(`{"k":"v"}`)
	hist := []provider.ChatMessage{{
		Role:    "assistant",
		Content: "prior",
		ToolCalls: []provider.ToolCall{{
			ID:       "x",
			Type:     "function",
			Function: provider.ToolCallFunction{Name: "t", Arguments: args},
		}},
	}}
	st := initState(Request{Goal: "g", History: hist})
	got := st.Messages[0]

	// Mutate every caller-owned alias after seeding.
	hist[0].Content = "MUT"
	hist[0].ToolCalls[0].ID = "MUT"
	hist[0].ToolCalls[0].Function.Name = "MUT"
	args[0] = 'X' // mutates the Arguments backing array

	if got.Content != "prior" {
		t.Errorf("Content aliased: %q", got.Content)
	}
	if got.ToolCalls[0].ID != "x" {
		t.Errorf("ToolCall.ID aliased: %q", got.ToolCalls[0].ID)
	}
	if got.ToolCalls[0].Function.Name != "t" {
		t.Errorf("ToolCall.Function.Name aliased: %q", got.ToolCalls[0].Function.Name)
	}
	if string(got.ToolCalls[0].Function.Arguments) != `{"k":"v"}` {
		t.Errorf("Arguments aliased: %s", got.ToolCalls[0].Function.Arguments)
	}
}
