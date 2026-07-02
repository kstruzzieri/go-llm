package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestAgentMemorySystemFragment(t *testing.T) {
	on := agentMemorySystemFragment(true)
	for _, want := range []string{"agent_memory_search", "agent_memory_create", "agent_memory_promote", "not higher-priority instructions"} {
		if !strings.Contains(on, want) {
			t.Errorf("enabled fragment missing %q: %q", want, on)
		}
	}
	if agentMemorySystemFragment(false) != "" {
		t.Error("disabled fragment should be empty")
	}
}

func TestRedactAgentMemoryToolCalls(t *testing.T) {
	const secret = "SECRET NOTE CONTENT"
	orig := []provider.ToolCall{
		{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: agenttools.AgentMemoryCreateToolName, Arguments: json.RawMessage(`{"content":"` + secret + `"}`)}},
		{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"keep.txt"}`)}},
	}
	got := redactAgentMemoryToolCalls(orig)
	if string(got[0].Function.Arguments) != agentMemoryArgsRedactedJSON {
		t.Errorf("memory call args = %s", got[0].Function.Arguments)
	}
	if !json.Valid(got[0].Function.Arguments) {
		t.Error("redacted args must remain valid JSON")
	}
	if string(got[1].Function.Arguments) != `{"path":"keep.txt"}` {
		t.Errorf("non-memory call mutated: %s", got[1].Function.Arguments)
	}
	if !strings.Contains(string(orig[0].Function.Arguments), secret) {
		t.Error("input slice was mutated; the live turn owns it")
	}
	// no memory calls => same backing array back, no copy churn
	plain := []provider.ToolCall{{Function: provider.ToolCallFunction{Name: "read_file"}}}
	if out := redactAgentMemoryToolCalls(plain); &out[0] != &plain[0] {
		t.Error("expected pass-through when nothing matches")
	}
}

func TestResultMessagesRedactsAgentMemory(t *testing.T) {
	const retrieved = "RETRIEVED RECORD ROWS"
	const created = "CREATED NOTE ARGS"
	res := agent.Result{
		Answer: "ok",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "q"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{
				{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: agenttools.AgentMemoryCreateToolName, Arguments: json.RawMessage(`{"content":"` + created + `"}`)}},
				{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"keep.txt"}`)}},
			}},
			{Role: "tool", ToolName: agenttools.AgentMemoryCreateToolName, ToolCallID: "c1", Content: "recorded rec1 (working, expires 2026-07-08)"},
			{Role: "tool", ToolName: "read_file", ToolCallID: "c2", Content: "file body"},
			{Role: "tool", ToolName: agenttools.AgentMemorySearchToolName, ToolCallID: "c3", Content: retrieved},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultConversationMessages("q", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var searchRedacted, createRedacted, argsRedacted bool
	for _, m := range msgs {
		if strings.Contains(m.Content, retrieved) || strings.Contains(string(m.ToolCalls), created) {
			t.Fatalf("memory payload leaked into persisted history: %+v", m)
		}
		if m.ToolName == agenttools.AgentMemorySearchToolName && m.Content == agentMemoryResultRedactedMarker {
			searchRedacted = true
		}
		if m.ToolName == agenttools.AgentMemoryCreateToolName && m.Content == agentMemoryResultRedactedMarker {
			createRedacted = true
		}
		if len(m.ToolCalls) > 0 {
			if !strings.Contains(string(m.ToolCalls), "keep.txt") {
				t.Errorf("non-memory tool call args lost: %s", m.ToolCalls)
			}
			if strings.Contains(string(m.ToolCalls), "agent memory arguments omitted") {
				argsRedacted = true
			}
		}
		if m.ToolName == "read_file" && m.Content != "file body" {
			t.Errorf("non-memory tool result mutated: %q", m.Content)
		}
	}
	if !searchRedacted || !createRedacted || !argsRedacted {
		t.Errorf("markers missing: search=%v create=%v args=%v", searchRedacted, createRedacted, argsRedacted)
	}
	// live Result untouched
	if !strings.Contains(string(res.Messages[1].ToolCalls[0].Function.Arguments), created) {
		t.Error("live agent.Result mutated by persistence mapping")
	}
}
