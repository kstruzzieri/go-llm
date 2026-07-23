package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/memory"
)

// listToolNames returns the set of tool names the server advertises.
func listToolNames(t *testing.T, session *gomcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := session.ListTools(context.Background(), &gomcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

func TestAgentMemoryToolsAbsentByDefault(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer env.cleanup()

	names := listToolNames(t, env.session)
	for _, n := range []string{"agent_memory_search", "agent_memory_create", "agent_memory_promote"} {
		if names[n] {
			t.Errorf("tool %q registered without WithAgentMemoryPath; want absent", n)
		}
	}
}

func TestWithAgentMemoryPathOpenFailureIsFatal(t *testing.T) {
	// Parent "dir" is a file -> OpenRecordStore must fail -> NewServer errors.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := NewServer(context.Background(),
		WithRAGDisabled(),
		WithAgentMemoryPath(filepath.Join(blocker, "memories.db")),
	)
	if err == nil {
		t.Fatal("NewServer() error = nil, want fatal error for unopenable agent-memory path")
	}
}

// callTool invokes an MCP tool and returns (textPayload, isError).
func callTool(t *testing.T, session *gomcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%s) transport error = %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s) returned no content", name)
	}
	tc, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s) content[0] = %T, want *TextContent", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

// newAgentMemoryEnv is a testEnv with agent memory enabled on a temp DB.
func newAgentMemoryEnv(t *testing.T) testEnv {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	return newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}), WithAgentMemoryPath(dbPath))
}

type searchPayload struct {
	Records []map[string]any `json:"records"`
	Count   int              `json:"count"`
}

func decodeSearch(t *testing.T, text string) searchPayload {
	t.Helper()
	var p searchPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("decode search payload: %v\npayload: %s", err, text)
	}
	return p
}

func TestAgentMemorySearchRoundTripAndShape(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()

	// Seed directly through the store (create tool lands in a later task).
	store := env.server.agentMemorySnapshot().Store()
	rec, err := store.Create(context.Background(), memory.CreateRecordParams{
		Kind:        memory.KindSemantic,
		Content:     "line one\nline two\x1b[31m",
		Namespace:   "firn",
		WorkspaceID: "ws1",
		Provenance:  memory.Provenance{SourceKind: "mcp_client", SourceID: "conv-1"},
		Metadata:    json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	text, isErr := callTool(t, env.session, "agent_memory_search", map[string]any{
		"query":        "line",
		"workspace_id": "ws1",
	})
	if isErr {
		t.Fatalf("search returned IsError: %s", text)
	}
	p := decodeSearch(t, text)
	if p.Count != 1 || len(p.Records) != 1 {
		t.Fatalf("count = %d, records = %d, want 1/1", p.Count, len(p.Records))
	}
	r := p.Records[0]
	if r["id"] != rec.ID {
		t.Errorf("id = %v, want %s", r["id"], rec.ID)
	}
	// Content present in search views, flattened: one line, no control chars.
	content, _ := r["content"].(string)
	if strings.ContainsAny(content, "\n\x1b") {
		t.Errorf("content not flattened: %q", content)
	}
	if !strings.Contains(content, "line one line two") {
		t.Errorf("content = %q, want flattened joined text", content)
	}
	// Provenance object present with source fields.
	prov, ok := r["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance missing or wrong type: %v", r["provenance"])
	}
	if prov["source_kind"] != "mcp_client" || prov["source_id"] != "conv-1" {
		t.Errorf("provenance = %v, want source_kind/source_id set", prov)
	}
	// Metadata round-trips.
	md, ok := r["metadata"].(map[string]any)
	if !ok || md["k"] != "v" {
		t.Errorf("metadata = %v, want {\"k\":\"v\"}", r["metadata"])
	}
	// Timestamps RFC3339.
	created, _ := r["created_at"].(string)
	if _, err := time.Parse(time.RFC3339Nano, created); err != nil {
		t.Errorf("created_at %q not RFC3339Nano: %v", created, err)
	}
	if _, hasUpdated := r["updated_at"]; !hasUpdated {
		t.Error("updated_at missing; must always be present")
	}
	if _, hasExpires := r["expires_at"]; hasExpires {
		t.Error("expires_at present on never-expiring record; must be omitted when zero")
	}
}

func TestAgentMemorySearchScopingAndFilters(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()
	store := env.server.agentMemorySnapshot().Store()
	ctx := context.Background()

	mustCreate := func(p memory.CreateRecordParams) memory.MemoryRecord {
		t.Helper()
		rec, err := store.Create(ctx, p)
		if err != nil {
			t.Fatalf("seed Create(%+v): %v", p, err)
		}
		return rec
	}
	mustCreate(memory.CreateRecordParams{Kind: memory.KindSemantic, Content: "workspace A fact", WorkspaceID: "wsA"})
	mustCreate(memory.CreateRecordParams{Kind: memory.KindSemantic, Content: "workspace B fact", WorkspaceID: "wsB"})
	mustCreate(memory.CreateRecordParams{Kind: memory.KindWorking, Content: "session note", WorkspaceID: "wsA", SessionID: "sess1"})
	mustCreate(memory.CreateRecordParams{Kind: memory.KindEpisodic, Content: "flux event", WorkspaceID: "wsA", Namespace: "flux"})

	// Workspace isolation: wsA search must not see wsB's record.
	text, _ := callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA", "session_id": "sess1"})
	p := decodeSearch(t, text)
	for _, r := range p.Records {
		if c, _ := r["content"].(string); strings.Contains(c, "workspace B") {
			t.Errorf("wsA search leaked wsB record: %v", r)
		}
	}
	if p.Count != 3 {
		t.Errorf("wsA+sess1 count = %d, want 3 (2 durable wsA + 1 session note)", p.Count)
	}

	// Session isolation: without session_id the working note is invisible.
	text, _ = callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA"})
	p = decodeSearch(t, text)
	for _, r := range p.Records {
		if r["kind"] == "working" {
			t.Errorf("working note visible without session_id: %v", r)
		}
	}

	// Kind filter.
	text, _ = callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA", "kind": "episodic"})
	p = decodeSearch(t, text)
	if p.Count != 1 || p.Records[0]["kind"] != "episodic" {
		t.Errorf("kind filter: got %v", p.Records)
	}

	// Namespace filter.
	text, _ = callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA", "namespace": "flux"})
	p = decodeSearch(t, text)
	if p.Count != 1 {
		t.Errorf("namespace filter count = %d, want 1", p.Count)
	}

	// Invalid kind is a validation error, not a silent widen.
	text, isErr := callTool(t, env.session, "agent_memory_search", map[string]any{"kind": "bogus"})
	if !isErr || !strings.Contains(text, "validation") {
		t.Errorf("invalid kind: isErr=%v text=%q, want validation error", isErr, text)
	}
}

func TestAgentMemorySearchExpiry(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()
	store := env.server.agentMemorySnapshot().Store()
	ctx := context.Background()

	if _, err := store.Create(ctx, memory.CreateRecordParams{
		Kind: memory.KindWorking, Content: "already expired note",
		WorkspaceID: "wsA", SessionID: "sess1",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Excluded by default.
	text, _ := callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA", "session_id": "sess1"})
	if p := decodeSearch(t, text); p.Count != 0 {
		t.Errorf("expired record returned by default: %v", p.Records)
	}
	// Included on request.
	text, _ = callTool(t, env.session, "agent_memory_search", map[string]any{"workspace_id": "wsA", "session_id": "sess1", "include_expired": true})
	if p := decodeSearch(t, text); p.Count != 1 {
		t.Errorf("include_expired count = %d, want 1", p.Count)
	}
}

type recordPayload struct {
	Record map[string]any `json:"record"`
}

func decodeRecord(t *testing.T, text string) map[string]any {
	t.Helper()
	var p recordPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("decode record payload: %v\npayload: %s", err, text)
	}
	if p.Record == nil {
		t.Fatalf("payload has no record: %s", text)
	}
	return p.Record
}

func TestAgentMemoryCreateHappyPaths(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()

	t.Run("working", func(t *testing.T) {
		text, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
			"content":      "working note content",
			"workspace_id": "ws1",
			"session_id":   "sess1",
		})
		if isErr {
			t.Fatalf("create returned IsError: %s", text)
		}
		r := decodeRecord(t, text)
		if r["kind"] != "working" {
			t.Errorf("kind = %v, want working (default)", r["kind"])
		}
		// Content-light write response: no content echo.
		if _, has := r["content"]; has {
			t.Error("create response echoes content; recordRef must omit it")
		}
		// Working TTL ~7d.
		expStr, _ := r["expires_at"].(string)
		exp, err := time.Parse(time.RFC3339Nano, expStr)
		if err != nil {
			t.Fatalf("expires_at %q: %v", expStr, err)
		}
		want := time.Now().Add(7 * 24 * time.Hour)
		if d := exp.Sub(want); d < -time.Minute || d > time.Minute {
			t.Errorf("expires_at = %v, want ~%v", exp, want)
		}
		// Default provenance source_kind.
		prov, _ := r["provenance"].(map[string]any)
		if prov["source_kind"] != "mcp_client" {
			t.Errorf("source_kind = %v, want mcp_client default", prov["source_kind"])
		}
	})

	t.Run("durable workspace", func(t *testing.T) {
		text, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
			"content":      "durable fact",
			"kind":         "semantic",
			"workspace_id": "ws1",
			"namespace":    "firn",
			"metadata":     map[string]any{"k": "v"},
			"source_kind":  "firn",
			"source_id":    "doc-9",
			"source_start": 10,
			"source_end":   20,
			"source_hash":  "abc123",
		})
		if isErr {
			t.Fatalf("create returned IsError: %s", text)
		}
		r := decodeRecord(t, text)
		if r["kind"] != "semantic" {
			t.Errorf("kind = %v, want semantic", r["kind"])
		}
		if _, has := r["expires_at"]; has {
			t.Error("durable record has expires_at; want omitted (never expires)")
		}
		prov, _ := r["provenance"].(map[string]any)
		if prov["source_kind"] != "firn" || prov["source_id"] != "doc-9" || prov["source_hash"] != "abc123" {
			t.Errorf("provenance = %v, want caller-supplied fields", prov)
		}
		md, _ := r["metadata"].(map[string]any)
		if md["k"] != "v" {
			t.Errorf("metadata = %v, want round-tripped", r["metadata"])
		}
	})

	t.Run("durable global with explicit scope", func(t *testing.T) {
		text, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
			"content": "global fact",
			"kind":    "semantic",
			"scope":   "global",
		})
		if isErr {
			t.Fatalf("create returned IsError: %s", text)
		}
		r := decodeRecord(t, text)
		if _, has := r["workspace_id"]; has {
			t.Errorf("global record has workspace_id: %v", r["workspace_id"])
		}
	})
}

func TestAgentMemoryCreateErrors(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()

	cases := []struct {
		name     string
		args     map[string]any
		category string
	}{
		{"empty content", map[string]any{"content": "   "}, "validation"},
		{"oversize content", map[string]any{"content": strings.Repeat("a", 4097), "workspace_id": "w", "session_id": "s"}, "validation"},
		{"bad kind", map[string]any{"content": "x", "kind": "bogus"}, "validation"},
		{"working without session", map[string]any{"content": "x", "workspace_id": "w"}, "validation"},
		{"working session without workspace", map[string]any{"content": "x", "session_id": "s"}, "validation"},
		{"durable with session_id", map[string]any{"content": "x", "kind": "semantic", "workspace_id": "w", "session_id": "s"}, "validation"},
		{"durable global without confirmation", map[string]any{"content": "x", "kind": "semantic"}, "validation"},
		{"scope global with workspace_id", map[string]any{"content": "x", "kind": "semantic", "scope": "global", "workspace_id": "w"}, "validation"},
		{"scope workspace without workspace_id", map[string]any{"content": "x", "kind": "semantic", "scope": "workspace"}, "validation"},
		{"bad scope", map[string]any{"content": "x", "kind": "semantic", "scope": "bogus", "workspace_id": "w"}, "validation"},
		{"bad metadata", map[string]any{"content": "x", "kind": "semantic", "workspace_id": "w", "metadata": "not-an-object"}, "validation"},
		{"null metadata", map[string]any{"content": "x", "kind": "semantic", "workspace_id": "w", "metadata": nil}, "validation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := callTool(t, env.session, "agent_memory_create", tc.args)
			if !isErr {
				t.Fatalf("want IsError, got success: %s", text)
			}
			if !strings.HasPrefix(text, tc.category+":") {
				t.Errorf("error = %q, want category %q", text, tc.category)
			}
		})
	}
}

func TestAgentMemoryToolsRegisteredWithPath(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()

	names := listToolNames(t, env.session)
	for _, n := range []string{"agent_memory_search", "agent_memory_create", "agent_memory_promote"} {
		if !names[n] {
			t.Errorf("tool %q not registered with WithAgentMemoryPath; want present", n)
		}
	}
}

func TestAgentMemoryPromote(t *testing.T) {
	env := newAgentMemoryEnv(t)
	defer env.cleanup()

	create := func(t *testing.T) string {
		t.Helper()
		text, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
			"content":      "promotable note",
			"workspace_id": "ws1",
			"session_id":   "sess1",
		})
		if isErr {
			t.Fatalf("seed create: %s", text)
		}
		id, _ := decodeRecord(t, text)["id"].(string)
		if id == "" {
			t.Fatal("seed create returned no id")
		}
		return id
	}

	t.Run("working to semantic", func(t *testing.T) {
		id := create(t)
		text, isErr := callTool(t, env.session, "agent_memory_promote", map[string]any{
			"id":           id,
			"kind":         "semantic",
			"workspace_id": "ws1",
			"session_id":   "sess1",
		})
		if isErr {
			t.Fatalf("promote returned IsError: %s", text)
		}
		r := decodeRecord(t, text)
		if r["kind"] != "semantic" {
			t.Errorf("kind = %v, want semantic", r["kind"])
		}
		if _, has := r["session_id"]; has {
			t.Errorf("promoted record kept session_id: %v", r["session_id"])
		}
		if _, has := r["expires_at"]; has {
			t.Errorf("promoted record kept expires_at: %v", r["expires_at"])
		}
		if _, has := r["content"]; has {
			t.Error("promote response echoes content; recordRef must omit it")
		}
	})

	t.Run("errors", func(t *testing.T) {
		id := create(t)
		cases := []struct {
			name     string
			args     map[string]any
			category string
		}{
			{"empty id", map[string]any{"id": "  ", "kind": "semantic"}, "validation"},
			{"bad kind", map[string]any{"id": id, "kind": "working"}, "validation"},
			{"unknown id", map[string]any{"id": "rec_does_not_exist", "kind": "semantic"}, "not_found"},
			{"out of scope", map[string]any{"id": id, "kind": "semantic", "workspace_id": "OTHER", "session_id": "OTHER"}, "not_found"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				text, isErr := callTool(t, env.session, "agent_memory_promote", tc.args)
				if !isErr {
					t.Fatalf("want IsError, got success: %s", text)
				}
				if !strings.HasPrefix(text, tc.category+":") {
					t.Errorf("error = %q, want category %q", text, tc.category)
				}
			})
		}
	})
}

// newAgentMemoryEnvWithPath is newAgentMemoryEnv but also returns the on-disk
// DB path, so sidecar-permission tests can loosen a sidecar and assert the
// write handlers re-secure it.
func newAgentMemoryEnvWithPath(t *testing.T) (testEnv, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}), WithAgentMemoryPath(dbPath))
	return env, dbPath
}

func TestAgentMemoryCreateResecuresSidecars(t *testing.T) {
	env, dbPath := newAgentMemoryEnvWithPath(t)
	defer env.cleanup()

	// First create materializes the -wal sidecar.
	if _, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
		"content": "first", "kind": "semantic", "workspace_id": "ws1",
	}); isErr {
		t.Fatal("first create returned IsError")
	}
	wal := dbPath + "-wal"
	if _, err := os.Stat(wal); err != nil {
		t.Fatalf("no -wal sidecar after committed write (%v); WAL not applied", err)
	}
	if err := os.Chmod(wal, 0o644); err != nil {
		t.Fatalf("chmod loosen: %v", err)
	}
	// A second create must re-secure the sidecar via the handler's defer.
	if _, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
		"content": "second", "kind": "semantic", "workspace_id": "ws1",
	}); isErr {
		t.Fatal("second create returned IsError")
	}
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wal perm after create = %o, want 0600 (handler must re-secure sidecars)", perm)
	}
}

func TestAgentMemoryPromoteResecuresSidecars(t *testing.T) {
	env, dbPath := newAgentMemoryEnvWithPath(t)
	defer env.cleanup()

	// Seed a working record to promote.
	text, isErr := callTool(t, env.session, "agent_memory_create", map[string]any{
		"content": "promotable", "workspace_id": "ws1", "session_id": "sess1",
	})
	if isErr {
		t.Fatalf("seed create: %s", text)
	}
	id, _ := decodeRecord(t, text)["id"].(string)
	if id == "" {
		t.Fatal("seed create returned no id")
	}
	wal := dbPath + "-wal"
	if _, err := os.Stat(wal); err != nil {
		t.Fatalf("no -wal sidecar after committed write (%v); WAL not applied", err)
	}
	if err := os.Chmod(wal, 0o644); err != nil {
		t.Fatalf("chmod loosen: %v", err)
	}
	// Promote must re-secure the sidecar via the handler's defer.
	if _, isErr := callTool(t, env.session, "agent_memory_promote", map[string]any{
		"id": id, "kind": "semantic", "workspace_id": "ws1", "session_id": "sess1",
	}); isErr {
		t.Fatal("promote returned IsError")
	}
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wal perm after promote = %o, want 0600 (handler must re-secure sidecars)", perm)
	}
}
