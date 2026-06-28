package main

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestResultMessagesRedactsMemorySearch(t *testing.T) {
	const secret = "USER PREFERS A SECRET THING"
	res := agent.Result{
		Answer: "ok",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "q"},
			{Role: "tool", ToolName: agenttools.MemorySearchToolName, ToolCallID: "c1", Content: secret},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultConversationMessages("q", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if strings.Contains(m.Content, secret) {
			t.Fatalf("memory text leaked into persisted history: %q", m.Content)
		}
		if m.ToolName == agenttools.MemorySearchToolName {
			found = true
			if m.Content != memorySearchRedactedMarker {
				t.Errorf("tool content = %q, want marker %q", m.Content, memorySearchRedactedMarker)
			}
		}
	}
	if !found {
		t.Error("memory_search tool message not persisted")
	}
}
