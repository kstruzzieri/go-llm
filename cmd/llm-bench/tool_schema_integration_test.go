//go:build llmbench_integration

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPSnapshotAgainstGoLLMMCPStdio builds and runs cmd/go-llm-mcp
// then snapshots its tools/list over stdio. Verifies the snapshot is
// non-empty and that each returned tool has a name + inputSchema.
//
// Gated behind llmbench_integration because it shells out to `go build`
// and runs an actual subprocess — slow and not appropriate for the
// default unit pass.
func TestMCPSnapshotAgainstGoLLMMCPStdio(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)
	binPath := filepath.Join(t.TempDir(), "go-llm-mcp")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/go-llm-mcp")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build go-llm-mcp: %v\n%s", err, out)
	}

	src, err := newMCPToolSchemaSourceStdio(binPath + " --no-rag")
	if err != nil {
		t.Fatalf("newMCPToolSchemaSourceStdio: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(tools) == 0 {
		t.Fatalf("Snapshot returned 0 tools — server may have started with no tools registered")
	}

	for i, raw := range tools {
		var fields struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("tool[%d] unmarshal: %v", i, err)
		}
		if strings.TrimSpace(fields.Name) == "" {
			t.Fatalf("tool[%d] missing name: %s", i, string(raw))
		}
		if len(fields.InputSchema) == 0 {
			t.Fatalf("tool[%d] %q missing inputSchema", i, fields.Name)
		}
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}
