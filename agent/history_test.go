package agent

import (
	"context"
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
	req := buildChatRequest(st, nil, 0)
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
	// System "" + no tools => pinned cost = len("GOAL") = 4 < ceiling 20.
	_, err := o.Run(context.Background(), Request{
		Goal:    "GOAL",
		History: hist,
		Budget:  Budget{InputCeiling: 20},
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
