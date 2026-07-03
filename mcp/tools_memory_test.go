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
