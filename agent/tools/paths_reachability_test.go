//go:build linux || darwin

package tools

import (
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

// R5: a scoped child pinned at frontend keeps its descriptor for its whole
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

// R5b: an ancestor of the scope is replaced by a symlink to its moved self.
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
	if data, err := child.readAll("x.txt"); !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("child read through symlinked ancestor = %q, %v", data, err)
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
	if data, err := inner.readAll("x.txt"); !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("nested read after ancestor swap = %q, %v", data, err)
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
	if data, err := inner.readAll("x.txt"); !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("nested read after swap above outer root = %q, %v", data, err)
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
	if !errors.Is(err, errFileChanged) || len(data) != 0 {
		t.Fatalf("read through symlinked root = %q, %v", data, err)
	}
}

// R4: List opened frontend (verified), then the directory moves under a
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

// R4b: a walk opened frontend from the held root and the directory moves
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
