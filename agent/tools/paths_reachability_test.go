//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/kstruzzieri/go-llm/agent"
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

// The directory is pinned, then moved under a denied name before the leaf
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

// The move happens inside the guard, after the leaf is already open.
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

// The leaf itself is replaced by a symlink to the moved file; verification
// must not follow it back to the held object.
func TestReachabilityLeafSymlinkInsideGuard(t *testing.T) {
	var root string
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/private.txt" {
			moved := filepath.Join(root, "vault", "private.txt")
			if err := os.Rename(filepath.Join(root, "frontend", "private.txt"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(root, "frontend", "private.txt")); err != nil {
				t.Fatal(err)
			}
		}
	})
	data, err := ws.readAll("frontend/private.txt")
	if !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("leaf symlink read = %q, %v", data, err)
	}
}

// openFDCount counts this process's open descriptors by name. Names only: on
// Darwin os.ReadDir's per-entry stat can hit the descriptor used to read
// /dev/fd after it is closed (EBADF).
func openFDCount(t *testing.T) int {
	t.Helper()
	fdDir := "/dev/fd"
	if runtime.GOOS == "linux" {
		fdDir = "/proc/self/fd"
	}
	dir, err := os.Open(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// verifyReachable must release every directory it opens on the way down. GC
// stays off so a finalizer cannot close a leaked *os.File and hide the leak
// (same technique as TestMutationDescriptorLifetime).
func TestReachabilityReleasesDescriptors(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("a", "b", "c", "d", "f.txt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	read := func() {
		t.Helper()
		if _, err := ws.readAll(rel); err != nil {
			t.Fatal(err)
		}
	}
	read()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	before := openFDCount(t)
	for range 64 {
		read()
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("descriptors before=%d after=%d", before, after)
	}
}

// Control: moved and restored before the check is reachable again; the
// check is about where the object is, not whether anything changed.
func TestReachabilityMovedAndRestoredPasses(t *testing.T) {
	var root string
	fired := false
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/private.txt" {
			fired = true
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
	if !fired {
		t.Fatal("guard never saw frontend/private.txt; nothing was moved")
	}
	if err != nil || string(data) != "MOVED_MARKER\n" {
		t.Fatalf("restored read = %q, %v", data, err)
	}
}

// The root is replaced after the per-call root open and before the
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

// Replacing an ancestor above the top-level root with a symlink to its moved
// self preserves the root inode. Checking only the root's final component
// would accept the moved tree, including through a scoped child's pinned root.
func TestReachabilityRootAncestorSymlink(t *testing.T) {
	for _, scope := range []string{"", "scope"} {
		for _, operation := range []string{"read", "read_file", "list", "glob", "search"} {
			t.Run(filepath.Join(scope, operation), func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "ancestor", "nested", "ws")
				if err := os.MkdirAll(filepath.Join(root, scope), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"a.txt", "b.txt"} {
					if err := os.WriteFile(filepath.Join(root, scope, name), []byte("MATCH\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				ws, err := NewWorkspace(root)
				if err != nil {
					t.Fatal(err)
				}
				ancestor := filepath.Dir(filepath.Dir(ws.root))
				armed, moved := false, false
				ws.SetScopeGuard(func(rel string, _ bool) error {
					if armed && rel == filepath.ToSlash(filepath.Join(scope, "b.txt")) && !moved {
						moved = true
						if err := os.Rename(ancestor, ancestor+"-moved"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(ancestor+"-moved", ancestor); err != nil {
							t.Fatal(err)
						}
					}
					return nil
				})
				target := ws
				if scope != "" {
					child, _, cleanup, err := newScopedWorkspace(ws, scope)
					if err != nil {
						t.Fatal(err)
					}
					defer cleanup()
					target = child
				}
				if data, err := target.readAll("b.txt"); err != nil || string(data) != "MATCH\n" {
					t.Fatalf("ordinary read = %q, %v", data, err)
				}
				armed = true
				switch operation {
				case "read":
					data, err := target.readAll("b.txt")
					if !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
						t.Fatalf("read through ancestor symlink = %q, %v; want ErrRootReplaced and no bytes", data, err)
					}
				case "list", "glob":
					content, isError := invokeListing(t, target, operation, "")
					if !isError || content != "path changed during access" {
						t.Fatalf("%s through ancestor symlink = %q, IsError=%v", operation, content, isError)
					}
				default:
					var tool agent.Tool = NewReadFile(target)
					raw := json.RawMessage(`{"path":"b.txt"}`)
					if operation == "search" {
						tool = NewSearch(target)
						raw = json.RawMessage(`{"pattern":"MATCH"}`)
					}
					res, err := tool.Invoke(t.Context(), raw)
					if err != nil || !res.IsError || res.Content != "path changed during access" {
						t.Fatalf("%s through ancestor symlink = %+v, %v", operation, res, err)
					}
				}
				if !moved {
					t.Fatal("guard never moved the root ancestor")
				}
			})
		}
	}
}

// A scoped child pinned at frontend keeps its descriptor for its whole
// lifetime; moving the scope under a denied name must fail its reads.
func TestReachabilityScopedChildScopeMoved(t *testing.T) {
	parent, root := reachFixture(t, nil)
	child, counts, cleanup, err := newScopedWorkspace(parent, "frontend")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if data, err := child.readAll("private.txt"); err != nil || string(data) != "MOVED_MARKER\n" {
		t.Fatalf("child read before move = %q, %v", data, err)
	}
	moveIntoVault(t, root)()
	res, err := NewReadFile(child).Invoke(t.Context(), json.RawMessage(`{"path":"private.txt"}`))
	if err != nil || !res.IsError || res.Content != "path changed during access" {
		t.Fatalf("child read_file = %+v, %v", res, err)
	}
	if counts.evaluations.Load() != 0 || counts.requests.Load() != 0 {
		t.Fatalf("reachability failure counted as policy denial: evaluations=%d, requests=%d", counts.evaluations.Load(), counts.requests.Load())
	}
}

// A scoped child's own root, looked up as ".", is its scope directory. Once the
// scope moves under a denied name, or a new directory takes its name, that
// lookup fails as a replaced root, like every other path through the scope.
func TestReachabilityScopedChildOwnRootLookup(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved", true: "replaced"}[replace], func(t *testing.T) {
			parent, root := reachFixture(t, nil)
			child, _, cleanup, err := newScopedWorkspace(parent, "frontend")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			moveIntoVault(t, root)()
			if replace {
				if err := os.Mkdir(filepath.Join(root, "frontend"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			f, err := child.openDir(".")
			if f != nil {
				_ = f.Close()
			}
			if !errors.Is(err, ErrRootReplaced) || f != nil {
				t.Fatalf("child root open = opened %v, %v; want ErrRootReplaced", f != nil, err)
			}
		})
	}
}

// An ancestor of the scope is replaced by a symlink to its moved self.
// Resolving the child's root by absolute path would follow the symlink and
// pass; only a check anchored at the parent root with O_NOFOLLOW fails it.
func TestReachabilityScopedAncestorSymlink(t *testing.T) {
	parent, root := reachFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "deep", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deep", "inner", "x.txt"), []byte("DEEP_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child, _, cleanup, err := newScopedWorkspace(parent, "deep/inner")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if data, err := child.readAll("x.txt"); err != nil || string(data) != "DEEP_MARKER\n" {
		t.Fatalf("child read before swap = %q, %v", data, err)
	}
	moved := filepath.Join(root, "vault", "deep")
	if err := os.Rename(filepath.Join(root, "deep"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(root, "deep")); err != nil {
		t.Fatal(err)
	}
	if data, err := child.readAll("x.txt"); !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("child read through symlinked ancestor = %q, %v; want ErrRootReplaced", data, err)
	}
}

// An inner child of a scoped outer child reads successfully, then the outer
// root's own final component is swapped for a symlink to its moved self; the
// inner read must fail closed. That an inner child anchors at the top-level
// root, not at the outer root, is proven by
// TestReachabilityNestedScopeSwapAboveOuterRoot: here the swapped name is the
// outer root's last component, which a by-path O_NOFOLLOW open fails anyway.
func TestReachabilityNestedScopeOuterRootSwapped(t *testing.T) {
	parent, root := reachFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "deep", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deep", "inner", "x.txt"), []byte("DEEP_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outer, _, closeOuter, err := newScopedWorkspace(parent, "deep")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOuter()
	inner, _, closeInner, err := newScopedWorkspace(outer, "inner")
	if err != nil {
		t.Fatal(err)
	}
	defer closeInner()
	if data, err := inner.readAll("x.txt"); err != nil || string(data) != "DEEP_MARKER\n" {
		t.Fatalf("nested read before move = %q, %v", data, err)
	}
	moved := filepath.Join(root, "vault", "deep")
	if err := os.Rename(filepath.Join(root, "deep"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(root, "deep")); err != nil {
		t.Fatal(err)
	}
	if data, err := inner.readAll("x.txt"); !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("nested read after ancestor swap = %q, %v; want ErrRootReplaced", data, err)
	}
}

// An ancestor ABOVE the outer scope becomes a symlink to its moved self. The
// outer root's absolute path still resolves to the same directory (only its
// final component is no-follow), so an inner child anchored at the outer root
// would pass; anchored at the top-level root, the walk meets the symlink.
func TestReachabilityNestedScopeSwapAboveOuterRoot(t *testing.T) {
	parent, root := reachFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "p", "deep", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "p", "deep", "inner", "x.txt"), []byte("DEEP_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outer, _, closeOuter, err := newScopedWorkspace(parent, "p/deep")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOuter()
	inner, _, closeInner, err := newScopedWorkspace(outer, "inner")
	if err != nil {
		t.Fatal(err)
	}
	defer closeInner()
	if data, err := inner.readAll("x.txt"); err != nil || string(data) != "DEEP_MARKER\n" {
		t.Fatalf("nested read before swap = %q, %v", data, err)
	}
	moved := filepath.Join(root, "vault", "p")
	if err := os.Rename(filepath.Join(root, "p"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(root, "p")); err != nil {
		t.Fatal(err)
	}
	if data, err := inner.readAll("x.txt"); !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("nested read after swap above outer root = %q, %v; want ErrRootReplaced", data, err)
	}
}

// Construction pins the scope directory through the same guarded open as a
// read, so the decision on the scope name must still hold after the guard
// returns. The scope moves under a denied name inside that decision: the
// constructor must fail closed, return no child and no cleanup, and leak no
// descriptor.
func TestReachabilityConstructionScopeMovedInsideGuard(t *testing.T) {
	var root string
	armed := false
	fired := false
	parent, root := reachFixture(t, func(rel string) {
		if rel == "frontend" && armed {
			armed, fired = false, true
			moveIntoVault(t, root)()
		}
	})
	// Control: unarmed construction succeeds, so the failure below is the move.
	_, _, closeControl, err := newScopedWorkspace(parent, "frontend")
	if err != nil {
		t.Fatalf("control construction = %v", err)
	}
	closeControl()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	before := openFDCount(t)
	armed = true
	child, counts, cleanup, err := newScopedWorkspace(parent, "frontend")
	if !fired {
		t.Fatal("guard never saw the scope name")
	}
	if !errors.Is(err, errFileChanged) || child != nil || counts != nil || cleanup != nil {
		t.Fatalf("construction after scope move = child %v, counts %v, cleanup %v, err %v; want errFileChanged and nothing returned", child != nil, counts != nil, cleanup != nil, err)
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("descriptors before=%d after=%d", before, after)
	}
}

// Nested construction verifies through the outer child's inherited anchor. The
// outer child's guard wraps the parent's, so the inner pin reaches the parent
// guard with the full name p/deep/inner; an ancestor above the outer root is
// swapped for a symlink to its moved self inside that decision.
func TestReachabilityNestedConstructionSwapAboveOuterRoot(t *testing.T) {
	var root string
	armed := false
	fired := false
	parent, root := reachFixture(t, func(rel string) {
		if rel == "p/deep/inner" && armed {
			armed, fired = false, true
			moved := filepath.Join(root, "vault", "p")
			if err := os.Rename(filepath.Join(root, "p"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(root, "p")); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := os.MkdirAll(filepath.Join(root, "p", "deep", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "p", "deep", "inner", "x.txt"), []byte("DEEP_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outer, _, closeOuter, err := newScopedWorkspace(parent, "p/deep")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOuter()
	// Control: unarmed nested construction succeeds and reads.
	control, _, closeControl, err := newScopedWorkspace(outer, "inner")
	if err != nil {
		t.Fatalf("control nested construction = %v", err)
	}
	if data, err := control.readAll("x.txt"); err != nil || string(data) != "DEEP_MARKER\n" {
		t.Fatalf("control nested read = %q, %v", data, err)
	}
	closeControl()
	armed = true
	inner, counts, closeInner, err := newScopedWorkspace(outer, "inner")
	if !fired {
		t.Fatal("parent guard never saw p/deep/inner")
	}
	if !errors.Is(err, errFileChanged) || inner != nil || counts != nil || closeInner != nil {
		t.Fatalf("nested construction after swap = inner %v, counts %v, cleanup %v, err %v; want errFileChanged and nothing returned", inner != nil, counts != nil, closeInner != nil, err)
	}
}

// The top-level root's canonical path becomes a symlink to its moved self
// after the read opened and the guard decided: the object and rel are
// unchanged, but the path no longer names the root without a symlink. The
// reachability check opens the root no-follow, like workspaceRoot does, and
// must fail closed.
func TestReachabilityRootPathBecomesSymlink(t *testing.T) {
	var root string
	swapped := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "frontend/private.txt" || swapped {
			return
		}
		swapped = true
		moved := root + "-moved"
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, root); err != nil {
			t.Fatal(err)
		}
	})
	data, err := ws.readAll("frontend/private.txt")
	if !swapped {
		t.Fatal("guard never saw frontend/private.txt")
	}
	if !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("read through symlinked root = %q, %v; want ErrRootReplaced", data, err)
	}
}

// The top-level root is renamed away inside the guard of a point read, so
// nothing is at the root path any more. That is a root-level condition, not a
// change to the file.
func TestReachabilityRootRenamedAwayInsideGuard(t *testing.T) {
	var root string
	moved := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "frontend/private.txt" || moved {
			return
		}
		moved = true
		if err := os.Rename(root, root+"-old"); err != nil {
			t.Fatal(err)
		}
	})
	data, err := ws.readAll("frontend/private.txt")
	if !moved {
		t.Fatal("guard never saw frontend/private.txt")
	}
	if !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("read after root renamed away = %q, %v; want ErrRootReplaced", data, err)
	}
}

// A scoped child anchors at the top-level root's construction-time identity.
// The top root is replaced either by a fresh tree with the same names, or by a
// new directory that the child's own scope directory is moved into. In the
// second case the scope still reaches the pinned directory by name; only the
// anchor identity check sees that the top root is not the one the child was
// built under.
func TestReachabilityScopedChildTopRootReplaced(t *testing.T) {
	for _, carry := range []bool{false, true} {
		t.Run(map[bool]string{false: "recreated", true: "scope carried over"}[carry], func(t *testing.T) {
			parent, root := reachFixture(t, nil)
			child, _, cleanup, err := newScopedWorkspace(parent, "frontend")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if data, err := child.readAll("private.txt"); err != nil || string(data) != "MOVED_MARKER\n" {
				t.Fatalf("child read before replacement = %q, %v", data, err)
			}
			old := root + "-old"
			if err := os.Rename(root, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if carry {
				if err := os.Rename(filepath.Join(old, "frontend"), filepath.Join(root, "frontend")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Join(root, "frontend"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "frontend", "private.txt"), []byte("REPLACEMENT\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := child.readAll("private.txt")
			if !errors.Is(err, ErrRootReplaced) || len(data) != 0 {
				t.Fatalf("child read after top root replacement = %q, %v; want ErrRootReplaced", data, err)
			}
		})
	}
}

// List opened frontend (verified), then the directory moves under a
// denied name before it is enumerated.
func TestReachabilityListDirMovedBeforeEnumeration(t *testing.T) {
	ws, root := reachFixture(t, nil)
	move := moveIntoVault(t, root)
	ws.beforeReadDir = func(rel string) {
		if rel == "frontend" {
			move()
		}
	}
	res, err := NewList(ws).Invoke(t.Context(), json.RawMessage(`{"path":"frontend"}`))
	if err != nil || !res.IsError || res.Content != "path changed during access" {
		t.Fatalf("list = %+v, %v", res, err)
	}
}

// A walk opened frontend from the held root and the directory moves
// before it is enumerated; the walk aborts instead of emitting its names.
func TestReachabilityGlobAbortsWhenSubdirMoves(t *testing.T) {
	ws, root := reachFixture(t, nil)
	move := moveIntoVault(t, root)
	ws.beforeReadDir = func(rel string) {
		if rel == "frontend" {
			move()
		}
	}
	res, err := NewGlob(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"**"}`))
	if err != nil || !res.IsError || res.Content != "path changed during access" {
		t.Fatalf("glob = %+v, %v", res, err)
	}
}

// Direct walk pins the enumeration check independently of List/Glob's later checks.
func TestReachabilityWalkRejectsMovedEnumeration(t *testing.T) {
	ws, root := reachFixture(t, nil)
	move := moveIntoVault(t, root)
	ws.beforeReadDir = func(rel string) {
		if rel == "frontend" {
			move()
		}
	}
	var names []string
	err := ws.walk(t.Context(), func(rel string, _ fs.DirEntry) error {
		names = append(names, rel)
		return nil
	})
	if !errors.Is(err, errFileChanged) || strings.Contains(strings.Join(names, "\n"), "private.txt") {
		t.Fatalf("walk = %v, %v; want changed path before child names", names, err)
	}
}

// invokeListing runs list on dir ("" for the root) or glob "**".
func invokeListing(t *testing.T, ws *Workspace, tool, dir string) (string, bool) {
	t.Helper()
	if tool == "list" {
		raw, _ := json.Marshal(map[string]string{"path": dir})
		res, err := NewList(ws).Invoke(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return res.Content, res.IsError
	}
	res, err := NewGlob(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"**"}`))
	if err != nil {
		t.Fatal(err)
	}
	return res.Content, res.IsError
}

// Directory enumeration has finished when the final entry's guard moves its
// parent. There is no later child-directory recursion to catch this move.
func TestReachabilityNamesMovedInsideEntryGuard(t *testing.T) {
	for _, tool := range []string{"list", "glob"} {
		t.Run(tool, func(t *testing.T) {
			var move func()
			calls := 0
			ws, root := reachFixture(t, func(rel string) {
				if rel == "frontend/private.txt" {
					calls++
					move()
				}
			})
			move = moveIntoVault(t, root)
			content, isError := invokeListing(t, ws, tool, "frontend")
			if calls != 1 || !isError || content != "path changed during access" {
				t.Fatalf("%s: content=%q, IsError=%v, guard calls=%d", tool, content, isError, calls)
			}
		})
	}
}

// The final-entry schedule on a flat root: the only entry's guard replaces the
// root itself. No directory follows for a recursion check to catch it, so the
// listing must be verified after its entry guards.
func TestReachabilityFlatRootReplacedInsideEntryGuard(t *testing.T) {
	for _, tool := range []string{"list", "glob"} {
		t.Run(tool, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "flat")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "only.txt"), []byte("ORIGINAL\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ws, err := NewWorkspace(dir)
			if err != nil {
				t.Fatal(err)
			}
			root := ws.root
			calls := 0
			ws.SetScopeGuard(func(rel string, _ bool) error {
				if rel != "only.txt" {
					return nil
				}
				calls++
				if calls > 1 {
					return nil
				}
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "only.txt"), []byte("REPLACEMENT\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return nil
			})
			content, isError := invokeListing(t, ws, tool, "")
			if calls != 1 || !isError || content != "path changed during access" {
				t.Fatalf("%s: content=%q, IsError=%v, guard calls=%d", tool, content, isError, calls)
			}
		})
	}
}

// A walk entry that holds no directory cannot be bound to its listing; the
// check fails closed instead of passing the name through.
func TestReachabilityWalkedParentRequiresHeldDirectory(t *testing.T) {
	ws, root := reachFixture(t, nil)
	info, err := os.Lstat(filepath.Join(root, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.verifyWalkedParent("keep.txt", fs.FileInfoToDirEntry(info)); !errors.Is(err, errWalkEntryUnheld) {
		t.Fatalf("unheld entry = %v", err)
	}
}

// A truncated listing is still verified after its entry guards: the move
// happens in an early entry's guard, and truncation must not skip the check.
func TestReachabilityTruncatedListMovedInsideEntryGuard(t *testing.T) {
	var move func()
	calls := 0
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/f0005.txt" {
			calls++
			move()
		}
	})
	for i := 0; i < listMaxEntries+5; i++ {
		if err := os.WriteFile(filepath.Join(root, "frontend", fmt.Sprintf("f%04d.txt", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	move = moveIntoVault(t, root)
	content, isError := invokeListing(t, ws, "list", "frontend")
	if calls != 1 || !isError || content != "path changed during access" {
		t.Fatalf("content tail=%q, IsError=%v, guard calls=%d", content[max(0, len(content)-60):], isError, calls)
	}
}

// A directory the walk enumerated moves away inside its own entry guard,
// before the walk recurses into it. The recursion's open then finds nothing,
// or a symlink, at that name: a changed path, like the identity mismatch the
// recursion already reports.
func TestReachabilityWalkChildMovedInsideEntryGuard(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved", true: "symlinked"}[symlink], func(t *testing.T) {
			var root string
			calls := 0
			ws, root := reachFixture(t, func(rel string) {
				if rel != "frontend" {
					return
				}
				calls++
				if calls > 1 {
					return
				}
				moveIntoVault(t, root)()
				if symlink {
					if err := os.Symlink(filepath.Join(root, "vault", "frontend"), filepath.Join(root, "frontend")); err != nil {
						t.Fatal(err)
					}
				}
			})
			content, isError := invokeListing(t, ws, "glob", "")
			if calls != 1 || !isError || content != "path changed during access" {
				t.Fatalf("content=%q, IsError=%v, guard calls=%d", content, isError, calls)
			}
		})
	}
}

// The walk's guard allows frontend/private.txt, then the directory moves
// under a denied name before the file is opened through the held descriptor.
// The file is skipped; other matches survive.
func TestReachabilitySearchSkipsMovedFile(t *testing.T) {
	var move func()
	ws, root := reachFixture(t, func(rel string) {
		if rel == "frontend/private.txt" {
			move()
		}
	})
	move = moveIntoVault(t, root)
	res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MARKER"}`))
	if err != nil || res.IsError || res.Content != "keep.txt:1: KEEP_MARKER" {
		t.Fatalf("search = %+v, %v", res, err)
	}
}

// A skipped file's descriptor is closed, not left for the garbage collector.
// The directory moves into the vault inside every search's guard decision and
// is restored between searches. GC stays off so a finalizer cannot close a
// leaked *os.File and hide the leak.
func TestReachabilitySearchSkipReleasesDescriptors(t *testing.T) {
	var root string
	moved := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "frontend/private.txt" {
			return
		}
		if err := os.Rename(filepath.Join(root, "frontend"), filepath.Join(root, "vault", "frontend")); err != nil {
			t.Fatal(err)
		}
		moved = true
	})
	search := func() {
		t.Helper()
		res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MARKER"}`))
		if err != nil || res.IsError || res.Content != "keep.txt:1: KEEP_MARKER" {
			t.Fatalf("search = %+v, %v", res, err)
		}
		if !moved {
			t.Fatal("guard never moved the directory; the skip path was not exercised")
		}
		moved = false
		if err := os.Rename(filepath.Join(root, "vault", "frontend"), filepath.Join(root, "frontend")); err != nil {
			t.Fatal(err)
		}
	}
	search()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	before := openFDCount(t)
	for range 64 {
		search()
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("descriptors before=%d after=%d", before, after)
	}
}

// Root replacement is a root-level condition, not a per-file mismatch: search
// aborts like glob and list instead of reporting absence ("no matches") or
// emitting the replaced root's content. The root is renamed away and recreated
// inside the guard of the file being searched.
func TestReachabilitySearchAbortsOnRootReplacement(t *testing.T) {
	cases := []struct {
		name         string
		files, after map[string]string
		trigger      string
	}{
		{"only file", map[string]string{"only.txt": "MATCH ORIGINAL\n"}, map[string]string{"only.txt": "MATCH REPLACEMENT\n"}, "only.txt"},
		{"last file after a match", map[string]string{"a/a.txt": "MATCH A_OLD\n", "z.txt": "MATCH Z_OLD\n"}, map[string]string{"z.txt": "MATCH Z_NEW\n"}, "z.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write := func(base string, files map[string]string) {
				for rel, body := range files {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(base, rel)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(base, rel), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			dir := filepath.Join(t.TempDir(), "ws")
			write(dir, tc.files)
			ws, err := NewWorkspace(dir)
			if err != nil {
				t.Fatal(err)
			}
			root := ws.root
			calls := 0
			ws.SetScopeGuard(func(rel string, _ bool) error {
				if rel != tc.trigger {
					return nil
				}
				calls++
				if calls > 1 {
					return nil
				}
				if err := os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				write(root, tc.after)
				return nil
			})
			res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
			if calls != 1 || err != nil || !res.IsError || res.Content != "path changed during access" {
				t.Fatalf("search = %+v, %v, guard calls=%d; want the changed-path error", res, err, calls)
			}
		})
	}
}

// The root is renamed away, or renamed and its path replaced by a symlink to
// its moved self, inside the guard of the second of three matching files. The
// root path no longer reaches the root: search aborts like glob and list
// instead of returning the first file's match as if the rest had none.
func TestReachabilitySearchAbortsWhenRootMovesAway(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "renamed away", true: "symlinked"}[symlink], func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ws")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("MATCH "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ws, err := NewWorkspace(dir)
			if err != nil {
				t.Fatal(err)
			}
			root := ws.root
			calls := 0
			ws.SetScopeGuard(func(rel string, _ bool) error {
				if rel != "b.txt" {
					return nil
				}
				calls++
				if calls > 1 {
					return nil
				}
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if symlink {
					if err := os.Symlink(moved, root); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
			if calls != 1 || err != nil || !res.IsError || res.Content != "path changed during access" {
				t.Fatalf("search = %+v, %v, guard calls=%d; want the changed-path error", res, err, calls)
			}
		})
	}
}

// Search permission is lost on the path to the workspace's own root inside the
// guard of the second of three matching files: on the top-level root's parent,
// or on an ancestor of a scoped child's scope. That is an anchor-level failure:
// search aborts with the cause's text instead of returning the first file's
// match as if the rest had none. Control: an unreadable file below the root is
// still skipped.
func TestReachabilitySearchAbortsWhenAnchorUnreachable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not deny root; permission assertions require non-root")
	}
	for _, tc := range []struct {
		name  string
		scope string                   // "" searches the top-level workspace
		lock  func(root string) string // the path made unsearchable
		want  string
	}{
		{"top-level root parent", "", filepath.Dir, "path is not accessible"},
		{"scoped child ancestor", "deep/inner", func(root string) string { return filepath.Join(root, "deep") }, "path is not accessible"},
		{"unreadable file below the root", "", func(root string) string { return filepath.Join(root, "b.txt") }, "a.txt:1: MATCH a.txt\nc.txt:1: MATCH c.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ws")
			if err := os.MkdirAll(filepath.Join(dir, tc.scope), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				if err := os.WriteFile(filepath.Join(dir, tc.scope, name), []byte("MATCH "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ws, err := NewWorkspace(dir)
			if err != nil {
				t.Fatal(err)
			}
			locked := tc.lock(ws.root)
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			trigger := filepath.ToSlash(filepath.Join(tc.scope, "b.txt"))
			calls := 0
			ws.SetScopeGuard(func(rel string, _ bool) error {
				if rel != trigger {
					return nil
				}
				calls++
				if calls == 1 {
					if err := os.Chmod(locked, 0); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			target := ws
			if tc.scope != "" {
				child, _, cleanup, err := newScopedWorkspace(ws, tc.scope)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				target = child
			}
			res, err := NewSearch(target).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
			wantErr := tc.want == "path is not accessible"
			if calls != 1 || err != nil || res.IsError != wantErr || res.Content != tc.want {
				t.Fatalf("search = %+v, %v, guard calls=%d; want IsError=%v %q", res, err, calls, wantErr, tc.want)
			}
		})
	}
}

// A scoped child searches its scope, which moves under a denied name, or is
// replaced by a new directory of the same name, inside the guard of the second
// of three matching files. The child's own root is gone either way: search
// aborts instead of returning a partial result, and the failure is not counted
// as a policy denial.
func TestReachabilityScopedSearchAbortsWhenScopeMoves(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved", true: "replaced"}[replace], func(t *testing.T) {
			var root string
			calls := 0
			// The child's guard hands the parent guard the full name.
			parent, root := reachFixture(t, func(rel string) {
				if rel != "scope/b.txt" {
					return
				}
				calls++
				if calls > 1 {
					return
				}
				if err := os.Rename(filepath.Join(root, "scope"), filepath.Join(root, "vault", "scope")); err != nil {
					t.Fatal(err)
				}
				if !replace {
					return
				}
				if err := os.Mkdir(filepath.Join(root, "scope"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "scope", "c.txt"), []byte("MATCH NEW_CONTENT\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			if err := os.Mkdir(filepath.Join(root, "scope"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				if err := os.WriteFile(filepath.Join(root, "scope", name), []byte("MATCH "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			child, counts, cleanup, err := newScopedWorkspace(parent, "scope")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			res, err := NewSearch(child).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
			if calls != 1 || err != nil || !res.IsError || res.Content != "path changed during access" {
				t.Fatalf("child search = %+v, %v, guard calls=%d; want the changed-path error", res, err, calls)
			}
			if counts.evaluations.Load() != 0 || counts.requests.Load() != 0 {
				t.Fatalf("reachability failure counted as policy denial: evaluations=%d, requests=%d", counts.evaluations.Load(), counts.requests.Load())
			}
		})
	}
}

// An entry that is not a regular file cannot be read and is skipped; the
// search continues and returns the regular file's match. The FIFO sorts first,
// so a search that aborted on it would never reach b.txt.
func TestReachabilitySearchSkipsNonRegularEntry(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "a.pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this filesystem: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("MATCH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"MATCH"}`))
	if err != nil || res.IsError || res.Content != "b.txt:1: MATCH" {
		t.Fatalf("search = %+v, %v", res, err)
	}
}

// openWalked fails closed for an entry that holds no directory: a by-name
// fallback would consult the guard again and re-list every ancestor.
func TestReachabilityOpenWalkedRequiresHeldDirectory(t *testing.T) {
	ws, root := reachFixture(t, nil)
	info, err := os.Lstat(filepath.Join(root, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ws.openWalked("keep.txt", fs.FileInfoToDirEntry(info))
	opened := f != nil
	if opened {
		_ = f.Close()
	}
	if !errors.Is(err, errWalkEntryUnheld) || opened {
		t.Fatalf("unheld entry = opened %v, %v; want errWalkEntryUnheld and no file", opened, err)
	}
}

// Control: ordinary tool output is unchanged when nothing moves.
func TestReachabilityOrdinaryOutputsUnchanged(t *testing.T) {
	ws, _ := reachFixture(t, nil)
	cases := []struct {
		name, raw, want string
		invoke          func(json.RawMessage) (string, bool, error)
	}{
		{"read_file", `{"path":"frontend/private.txt"}`, "MOVED_MARKER\n", toolInvoker(t, NewReadFile(ws))},
		{"search", `{"pattern":"MARKER"}`, "frontend/private.txt:1: MOVED_MARKER\nkeep.txt:1: KEEP_MARKER", toolInvoker(t, NewSearch(ws))},
		{"list", `{}`, "frontend/\nkeep.txt", toolInvoker(t, NewList(ws))},
		{"glob", `{"pattern":"**"}`, "frontend/\nfrontend/private.txt\nkeep.txt", toolInvoker(t, NewGlob(ws))},
	}
	for _, tc := range cases {
		got, isErr, err := tc.invoke(json.RawMessage(tc.raw))
		if err != nil || isErr || got != tc.want {
			t.Errorf("%s = %q (error=%v), %v; want %q", tc.name, got, isErr, err, tc.want)
		}
	}
}

func toolInvoker(t *testing.T, tool interface {
	Invoke(context.Context, json.RawMessage) (agent.ToolResult, error)
}) func(json.RawMessage) (string, bool, error) {
	return func(raw json.RawMessage) (string, bool, error) {
		res, err := tool.Invoke(t.Context(), raw)
		return res.Content, res.IsError, err
	}
}

// Control: a case alias resolves through the canonical spelling the open
// recorded, so verification passes on a case-insensitive filesystem. The guard
// must also have decided on that canonical spelling, not the caller's.
func TestReachabilityCaseAliasPasses(t *testing.T) {
	var seen []string
	ws, root := reachFixture(t, func(rel string) { seen = append(seen, rel) })
	if _, err := os.Lstat(filepath.Join(root, "FRONTEND")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if data, err := ws.readAll("FRONTEND/private.txt"); err != nil || string(data) != "MOVED_MARKER\n" {
		t.Fatalf("case alias read = %q, %v", data, err)
	}
	f, canonical, err := ws.openRead("FRONTEND/private.txt", false)
	if err != nil {
		t.Fatalf("case alias open: %v", err)
	}
	_ = f.Close()
	if canonical != "frontend/private.txt" {
		t.Fatalf("canonical = %q, want frontend/private.txt", canonical)
	}
	if len(seen) != 2 {
		t.Fatalf("guard fired %d times, want 2 (%q)", len(seen), seen)
	}
	for _, rel := range seen {
		if rel != "frontend/private.txt" {
			t.Fatalf("guard saw %q, want the canonical spelling", rel)
		}
	}
}

// Control: moving an unrelated sibling during the decision does not fail the
// read, whether the sibling is a file beside the root, a directory beside a
// walked directory, or a directory beside the leaf at the intermediate level.
func TestReachabilitySiblingMovePasses(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		dir            bool
	}{
		{"file at root", "keep.txt", "kept.txt", false},
		{"directory beside walked directory", "side", "side-moved", true},
		{"directory beside leaf", "frontend/other", "frontend/other-moved", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root string
			fired := 0
			ws, root := reachFixture(t, func(rel string) {
				if rel != "frontend/private.txt" {
					return
				}
				fired++
				if fired == 1 {
					if err := os.Rename(filepath.Join(root, tc.from), filepath.Join(root, tc.to)); err != nil {
						t.Fatal(err)
					}
				}
			})
			if tc.dir {
				if err := os.MkdirAll(filepath.Join(root, tc.from), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if data, err := ws.readAll("frontend/private.txt"); err != nil || string(data) != "MOVED_MARKER\n" {
				t.Fatalf("read with sibling move = %q, %v", data, err)
			}
			if fired != 1 {
				t.Fatalf("guard fired %d times for the leaf, want 1", fired)
			}
			if _, err := os.Lstat(filepath.Join(root, tc.to)); err != nil {
				t.Fatalf("sibling was not moved: %v", err)
			}
		})
	}
}

// A middle component of rel, neither the first below the root nor the leaf,
// becomes a symlink to its moved self inside the guard. Verification must open
// every component no-follow, not only the first. The change is below the root,
// so it is a per-file errFileChanged, not ErrRootReplaced.
func TestReachabilityMiddleComponentSymlink(t *testing.T) {
	var root string
	fired := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "frontend/mid/x.txt" || fired {
			return
		}
		fired = true
		moved := filepath.Join(root, "vault", "mid")
		if err := os.Rename(filepath.Join(root, "frontend", "mid"), moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, filepath.Join(root, "frontend", "mid")); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Mkdir(filepath.Join(root, "frontend", "mid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend", "mid", "x.txt"), []byte("MID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ws.readAll("frontend/mid/x.txt")
	if !fired {
		t.Fatal("guard never saw frontend/mid/x.txt")
	}
	if !errors.Is(err, errFileChanged) || errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("read through symlinked middle component = %q, %v; want errFileChanged, not ErrRootReplaced", data, err)
	}
}

// Once glob holds its cap of entries, a later candidate is never returned, so
// its parent is not verified: a parent moved inside that candidate's guard
// does not fail the truncated call.
func TestReachabilityGlobCapPrecedesParentCheck(t *testing.T) {
	var root string
	moved := false
	ws, root := reachFixture(t, func(rel string) {
		if rel != "b/x.dat" || moved {
			return
		}
		moved = true
		if err := os.Rename(filepath.Join(root, "b"), filepath.Join(root, "vault", "b")); err != nil {
			t.Fatal(err)
		}
	})
	for _, dir := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < listMaxEntries; i++ {
		if err := os.WriteFile(filepath.Join(root, "a", fmt.Sprintf("f%04d.dat", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "b", "x.dat"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := NewGlob(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"**/*.dat"}`))
	if !moved {
		t.Fatal("guard never saw b/x.dat")
	}
	if err != nil || res.IsError || !res.Truncated || strings.Contains(res.Content, "b/x.dat") {
		t.Fatalf("glob tail=%q, IsError=%v, Truncated=%v, %v; want a truncated listing without b/x.dat", res.Content[max(0, len(res.Content)-60):], res.IsError, res.Truncated, err)
	}
}

// Control: an unchanged deep path verifies and reads normally.
// TestReachabilityMiddleComponentSymlink proves components past the first are
// checked.
func TestReachabilityDeepPathPasses(t *testing.T) {
	ws, root := reachFixture(t, nil)
	rel := filepath.Join("a", "b", "c", "d", "e", "f.txt")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("DEEP\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := ws.readAll(rel); err != nil || string(data) != "DEEP\n" {
		t.Fatalf("deep read = %q, %v", data, err)
	}
}

// openChain opens the directory that names lead to from root, one descriptor at
// a time and never following a symlink, creating each directory first when
// mkdir is set. The caller closes the result. A long tree's absolute path is
// too long for a single path lookup on Darwin, so tests reach it this way.
func openChain(t *testing.T, root string, names []string, mkdir bool) int {
	t.Helper()
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if mkdir {
			if err := unix.Mkdirat(dir, name, 0o700); err != nil {
				_ = unix.Close(dir)
				t.Fatal(err)
			}
		}
		next, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dir)
		if err != nil {
			t.Fatal(err)
		}
		dir = next
	}
	return dir
}

// longPathTree builds five nested directories with 240-byte names under root
// and f.txt (LONG_MARKER) in the innermost one. Each name fits NAME_MAX, but
// the directories joined exceed Darwin's PATH_MAX (1024), so no single path
// lookup can reach the file. It returns the directory names and the file's
// relative path.
func longPathTree(t *testing.T, root string) ([]string, string) {
	t.Helper()
	names := make([]string, 5)
	for i := range names {
		names[i] = fmt.Sprint(i) + strings.Repeat("d", 239)
	}
	if run := filepath.Join(names...); len(run) <= 1024 {
		t.Fatalf("directory run is %d bytes; it must exceed Darwin's PATH_MAX", len(run))
	}
	dir := openChain(t, root, names, true)
	defer func() { _ = unix.Close(dir) }()
	fd, err := unix.Openat(dir, "f.txt", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = unix.Write(fd, []byte("LONG_MARKER\n"))
	_ = unix.Close(fd)
	if err != nil {
		t.Fatal(err)
	}
	return names, filepath.Join(filepath.Join(names...), "f.txt")
}

// Control: a path whose directories joined exceed Darwin's PATH_MAX, though
// each name is short enough, still verifies and reads. Verification must not
// depend on looking the whole run up in one call.
func TestReachabilityLongPathPasses(t *testing.T) {
	ws, root := reachFixture(t, nil)
	_, rel := longPathTree(t, root)
	data, err := ws.readAll(rel)
	if err != nil || string(data) != "LONG_MARKER\n" {
		t.Fatalf("long path read = %q, %v", data, err)
	}
}

// A middle directory of a path too long for one lookup becomes a symlink to its
// moved self inside the guard. Verifying such a path one name at a time must
// still refuse the symlink: a per-file errFileChanged, not ErrRootReplaced.
func TestReachabilityLongPathMiddleSymlink(t *testing.T) {
	var root, rel string
	var names []string
	fired := false
	ws, root := reachFixture(t, func(r string) {
		if r != rel || fired {
			return
		}
		fired = true
		parent := openChain(t, root, names[:2], false)
		defer func() { _ = unix.Close(parent) }()
		vault := openChain(t, root, []string{"vault"}, false)
		defer func() { _ = unix.Close(vault) }()
		if err := unix.Renameat(parent, names[2], vault, names[2]); err != nil {
			t.Fatal(err)
		}
		if err := unix.Symlinkat(filepath.Join(root, "vault", names[2]), parent, names[2]); err != nil {
			t.Fatal(err)
		}
	})
	names, rel = longPathTree(t, root)
	data, err := ws.readAll(rel)
	if !fired {
		t.Fatal("guard never saw the long path")
	}
	if !errors.Is(err, errFileChanged) || errors.Is(err, ErrRootReplaced) || len(data) != 0 {
		t.Fatalf("read through symlinked middle directory = %q, %v; want errFileChanged, not ErrRootReplaced", data, err)
	}
}
