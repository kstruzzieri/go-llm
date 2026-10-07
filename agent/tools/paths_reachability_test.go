//go:build linux || darwin

package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reachFixture builds <root>/frontend/private.txt (MOVED_MARKER), an empty
// <root>/vault, and <root>/keep.txt (KEEP_MARKER). The guard denies vault and
// everything under it; onAllow runs inside the guard for allowed paths so a
// test can change the namespace after the decision.
func reachFixture(t *testing.T, onAllow func(rel string)) (*Workspace, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	for rel, body := range map[string]string{
		"frontend/private.txt": "MOVED_MARKER\n",
		"keep.txt":             "KEEP_MARKER\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "vault"), 0o700); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetScopeGuard(func(rel string, _ bool) error {
		if rel == "vault" || strings.HasPrefix(rel, "vault/") {
			return errors.New("denied by test policy")
		}
		if onAllow != nil {
			onAllow(rel)
		}
		return nil
	})
	return ws, ws.root
}

// moveIntoVault renames <root>/frontend to <root>/vault/frontend, a denied
// name, exactly once per returned closure.
func moveIntoVault(t *testing.T, root string) func() {
	t.Helper()
	done := false
	return func() {
		if done {
			return
		}
		done = true
		if err := os.Rename(filepath.Join(root, "frontend"), filepath.Join(root, "vault", "frontend")); err != nil {
			t.Fatal(err)
		}
	}
}

// R1: the directory is pinned, then moved under a denied name before the leaf
// is opened through it. beforeReadOpen fires before each component open; the
// second firing precedes the leaf.
func TestReachabilityPointReadMovedBeforeLeafOpen(t *testing.T) {
	var move func()
	ws, root := reachFixture(t, nil)
	move = moveIntoVault(t, root)
	calls := 0
	ws.beforeReadOpen = func() {
		calls++
		if calls == 2 {
			move()
		}
	}
	data, err := ws.readAll("frontend/private.txt")
	if !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("read after move = %q, %v; want errFileChanged and no bytes", data, err)
	}
	if calls != 2 {
		t.Fatalf("seam fired %d times, want 2", calls)
	}
}

// R2: the move happens inside the guard, after the leaf is already open.
// A symlink back to the moved directory must not make that guarded name valid.
func TestReachabilityPointReadMovedInsideGuard(t *testing.T) {
	var root string
	var move func()
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/private.txt" {
			move()
			if err := os.Symlink(filepath.Join(root, "vault", "frontend"), filepath.Join(root, "frontend")); err != nil {
				t.Fatal(err)
			}
		}
	})
	move = moveIntoVault(t, root)
	res, err := NewReadFile(ws).Invoke(t.Context(), json.RawMessage(`{"path":"frontend/private.txt"}`))
	if err != nil || !res.IsError || res.Content != "path changed during access" {
		t.Fatalf("read_file = %+v, %v; want the changed-path error", res, err)
	}
}

// Control: moved and restored before the check is reachable again; the
// check is about where the object is, not whether anything changed.
func TestReachabilityMovedAndRestoredPasses(t *testing.T) {
	var root string
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/private.txt" {
			moved := filepath.Join(root, "vault", "frontend")
			if err := os.Rename(filepath.Join(root, "frontend"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(moved, filepath.Join(root, "frontend")); err != nil {
				t.Fatal(err)
			}
		}
	})
	data, err := ws.readAll("frontend/private.txt")
	if err != nil || string(data) != "MOVED_MARKER\n" {
		t.Fatalf("restored read = %q, %v", data, err)
	}
}

// R6: the root is replaced after the per-call root open and before the
// guard returns.
func TestReachabilityRootReplacedInsideGuard(t *testing.T) {
	var root string
	replaced := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "frontend/private.txt" || replaced {
			return
		}
		replaced = true
		if err := os.Rename(root, root+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "frontend"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "frontend", "private.txt"), []byte("REPLACEMENT\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	data, err := ws.readAll("frontend/private.txt")
	if !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("read after root replacement = %q, %v; want ErrRootReplaced", data, err)
	}
}
