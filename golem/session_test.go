package golem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestResultMessagesRedactsMemoryData(t *testing.T) {
	const secret = "do not persist this"
	result := agent.Result{Messages: []provider.ChatMessage{
		{Role: "user", Content: "question"},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      agenttools.AgentMemoryCreateToolName,
					Arguments: json.RawMessage(`{"content":"` + secret + `"}`),
				},
			}},
		},
		{Role: "tool", ToolName: agenttools.AgentMemoryCreateToolName, ToolCallID: "call-1", Content: secret},
		{Role: "tool", ToolName: agenttools.MemorySearchToolName, ToolCallID: "call-2", Content: secret},
	}}

	messages, err := resultMessages("question", result)
	if err != nil {
		t.Fatalf("resultMessages: %v", err)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal persisted messages: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("persisted messages contain memory data: %s", raw)
	}
}

func TestRedactAgentMemoryToolCallsPreservesInput(t *testing.T) {
	const secret = "SECRET NOTE CONTENT"
	orig := []provider.ToolCall{
		{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: agenttools.AgentMemoryCreateToolName, Arguments: json.RawMessage(`{"content":"` + secret + `"}`)}},
		{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"keep.txt"}`)}},
	}
	got := redactAgentMemoryToolCalls(orig)
	if string(got[0].Function.Arguments) != agentMemoryArgsRedactedMarker {
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

func TestResultMessagesRedactsAgentMemoryMarkers(t *testing.T) {
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
			{Role: "tool", ToolName: agenttools.AgentMemoryCreateToolName, ToolCallID: "c1", Content: "recorded rec1 (working)"},
			{Role: "tool", ToolName: "read_file", ToolCallID: "c2", Content: "file body"},
			{Role: "tool", ToolName: agenttools.AgentMemorySearchToolName, ToolCallID: "c3", Content: retrieved},
			{Role: "tool", ToolName: agenttools.MemorySearchToolName, ToolCallID: "c4", Content: retrieved},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultMessages("q", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var searchRedacted, createRedacted, argsRedacted, memSearchRedacted bool
	for _, m := range msgs {
		if strings.Contains(m.Content, retrieved) || strings.Contains(string(m.ToolCalls), created) {
			t.Fatalf("memory payload leaked into persisted history: %+v", m)
		}
		if m.ToolName == agenttools.AgentMemorySearchToolName && m.Content == agentMemoryRedactedMarker {
			searchRedacted = true
		}
		if m.ToolName == agenttools.AgentMemoryCreateToolName && m.Content == agentMemoryRedactedMarker {
			createRedacted = true
		}
		if m.ToolName == agenttools.MemorySearchToolName && m.Content == memorySearchRedactedMarker {
			memSearchRedacted = true
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
	if !searchRedacted || !createRedacted || !argsRedacted || !memSearchRedacted {
		t.Errorf("markers missing: agentSearch=%v create=%v args=%v memSearch=%v",
			searchRedacted, createRedacted, argsRedacted, memSearchRedacted)
	}
	// live Result untouched
	if !strings.Contains(string(res.Messages[1].ToolCalls[0].Function.Arguments), created) {
		t.Error("live agent.Result mutated by persistence mapping")
	}
}

// TestResultMessagesReplacesGoalWithRawUserMessage pins that host-supplied
// ContextItems (embedded into the orchestrator goal) never persist: the first
// user message is stored as the raw Turn.Message, not the composed goal.
func TestResultMessagesReplacesGoalWithRawUserMessage(t *testing.T) {
	const contextValue = "CONTEXT FILE CONTENTS"
	goal := "question" + contextDelimiter + `[{"description":"file","value":"` + contextValue + `"}]`
	res := agent.Result{
		Answer: "ok",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: goal},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultMessages("question", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if msgs[0].Content != "question" {
		t.Fatalf("persisted user message = %q, want raw message", msgs[0].Content)
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), contextValue) {
		t.Fatalf("turn context leaked into persisted history: %s", raw)
	}
}

func TestDefaultSessionDBPathRejectsSymlinkIntoWorkspace(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink data directory: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", link)

	if _, err := defaultSessionDBPath(root); err == nil {
		t.Fatal("want symlinked session database inside workspace to be rejected")
	}
}

func TestConcurrentCloseWaitsForResourceShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runtime := &Runtime{
		active:        make(map[string]*activeRun),
		activeThreads: make(map[string]*activeRun),
		closeOwned: func() error {
			close(started)
			<-release
			return nil
		},
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- runtime.Close() }()
	<-started

	secondDone := make(chan error, 1)
	go func() { secondDone <- runtime.Close() }()
	select {
	case err := <-secondDone:
		t.Fatalf("second Close returned before resources closed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
