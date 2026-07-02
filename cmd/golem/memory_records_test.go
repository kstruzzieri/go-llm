package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
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

func TestOpenMemoryRuntimeDualStores(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	rt := openMemoryRuntime(ctx, getenv, "/some/workspace/root", true, true)
	if len(rt.warns) != 0 {
		t.Fatalf("warns: %v", rt.warns)
	}
	if rt.user == nil || rt.records == nil || rt.db == nil {
		t.Fatalf("stores not constructed: %+v", rt)
	}
	t.Cleanup(func() { _ = rt.db.Close() })
	info, err := os.Stat(rt.dbPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("db file mode: %v err=%v", info.Mode().Perm(), err)
	}
	if _, err := rt.user.Add(ctx, memory.AddParams{Text: "u", Scope: memory.ScopeGlobal}); err != nil {
		t.Errorf("user store: %v", err)
	}
	if _, err := rt.records.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindWorking, Content: "r", WorkspaceID: "w", SessionID: "s",
	}); err != nil {
		t.Errorf("record store: %v", err)
	}
}

func TestOpenMemoryRuntimeRecordsOnly(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	rt := openMemoryRuntime(context.Background(), getenv, "/some/workspace/root", false, true)
	if len(rt.warns) != 0 {
		t.Fatalf("warns: %v", rt.warns)
	}
	if rt.user != nil {
		t.Error("user store constructed though not requested")
	}
	if rt.records == nil || rt.db == nil {
		t.Fatalf("records store missing: %+v", rt)
	}
	_ = rt.db.Close()
}

func TestOpenMemoryRuntimeFailOpen(t *testing.T) {
	getenv := func(string) string { return "" } // no HOME, no XDG => path resolution fails
	rt := openMemoryRuntime(context.Background(), getenv, "/some/workspace/root", true, true)
	if rt.user != nil || rt.records != nil || rt.db != nil {
		t.Fatalf("expected everything disabled: %+v", rt)
	}
	joined := strings.Join(rt.warns, "\n")
	if !strings.Contains(joined, "memory disabled:") || !strings.Contains(joined, "agent memory disabled:") {
		t.Errorf("both features must warn: %v", rt.warns)
	}
}

func TestOpenMemoryRuntimeDBOpenFailure(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	// A directory at the db path makes prepareDBFile/open fail after path
	// resolution succeeds, exercising the openMemoryDB-failure branch.
	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "golem", "memories.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt := openMemoryRuntime(context.Background(), getenv, "/some/workspace/root", true, true)
	if rt.user != nil || rt.records != nil || rt.db != nil {
		t.Fatalf("expected everything disabled: %+v", rt)
	}
	joined := strings.Join(rt.warns, "\n")
	if !strings.Contains(joined, "memory disabled:") || !strings.Contains(joined, "agent memory disabled:") {
		t.Errorf("both features must warn: %v", rt.warns)
	}
}

func TestOpenMemoryRuntimeNothingRequested(t *testing.T) {
	rt := openMemoryRuntime(context.Background(), func(string) string { return "" }, "/r", false, false)
	if rt.db != nil || rt.user != nil || rt.records != nil || len(rt.warns) != 0 {
		t.Fatalf("expected zero value: %+v", rt)
	}
}

func TestOpenMemoryRuntimeUserOnlyNoAgentWarn(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	rt := openMemoryRuntime(context.Background(), getenv, "/some/workspace/root", true, false)
	if len(rt.warns) != 0 || rt.user == nil || rt.records != nil {
		t.Fatalf("user-only open wrong: %+v", rt)
	}
	_ = rt.db.Close()
}
