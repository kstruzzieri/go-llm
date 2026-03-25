package conversation

import (
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestFromChatMessages_SimpleRoundTrip(t *testing.T) {
	original := []ollama.ChatMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", Content: "4"},
	}

	converted, err := FromChatMessages(original)
	if err != nil {
		t.Fatalf("FromChatMessages() error: %v", err)
	}
	if len(converted) != 3 {
		t.Fatalf("len = %d, want 3", len(converted))
	}

	restored, err := ToChatMessages(converted)
	if err != nil {
		t.Fatalf("ToChatMessages() error: %v", err)
	}
	if len(restored) != 3 {
		t.Fatalf("restored len = %d, want 3", len(restored))
	}

	for i, msg := range restored {
		if msg.Role != original[i].Role || msg.Content != original[i].Content {
			t.Errorf("[%d] = {%s, %q}, want {%s, %q}",
				i, msg.Role, msg.Content, original[i].Role, original[i].Content)
		}
	}
}

func TestFromChatMessages_ToolCallRoundTrip(t *testing.T) {
	original := []ollama.ChatMessage{
		{Role: "user", Content: "Search for Go tutorials"},
		{
			Role: "assistant",
			ToolCalls: []ollama.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: ollama.ToolCallFunction{
						Index: 0,
						Name:  "search",
						Arguments: map[string]any{
							"query":  "Go tutorials",
							"limit":  float64(10),
							"nested": map[string]any{"key": "value"},
							"tags":   []any{"go", "tutorial"},
						},
					},
				},
			},
		},
		{
			Role:       "tool",
			Content:    `{"results": ["result1", "result2"]}`,
			ToolName:   "search",
			ToolCallID: "call_1",
		},
		{Role: "assistant", Content: "I found some tutorials."},
	}

	converted, err := FromChatMessages(original)
	if err != nil {
		t.Fatalf("FromChatMessages() error: %v", err)
	}

	if converted[1].ToolCalls == nil {
		t.Fatal("converted[1].ToolCalls is nil")
	}

	if converted[2].ToolName != "search" {
		t.Errorf("ToolName = %q, want %q", converted[2].ToolName, "search")
	}
	if converted[2].ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want %q", converted[2].ToolCallID, "call_1")
	}

	restored, err := ToChatMessages(converted)
	if err != nil {
		t.Fatalf("ToChatMessages() error: %v", err)
	}

	if len(restored[1].ToolCalls) != 1 {
		t.Fatalf("restored[1].ToolCalls len = %d, want 1", len(restored[1].ToolCalls))
	}
	tc := restored[1].ToolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("ToolCall.ID = %q, want %q", tc.ID, "call_1")
	}
	if tc.Function.Name != "search" {
		t.Errorf("ToolCall.Function.Name = %q, want %q", tc.Function.Name, "search")
	}
	if tc.Function.Arguments["query"] != "Go tutorials" {
		t.Errorf("Arguments[query] = %v, want %q", tc.Function.Arguments["query"], "Go tutorials")
	}

	if restored[2].ToolCallID != "call_1" {
		t.Errorf("restored ToolCallID = %q, want %q", restored[2].ToolCallID, "call_1")
	}
}

func TestFromChatMessages_NilToolCalls(t *testing.T) {
	original := []ollama.ChatMessage{
		{Role: "user", Content: "hello"},
	}

	converted, err := FromChatMessages(original)
	if err != nil {
		t.Fatalf("FromChatMessages() error: %v", err)
	}
	if converted[0].ToolCalls != nil {
		t.Error("ToolCalls should be nil for non-tool message")
	}

	restored, err := ToChatMessages(converted)
	if err != nil {
		t.Fatalf("ToChatMessages() error: %v", err)
	}
	if restored[0].ToolCalls != nil {
		t.Error("restored ToolCalls should be nil")
	}
}

func TestToChatMessages_CorruptedJSON(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: json.RawMessage(`{broken json`)},
	}
	_, err := ToChatMessages(msgs)
	if err == nil {
		t.Fatal("ToChatMessages() should return error for corrupted JSON")
	}
}
