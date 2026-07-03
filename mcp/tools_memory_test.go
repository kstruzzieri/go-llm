package mcp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
