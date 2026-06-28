package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
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

func TestMemorySystemFragment(t *testing.T) {
	on := memorySystemFragment(true)
	if !strings.Contains(on, "memory_search") || !strings.Contains(on, "not higher-priority instructions") {
		t.Errorf("enabled fragment missing framing: %q", on)
	}
	if memorySystemFragment(false) != "" {
		t.Error("disabled fragment should be empty")
	}
}

func TestOpenMemoryStoreHardening(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	store, db, err := openMemoryStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if store == nil {
		t.Fatal("nil store")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := store.Add(context.Background(), memory.AddParams{Text: "x", Scope: memory.ScopeGlobal}); err != nil {
		t.Errorf("add after open: %v", err)
	}
}

func newTestReplWithMemory(t *testing.T) (*replSession, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	store, db, err := openMemoryStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &replSession{memory: store, memoryDBPath: dbPath, workspaceID: "workspace:aaa"}, dbPath
}

func TestRememberForgetMemories(t *testing.T) {
	ctx := context.Background()
	sess, _ := newTestReplWithMemory(t)
	var buf bytes.Buffer

	handleRemember(ctx, &buf, sess, "/remember use table tests")
	handleRemember(ctx, &buf, sess, "/remember --global prefer small diffs")
	got, _ := sess.memory.List(ctx, memory.ListOptions{WorkspaceID: "workspace:aaa"})
	if len(got) != 2 {
		t.Fatalf("want 2 memories, got %d", len(got))
	}

	buf.Reset()
	handleMemories(ctx, &buf, sess, []string{"/memories"})
	if !strings.Contains(buf.String(), "use table tests") {
		t.Errorf("list missing entry: %q", buf.String())
	}

	var wsID string
	for _, m := range got {
		if m.Scope == memory.ScopeWorkspace {
			wsID = m.ID
		}
	}
	buf.Reset()
	handleMemories(ctx, &buf, sess, []string{"/memories", "--promote", wsID})
	if pm, _ := sess.memory.ResolveVisible(ctx, wsID, "workspace:other"); pm.Scope != memory.ScopeGlobal {
		t.Errorf("promote failed: %+v", pm)
	}

	buf.Reset()
	handleForget(ctx, &buf, sess, []string{"/forget", wsID})
	if _, err := sess.memory.ResolveVisible(ctx, wsID, "workspace:aaa"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("not forgotten: %v", err)
	}
}

func TestMemoryCommandsReSecureSidecarsAfterMutations(t *testing.T) {
	ctx := context.Background()
	sess, dbPath := newTestReplWithMemory(t)
	var buf bytes.Buffer

	loosen := func() []string {
		t.Helper()
		var paths []string
		for _, suffix := range []string{"-wal", "-shm"} {
			p := dbPath + suffix
			if _, err := os.Stat(p); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				t.Fatalf("stat %s: %v", p, err)
			}
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatalf("chmod %s: %v", p, err)
			}
			paths = append(paths, p)
		}
		if len(paths) == 0 {
			t.Skip("sqlite did not create WAL sidecars on this platform")
		}
		return paths
	}
	requireSecure := func(paths []string) {
		t.Helper()
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("%s mode = %v, want 0600", filepath.Base(p), got)
			}
		}
	}

	paths := loosen()
	handleRemember(ctx, &buf, sess, "/remember re-secure after write")
	requireSecure(paths)

	m, err := sess.memory.Add(ctx, memory.AddParams{Text: "scope change", Scope: memory.ScopeWorkspace, WorkspaceID: sess.workspaceID})
	if err != nil {
		t.Fatalf("seed promote memory: %v", err)
	}
	paths = loosen()
	handleMemories(ctx, &buf, sess, []string{"/memories", "--promote", m.ID})
	requireSecure(paths)

	m, err = sess.memory.Add(ctx, memory.AddParams{Text: "delete me", Scope: memory.ScopeWorkspace, WorkspaceID: sess.workspaceID})
	if err != nil {
		t.Fatalf("seed forget memory: %v", err)
	}
	paths = loosen()
	handleForget(ctx, &buf, sess, []string{"/forget", m.ID})
	requireSecure(paths)
}

func TestMemoryCommandsDisabled(t *testing.T) {
	var buf bytes.Buffer
	sess := &replSession{} // memory nil
	handleRemember(context.Background(), &buf, sess, "/remember x")
	handleForget(context.Background(), &buf, sess, []string{"/forget", "id"})
	handleMemories(context.Background(), &buf, sess, []string{"/memories"})
	if c := strings.Count(buf.String(), "memory disabled"); c != 3 {
		t.Errorf("want 3 'memory disabled', got %d: %q", c, buf.String())
	}
}
