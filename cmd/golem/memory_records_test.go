package main

import (
	"bytes"
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

func newTestReplWithRecords(t *testing.T) (*replSession, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	db, err := openMemoryDB(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rs, err := memory.NewMemoryRecordStore(ctx, db)
	if err != nil {
		t.Fatalf("record store: %v", err)
	}
	if err := chmodDBFiles(dbPath); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	sess := &replSession{records: rs, memoryDBPath: dbPath, workspaceID: "workspace:aaa"}
	sess.session = &session{id: "workspace:aaa"} // handlers only read .id
	return sess, dbPath
}

func TestRecordsCommands(t *testing.T) {
	ctx := context.Background()
	sess, _ := newTestReplWithRecords(t)
	rec, err := sess.records.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindWorking, Content: "note body",
		WorkspaceID: sess.workspaceID, SessionID: sess.session.id,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	handleRecords(ctx, &out, sess, []string{"/records"})
	if !strings.Contains(out.String(), rec.ID) || !strings.Contains(out.String(), "note body") {
		t.Errorf("list output: %q", out.String())
	}

	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--promote", rec.ID, "semantic"})
	if !strings.Contains(out.String(), "promoted "+rec.ID+" to semantic") {
		t.Errorf("promote output: %q", out.String())
	}

	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--forget", rec.ID})
	if !strings.Contains(out.String(), "forgot record "+rec.ID) {
		t.Errorf("forget output: %q", out.String())
	}

	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records"})
	if !strings.Contains(out.String(), "no records") {
		t.Errorf("post-forget list: %q", out.String())
	}

	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--bogus"})
	if !strings.Contains(out.String(), "usage:") {
		t.Errorf("usage output: %q", out.String())
	}
}

func TestRecordsCommandsDisabled(t *testing.T) {
	var out bytes.Buffer
	sess := &replSession{} // records nil
	handleRecords(context.Background(), &out, sess, []string{"/records"})
	if !strings.Contains(out.String(), "agent memory disabled") {
		t.Errorf("disabled output: %q", out.String())
	}
}

func TestRecordsSlashReSecuresSidecars(t *testing.T) {
	ctx := context.Background()
	sess, dbPath := newTestReplWithRecords(t)
	rec, err := sess.records.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindWorking, Content: "x",
		WorkspaceID: sess.workspaceID, SessionID: sess.session.id,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	loosen := func() {
		for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
			if _, err := os.Stat(p); err == nil {
				_ = os.Chmod(p, 0o644)
			}
		}
	}
	assertTight := func(when string) {
		t.Helper()
		for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
			info, err := os.Stat(p)
			if err != nil {
				continue // sidecar may not exist; nothing to leak
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s: %s mode = %v, want 0600", when, p, info.Mode().Perm())
			}
		}
	}
	var out bytes.Buffer
	loosen()
	handleRecords(ctx, &out, sess, []string{"/records", "--promote", rec.ID, "semantic"})
	assertTight("after --promote")
	loosen()
	handleRecords(ctx, &out, sess, []string{"/records", "--forget", rec.ID})
	assertTight("after --forget")
}

func TestSidecarWrappedToolsAreNotPlanningTools(t *testing.T) {
	// sidecarSecuringTool's interface embedding would silently drop Plan;
	// pin that the tools golem wraps never implement it.
	for _, tl := range []agent.Tool{agenttools.AgentMemoryCreate{}, agenttools.AgentMemoryPromote{}} {
		if _, ok := tl.(agent.PlanningTool); ok {
			t.Fatalf("%s implements PlanningTool; sidecarSecuringTool would drop Plan", tl.Spec().Name)
		}
	}
}

func TestRecordsCommandsCrossSessionDurable(t *testing.T) {
	ctx := context.Background()
	sess, _ := newTestReplWithRecords(t)
	// Durable record: workspace-scoped, no session (legal for semantic/episodic;
	// visible from any session in the workspace).
	rec, err := sess.records.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindSemantic, Content: "durable", WorkspaceID: sess.workspaceID,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	sess.session = &session{id: "different-session"}
	var out bytes.Buffer
	handleRecords(ctx, &out, sess, []string{"/records"})
	if !strings.Contains(out.String(), rec.ID) {
		t.Errorf("durable record not listed cross-session: %q", out.String())
	}
	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--promote", rec.ID, "episodic"})
	if !strings.Contains(out.String(), "promoted "+rec.ID+" to episodic") {
		t.Errorf("cross-session promote: %q", out.String())
	}
	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--forget", rec.ID})
	if !strings.Contains(out.String(), "forgot record "+rec.ID) {
		t.Errorf("cross-session forget: %q", out.String())
	}

	// Sessions off: recordAccess yields SessionID "" and mutations still work.
	rec2, err := sess.records.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindSemantic, Content: "durable2", WorkspaceID: sess.workspaceID,
	})
	if err != nil {
		t.Fatalf("seed2: %v", err)
	}
	sess.session = nil
	out.Reset()
	handleRecords(ctx, &out, sess, []string{"/records", "--forget", rec2.ID})
	if !strings.Contains(out.String(), "forgot record "+rec2.ID) {
		t.Errorf("no-session forget: %q", out.String())
	}
}

func TestSidecarSecuringToolReChmods(t *testing.T) {
	ctx := context.Background()
	sess, dbPath := newTestReplWithRecords(t)
	inner := agenttools.AgentMemoryCreate{
		S: sess.records, WorkspaceID: sess.workspaceID,
		SessionID: func() string { return sess.session.id },
	}
	tool := sidecarSecuringTool{Tool: inner, dbPath: dbPath}
	if tool.Spec().Name != agenttools.AgentMemoryCreateToolName {
		t.Errorf("decorator must delegate Spec: %q", tool.Spec().Name)
	}
	if tool.Effect().Class != agent.Write {
		t.Errorf("decorator must delegate Effect")
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			_ = os.Chmod(p, 0o644)
		}
	}
	res, err := tool.Invoke(ctx, json.RawMessage(`{"content":"hello"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v res=%+v", err, res)
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600 after tool write", p, info.Mode().Perm())
		}
	}
	// re-chmod must also run when the inner tool errors
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			_ = os.Chmod(p, 0o644)
		}
	}
	if res, _ := tool.Invoke(ctx, json.RawMessage(`{"content":""}`)); !res.IsError {
		t.Fatal("expected IsError for empty content")
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600 after erroring tool write", p, info.Mode().Perm())
		}
	}
}

func TestAgentMemoryNotice(t *testing.T) {
	cases := []struct {
		enabled, sessionUp bool
		want               string
	}{
		{false, true, ""},
		{true, true, "agent memory: enabled"},
		{true, false, "agent memory: enabled (session unavailable; working notes disabled)"},
	}
	for _, c := range cases {
		if got := agentMemoryNotice(c.enabled, c.sessionUp); got != c.want {
			t.Errorf("agentMemoryNotice(%v, %v) = %q, want %q", c.enabled, c.sessionUp, got, c.want)
		}
	}
}
