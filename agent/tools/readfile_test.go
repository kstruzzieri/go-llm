package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func mustWorkspace(t *testing.T, root string) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws
}

func invoke(t *testing.T, tool agent.Tool, args any) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke returned a Go error (should be IsError ToolResult): %v", err)
	}
	return res
}

func TestReadFileHappyPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))

	res := invoke(t, rf, map[string]any{"path": "a.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "line2") {
		t.Fatalf("content missing: %q", res.Content)
	}
}

func TestReadFileLineRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))

	res := invoke(t, rf, map[string]any{"path": "a.txt", "start_line": 2, "end_line": 3})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if res.Content != "l2\nl3" && res.Content != "l2\nl3\n" {
		t.Fatalf("range wrong: %q", res.Content)
	}
	if strings.Contains(res.Content, "l1") || strings.Contains(res.Content, "l4") {
		t.Fatalf("range leaked extra lines: %q", res.Content)
	}
}

func TestReadFileInvalidLineRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("l1\nl2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))

	for _, args := range []map[string]any{
		{"path": "a.txt", "start_line": 0, "end_line": 1}, // end set, start unset/0
		{"path": "a.txt", "start_line": 3, "end_line": 2}, // end < start
		{"path": "a.txt", "start_line": 99},               // start beyond EOF
	} {
		res := invoke(t, rf, args)
		if !res.IsError {
			t.Fatalf("args %v should be IsError, got %q", args, res.Content)
		}
	}
}

func TestReadFileEscapeIsError(t *testing.T) {
	rf := NewReadFile(mustWorkspace(t, t.TempDir()))
	res := invoke(t, rf, map[string]any{"path": "../../etc/passwd"})
	if !res.IsError {
		t.Fatalf("escape must be IsError, got %q", res.Content)
	}
}

func TestReadFileBinaryRejected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bin"), []byte("ab\x00cd"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))
	res := invoke(t, rf, map[string]any{"path": "bin"})
	if !res.IsError {
		t.Fatalf("binary file must be IsError, got %q", res.Content)
	}
}

func TestReadFileOversizeTruncatesNotError(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", readFileMaxBytes+5000)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))
	res := invoke(t, rf, map[string]any{"path": "big.txt"})
	if res.IsError {
		t.Fatalf("oversize should be a successful partial, got IsError: %s", res.Content)
	}
	if !res.Truncated {
		t.Fatal("oversize should set Truncated")
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("oversize should carry an in-band truncation marker, got tail: %q", res.Content[len(res.Content)-80:])
	}
}

func TestReadFileSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rf := NewReadFile(mustWorkspace(t, root))
	res := invoke(t, rf, map[string]any{"path": "evil"})
	if !res.IsError {
		t.Fatalf("symlink must be IsError, got %q", res.Content)
	}
	if strings.Contains(res.Content, "TOPSECRET") {
		t.Fatal("symlink leaked out-of-root contents")
	}
}

func TestReadFileEndLineBeyondEOFClamps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("l1\nl2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))
	res := invoke(t, rf, map[string]any{"path": "a.txt", "start_line": 1, "end_line": 999})
	if res.IsError {
		t.Fatalf("end_line beyond EOF must clamp, not error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "l1") || !strings.Contains(res.Content, "l2") {
		t.Fatalf("clamped range should include all lines: %q", res.Content)
	}
}

func TestReadFileEmptyFileWithStartLine(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rf := NewReadFile(mustWorkspace(t, root))
	res := invoke(t, rf, map[string]any{"path": "empty.txt", "start_line": 1})
	if !res.IsError {
		t.Fatalf("empty file with start_line=1 should be IsError (start beyond EOF), got %q", res.Content)
	}
}

func TestReadFileEffect(t *testing.T) {
	rf := NewReadFile(mustWorkspace(t, t.TempDir()))
	e := rf.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("Effect = %+v, want Read/ApprovalNever", e)
	}
	if e.OutputCap <= readFileMaxBytes {
		t.Fatalf("OutputCap %d must exceed self-cap %d to avoid runtime re-truncation", e.OutputCap, readFileMaxBytes)
	}
}
