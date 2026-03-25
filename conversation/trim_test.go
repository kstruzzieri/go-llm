package conversation

import (
	"encoding/json"
	"testing"
)

// lenEstimator counts characters as tokens (1:1) for deterministic tests.
func lenEstimator(s string) int { return len(s) }

// makeMsg is a test helper for building messages.
func makeMsg(role, content string) Message {
	return Message{Role: role, Content: content}
}

// makeToolCallMsg builds an assistant message with tool calls.
func makeToolCallMsg(content string, toolCallsJSON string) Message {
	return Message{
		Role:      "assistant",
		Content:   content,
		ToolCalls: json.RawMessage(toolCallsJSON),
	}
}

// makeToolResult builds a tool result message.
func makeToolResult(content, toolName, callID string) Message {
	return Message{
		Role:       "tool",
		Content:    content,
		ToolName:   toolName,
		ToolCallID: callID,
	}
}

func TestTrimMessages_AllFit(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "hi"),
		makeMsg("assistant", "hello"),
	}
	result := TrimMessages(msgs, 1000, lenEstimator)
	if result.TrimmedCount != 0 {
		t.Errorf("TrimmedCount = %d, want 0", result.TrimmedCount)
	}
	if len(result.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(result.Messages))
	}
}

func TestTrimMessages_Empty(t *testing.T) {
	result := TrimMessages(nil, 100, lenEstimator)
	if len(result.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(result.Messages))
	}
	if result.TrimmedCount != 0 {
		t.Errorf("TrimmedCount = %d, want 0", result.TrimmedCount)
	}
}

func TestTrimMessages_SystemAlwaysPreserved(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "You are helpful."),
		makeMsg("user", "hello"),
		makeMsg("assistant", "hi there how are you doing today"),
		makeMsg("user", "fine"),
		makeMsg("assistant", "great"),
	}
	result := TrimMessages(msgs, 30, lenEstimator)
	if result.Messages[0].Role != "system" {
		t.Error("system message missing")
	}
	for _, m := range result.Messages {
		if m.Role == "system" {
			return
		}
	}
	t.Error("system message not found in result")
}

func TestTrimMessages_ToolPairsAtomic(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "search"),
		makeToolCallMsg("", `[{"id":"c1","type":"function","function":{"name":"search","arguments":{}}}]`),
		makeToolResult("result data here", "search", "c1"),
		makeMsg("assistant", "found it"),
		makeMsg("user", "thanks"),
		makeMsg("assistant", "welcome"),
	}
	result := TrimMessages(msgs, 40, lenEstimator)

	for i, m := range result.Messages {
		if m.Role == "tool" {
			found := false
			for j := i - 1; j >= 0; j-- {
				if result.Messages[j].Role == "assistant" && result.Messages[j].ToolCalls != nil {
					found = true
					break
				}
				if result.Messages[j].Role != "tool" {
					break
				}
			}
			if !found {
				t.Errorf("orphaned tool result at index %d", i)
			}
		}
	}
}

func TestTrimMessages_FirstNonSystemMustBeUser(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys"),
		makeMsg("assistant", "summary"),
		makeMsg("user", "hello"),
		makeMsg("assistant", "hi"),
	}
	result := TrimMessages(msgs, 13, lenEstimator)
	for _, m := range result.Messages {
		if m.Role == "system" {
			continue
		}
		if m.Role != "user" {
			t.Errorf("first non-system message role = %q, want user", m.Role)
		}
		break
	}
}

func TestTrimMessages_FirstNonSystemMustBeUser_NoTrimNeeded(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys"),
		makeMsg("assistant", "summary"),
		makeMsg("user", "hello"),
		makeMsg("assistant", "hi"),
	}
	result := TrimMessages(msgs, 1000, lenEstimator)
	for _, m := range result.Messages {
		if m.Role == "system" {
			continue
		}
		if m.Role != "user" {
			t.Errorf("first non-system message role = %q, want user (even without budget pressure)", m.Role)
		}
		break
	}
}

func TestTrimMessages_TieredStrategy_ToolChainsFirst(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "aaa"),
		makeToolCallMsg("", `[{"id":"c1","type":"function","function":{"name":"f","arguments":{"x":"long-argument-value-here"}}}]`),
		makeToolResult("long tool result content here!!!", "f", "c1"),
		makeMsg("assistant", "synthesized"),
		makeMsg("user", "bbb"),
		makeMsg("assistant", "short"),
	}

	totalCost := 0
	for _, m := range msgs {
		totalCost += messageCost(m, lenEstimator)
	}
	chainCost := messageCost(msgs[1], lenEstimator) +
		messageCost(msgs[2], lenEstimator)

	budget := totalCost - chainCost + 5
	result := TrimMessages(msgs, budget, lenEstimator)

	if result.Messages[0].Content != "aaa" {
		t.Errorf("expected first user msg preserved, got %q", result.Messages[0].Content)
	}
	for _, m := range result.Messages {
		if m.Role == "tool" {
			t.Error("tool message should have been trimmed by tiered strategy")
		}
	}
	if result.TrimmedCount < 2 {
		t.Errorf("TrimmedCount = %d, want >= 2", result.TrimmedCount)
	}
}

func TestTrimMessages_UnresolvedTailPreserved(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "old question"),
		makeMsg("assistant", "old answer that is rather long to push budget"),
		makeMsg("user", "do something"),
		makeToolCallMsg("", `[{"id":"c1","type":"function","function":{"name":"act","arguments":{}}}]`),
		makeToolResult("pending result", "act", "c1"),
	}

	result := TrimMessages(msgs, 60, lenEstimator)

	lastTwo := result.Messages[len(result.Messages)-2:]
	if lastTwo[0].ToolCalls == nil {
		t.Error("unresolved assistant tool-call message was trimmed")
	}
	if lastTwo[1].Role != "tool" {
		t.Error("unresolved tool result was trimmed")
	}
}

func TestTrimMessages_SingleUserExceedsBudget(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "this message is way longer than the budget allows"),
	}
	result := TrimMessages(msgs, 5, lenEstimator)
	if len(result.Messages) != 1 {
		t.Errorf("Messages len = %d, want 1 (preserve at least one)", len(result.Messages))
	}
}

func TestTrimMessages_OnlySystemFits(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "short"),
		makeMsg("user", "this is a long user message that exceeds budget"),
		makeMsg("assistant", "this is a long assistant message that exceeds budget"),
	}
	result := TrimMessages(msgs, 6, lenEstimator)
	if len(result.Messages) < 1 || result.Messages[0].Role != "system" {
		t.Error("system message should be preserved even when nothing else fits")
	}
}

func TestTrimMessages_AllSystem(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys1"),
		makeMsg("system", "sys2"),
	}
	result := TrimMessages(msgs, 100, lenEstimator)
	if len(result.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(result.Messages))
	}
	if result.TrimmedCount != 0 {
		t.Errorf("TrimmedCount = %d, want 0", result.TrimmedCount)
	}
}

func TestTrimMessages_ZeroBudget(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys"),
		makeMsg("user", "hello"),
		makeMsg("assistant", "hi"),
	}
	result := TrimMessages(msgs, 0, lenEstimator)
	if result.Messages[0].Role != "system" {
		t.Error("system message missing")
	}
}

func TestTrimByExchanges_Basic(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys"),
		makeMsg("user", "q1"),
		makeMsg("assistant", "a1"),
		makeMsg("user", "q2"),
		makeMsg("assistant", "a2"),
		makeMsg("user", "q3"),
		makeMsg("assistant", "a3"),
	}
	result := TrimByExchanges(msgs, 2)
	if result.Messages[0].Role != "system" {
		t.Error("system missing")
	}
	if len(result.Messages) != 5 {
		t.Errorf("Messages len = %d, want 5", len(result.Messages))
	}
	if result.Messages[1].Content != "q2" {
		t.Errorf("expected q2, got %q", result.Messages[1].Content)
	}
	if result.EstimatedTokens != 0 {
		t.Errorf("EstimatedTokens = %d, want 0 for TrimByExchanges", result.EstimatedTokens)
	}
}

func TestTrimByExchanges_WithToolChain(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "q1"),
		makeMsg("assistant", "a1"),
		makeMsg("user", "search"),
		makeToolCallMsg("", `[{"id":"c1","type":"function","function":{"name":"s","arguments":{}}}]`),
		makeToolResult("data", "s", "c1"),
		makeMsg("assistant", "found it"),
		makeMsg("user", "q3"),
		makeMsg("assistant", "a3"),
	}
	result := TrimByExchanges(msgs, 1)
	if len(result.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(result.Messages))
	}
	if result.Messages[0].Content != "q3" {
		t.Errorf("expected q3, got %q", result.Messages[0].Content)
	}
}

func TestTrimByExchanges_UnresolvedTail(t *testing.T) {
	msgs := []Message{
		makeMsg("user", "q1"),
		makeMsg("assistant", "a1"),
		makeMsg("user", "do it"),
		makeToolCallMsg("", `[{"id":"c1","type":"function","function":{"name":"act","arguments":{}}}]`),
		makeToolResult("pending", "act", "c1"),
	}
	result := TrimByExchanges(msgs, 0)
	found := false
	for _, m := range result.Messages {
		if m.ToolCalls != nil {
			found = true
		}
	}
	if !found {
		t.Error("unresolved tail should be preserved even with 0 exchanges")
	}
}

func TestTrimByExchanges_ZeroExchanges(t *testing.T) {
	msgs := []Message{
		makeMsg("system", "sys"),
		makeMsg("user", "q1"),
		makeMsg("assistant", "a1"),
	}
	result := TrimByExchanges(msgs, 0)
	if len(result.Messages) != 1 {
		t.Errorf("Messages len = %d, want 1 (system only)", len(result.Messages))
	}
	if result.Messages[0].Role != "system" {
		t.Error("expected system message only")
	}
}
