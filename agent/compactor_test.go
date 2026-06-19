package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// fixed estimator: 1 token per rune of Content, ignores role overhead.
func runeEstimator(s string) int { return len([]rune(s)) }

func pinned(role, content string) Message {
	return Message{ChatMessage: provider.ChatMessage{Role: role, Content: content}, Segment: Pinned}
}
func elastic(role, content string) Message {
	return Message{ChatMessage: provider.ChatMessage{Role: role, Content: content}, Segment: Elastic}
}

func TestRecencyCompactorKeepsPinnedDropsOldestElastic(t *testing.T) {
	st := State{
		System: "", // empty so token math is exactly the message contents
		Messages: []Message{
			pinned("user", "GOAL"),        // 4 (pinned, always kept)
			elastic("assistant", "OLD"),   // 3 (oldest, dropped)
			elastic("user", "MIDDLE"),     // 6
			elastic("assistant", "NEWER"), // 5
		},
	}
	// budget = GOAL(4) + MIDDLE(6) + NEWER(5) = 15; OLD must be evicted.
	rc := RecencyCompactor{Estimate: runeEstimator}
	out, rep, err := rc.Compact(context.Background(), st, TokenBudget{Input: 4 + 6 + 5})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if rep.DroppedCount != 1 {
		t.Fatalf("DroppedCount = %d, want 1", rep.DroppedCount)
	}
	if len(out.Messages) != 3 || out.Messages[0].Content != "GOAL" {
		t.Fatalf("pinned goal must survive, got %+v", out.Messages)
	}
	for _, m := range out.Messages {
		if m.Content == "OLD" {
			t.Fatal("oldest elastic message should have been dropped")
		}
	}
	if rep.Strategy != "recency" || rep.TokensAfter > rep.TokensBefore {
		t.Fatalf("bad report: %+v", rep)
	}
}

func TestRecencyCompactorDropsToolChainsAtomically(t *testing.T) {
	asst := Message{ChatMessage: provider.ChatMessage{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)},
		}},
	}, Segment: Elastic}
	toolResult := Message{ChatMessage: provider.ChatMessage{
		Role: "tool", ToolName: "search", ToolCallID: "c1", Content: "RESULT-DATA",
	}, Segment: Elastic}

	st := State{
		System: "", // empty so token math is exactly the message contents
		Messages: []Message{
			pinned("user", "GOAL"),
			asst, toolResult, // oldest completed chain — must drop together
			elastic("assistant", "FINAL-ANSWER"),
		},
	}
	// budget = GOAL(4) + FINAL-ANSWER(12) = 16; the [asst,toolResult] chain
	// includes prompt-visible tool fields and is evicted atomically.
	rc := RecencyCompactor{Estimate: runeEstimator}
	out, _, err := rc.Compact(context.Background(), st, TokenBudget{Input: 4 + len("FINAL-ANSWER")})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	for _, m := range out.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("partial tool chain survived: %+v", out.Messages)
		}
	}
}

func TestRecencyCompactorCountsPromptVisibleToolFields(t *testing.T) {
	asst := Message{ChatMessage: provider.ChatMessage{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{{
			ID: "expensive-call-id", Type: "function",
			Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(`{"query":"long"}`)},
		}},
	}, Segment: Elastic}
	toolResult := Message{ChatMessage: provider.ChatMessage{
		Role: "tool", ToolName: "search", ToolCallID: "expensive-call-id",
	}, Segment: Elastic}
	st := State{
		Messages: []Message{
			pinned("user", "G"),
			asst, toolResult, // completed chain has no Content, but still costs tokens
			elastic("assistant", "FINAL"),
		},
	}

	rc := RecencyCompactor{Estimate: runeEstimator}
	out, _, err := rc.Compact(context.Background(), st, TokenBudget{Input: len("G") + len("FINAL")})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out.Messages) != 2 || out.Messages[1].Content != "FINAL" {
		t.Fatalf("tool fields must count toward budget and force chain eviction, got %+v", out.Messages)
	}
}

func TestRecencyCompactorDropsCompletedToolChainsBeforePlainHistory(t *testing.T) {
	asst := Message{ChatMessage: provider.ChatMessage{
		Role: "assistant",
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(`{}`)},
		}},
	}, Segment: Elastic}
	toolResult := Message{ChatMessage: provider.ChatMessage{
		Role: "tool", ToolName: "search", ToolCallID: "c1", Content: "TOOLRESULT",
	}, Segment: Elastic}
	st := State{Messages: []Message{
		pinned("user", "G"),
		elastic("assistant", "PLAIN"),
		asst, toolResult,
		elastic("assistant", "FINAL"),
	}}

	rc := RecencyCompactor{Estimate: runeEstimator}
	out, _, err := rc.Compact(context.Background(), st, TokenBudget{Input: len("G") + len("PLAIN") + len("FINAL")})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out.Messages) != 3 || out.Messages[1].Content != "PLAIN" || out.Messages[2].Content != "FINAL" {
		t.Fatalf("completed tool chain should drop before plain history, got %+v", out.Messages)
	}
}

func TestRecencyCompactorPreservesOriginalOrder(t *testing.T) {
	st := State{Messages: []Message{
		elastic("assistant", "DROP"),
		elastic("assistant", "KEEP"),
		pinned("user", "PIN"),
		elastic("assistant", "NEW"),
	}}
	rc := RecencyCompactor{Estimate: runeEstimator}
	out, _, err := rc.Compact(context.Background(), st, TokenBudget{Input: len("KEEP") + len("PIN") + len("NEW")})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	got := []string{out.Messages[0].Content, out.Messages[1].Content, out.Messages[2].Content}
	want := []string{"KEEP", "PIN", "NEW"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
