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
