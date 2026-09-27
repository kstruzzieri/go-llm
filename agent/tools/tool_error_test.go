package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// TestBuiltinFileToolErrorsDoNotExposeRoot drives the four read-only file
// tools through real filesystem failures: read_file and list against missing
// paths, glob and search after the workspace root itself is removed (a
// post-check walk failure that would otherwise disclose the canonical root).
// Every result must stay IsError with a fixed path-free message.
func TestBuiltinFileToolErrorsDoNotExposeRoot(t *testing.T) {
	const marker = "tools-error-secret-marker"
	root := filepath.Join(t.TempDir(), marker)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	canonical := ws.root

	check := func(t *testing.T, tool agent.Tool, args string, want string) {
		t.Helper()
		res, err := tool.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("Invoke returned a Go error (should be IsError ToolResult): %v", err)
		}
		if !res.IsError {
			t.Fatalf("result = %#v, want IsError", res)
		}
		if res.Content != want {
			t.Fatalf("Content = %q, want stable message %q", res.Content, want)
		}
		if strings.Contains(res.Content, marker) || strings.Contains(res.Content, canonical) {
			t.Fatalf("Content leaks the workspace root: %q", res.Content)
		}
	}

	check(t, NewReadFile(ws), `{"path":"missing.txt"}`, "path not found")
	check(t, NewList(ws), `{"path":"missing-dir"}`, "path not found")

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	check(t, NewGlob(ws), `{"pattern":"**"}`, "path not found")
	check(t, NewSearch(ws), `{"pattern":"anything"}`, "path not found")
}

func TestToolErrorClassifierAllowlist(t *testing.T) {
	const leaked = "/secret/canonical/root"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"canceled context", context.Canceled, "filesystem operation canceled"},
		{"deadline", context.DeadlineExceeded, "filesystem operation canceled"},
		{"scope denial", errScopeDenied, "path denied by workspace policy"},
		{"not found", &fs.PathError{Op: "lstat", Path: leaked + "/x", Err: fs.ErrNotExist}, "path not found"},
		{"parent missing", errParentMissing, "path not found"},
		{"permission", &fs.PathError{Op: "open", Path: leaked + "/x", Err: fs.ErrPermission}, "path is not accessible"},
		{"nul byte", errNUL, "path is outside the workspace"},
		{"absolute input", errAbsPath, "path is outside the workspace"},
		{"escape", errEscape, "path is outside the workspace"},
		{"symlink", errSymlink, "symlinks are not followed"},
		{"not regular", errNotRegular, "path has the wrong type"},
		{"not dir", errNotDir, "path has the wrong type"},
		{"identity race", errFileChanged, "path changed during access"},
		{"unknown", errors.New("mount gone at " + leaked), "filesystem operation failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolErrMessage(tc.err)
			if got != tc.want {
				t.Fatalf("toolErrMessage(%v) = %q, want %q", tc.err, got, tc.want)
			}
			if strings.Contains(got, leaked) {
				t.Fatalf("classifier leaked the cause path: %q", got)
			}
			if tc.name == "unknown" && got == tc.err.Error() {
				t.Fatalf("default must be generic, never err.Error(): %q", got)
			}
		})
	}
}

func TestScopedRequestDenialsTypedIdentity(t *testing.T) {
	t.Run("typed identity", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
			want int64
			text string
		}{
			{"typed", errScopeDenied, 1, "path denied by workspace policy"},
			{"wrapped", fmt.Errorf("private host path: %w", errScopeDenied), 1, "path denied by workspace policy"},
			{"guard", scopeDeniedError{cause: errors.New("private guard detail")}, 1, "path denied by workspace policy"},
			{"same text", errors.New("path denied by workspace policy"), 0, "filesystem operation failed"},
			{"missing", os.ErrNotExist, 0, "path not found"},
			{"canceled", context.Canceled, 0, "filesystem operation canceled"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ws := &Workspace{scope: new(scopeCounters)}
				out := ws.toolErrorResult(tc.err)
				if !out.IsError || out.Content != tc.text || ws.scope.requests.Load() != tc.want {
					t.Fatalf("result=%+v requests=%d, want %q / %d", out, ws.scope.requests.Load(), tc.text, tc.want)
				}
				if ws.scope.evaluations.Load() != 0 {
					t.Fatal("rendering added a policy evaluation")
				}
				_ = toolErrMessage(tc.err)
				if ws.scope.requests.Load() != tc.want {
					t.Fatal("pure formatting added a request")
				}
				unscoped := &Workspace{}
				if got := unscoped.toolErrorResult(tc.err); got.Content != out.Content || got.IsError != out.IsError {
					t.Fatalf("unscoped sanitization changed: %+v", got)
				}
			})
		}
	})
}
