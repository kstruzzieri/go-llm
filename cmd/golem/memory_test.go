package main

import (
	"path/filepath"
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

func TestWorkspaceIDStable(t *testing.T) {
	a := workspaceID("/x/y")
	b := workspaceID("/x/y")
	c := workspaceID("/x/z")
	if a != b {
		t.Error("workspaceID not stable for same root")
	}
	if a == c {
		t.Error("workspaceID collision across roots")
	}
	if !strings.HasPrefix(a, "workspace:") {
		t.Errorf("prefix: %s", a)
	}
}

func TestMemoryDBPathOutsideWorkspace(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	p, err := memoryDBPathForWorkspace(getenv, "/some/workspace/root")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if filepath.Base(p) != "memories.db" {
		t.Errorf("base = %s, want memories.db", p)
	}
	inside := filepath.Join(home, ".local", "share", "golem")
	if _, err := memoryDBPathForWorkspace(getenv, inside); err == nil {
		t.Error("expected rejection when workspace contains the db path")
	}
}
