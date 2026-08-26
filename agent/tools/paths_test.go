package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestWorkspaceCleanRelContainment(t *testing.T) {
	root := t.TempDir()
	// a real file inside root so happy paths resolve
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	tests := []struct {
		name    string
		input   string
		wantErr error // nil => expect success
	}{
		{"simple file", "ok.txt", nil},
		{"nested clean", "a/b/c.txt", nil},
		{"root dot", ".", nil},
		{"parent escape", "../../etc/passwd", errEscape},
		{"absolute escape", "/etc/passwd", errAbsPath},
		{"midpath escape", "a/../../b", errEscape},
		{"nul byte", "foo\x00bar", errNUL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ws.cleanRel(tt.input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("cleanRel(%q) error = %v, want %v", tt.input, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("cleanRel(%q) unexpected error: %v", tt.input, err)
			}
			if !ws.underRoot(got) {
				t.Fatalf("cleanRel(%q) = %q not under root %q", tt.input, got, ws.root)
			}
		})
	}
}

func TestWorkspaceUnderRootPrefixSibling(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "rootx") // shares "root" prefix but is NOT under root
	for _, d := range []string{root, sibling} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// Use ws.root (canonical) for in-root assertions: on macOS /var→/private/var
	// means filepath.Join(root,"f.txt") differs from the canonicalized ws.root prefix.
	siblingCanon, err := filepath.EvalSymlinks(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if ws.underRoot(filepath.Join(siblingCanon, "f.txt")) {
		t.Fatalf("underRoot leaked across prefix-sibling: %q accepted", siblingCanon)
	}
	if !ws.underRoot(filepath.Join(ws.root, "f.txt")) {
		t.Fatalf("underRoot rejected a real in-root path")
	}
	if !ws.underRoot(ws.root) {
		t.Fatalf("underRoot rejected the root itself")
	}
}

func TestWorkspaceUnderVolumeRoot(t *testing.T) {
	root := filepath.VolumeName(os.TempDir()) + string(os.PathSeparator)
	ws := &Workspace{root: root}
	child := filepath.Join(root, "child")
	if !ws.underRoot(child) {
		t.Fatalf("underRoot(%q) rejected child of volume root %q", child, root)
	}
}

func TestNewWorkspaceCanonicalizesRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws, err := NewWorkspace(link) // root itself may legitimately be a symlinked path
	if err != nil {
		t.Fatalf("NewWorkspace(symlinked root): %v", err)
	}
	realCanon, _ := filepath.EvalSymlinks(real)
	if ws.root != realCanon {
		t.Fatalf("root not canonicalized: got %q want %q", ws.root, realCanon)
	}
}

func TestWorkspaceCanonicalPathForUndoKeepsDistinctCaseSensitiveNames(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "Case.txt")
	lower := filepath.Join(root, "case.txt")
	if err := os.WriteFile(upper, []byte("upper"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lower, []byte("lower"), 0o600); err != nil {
		t.Fatal(err)
	}
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("filesystem is case-insensitive")
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	gotUpper, err := ws.CanonicalPathForUndo("Case.txt")
	if err != nil {
		t.Fatal(err)
	}
	gotLower, err := ws.CanonicalPathForUndo("case.txt")
	if err != nil {
		t.Fatal(err)
	}
	if gotUpper != "Case.txt" || gotLower != "case.txt" {
		t.Fatalf("canonical paths = %q, %q; want distinct spellings", gotUpper, gotLower)
	}
}

func TestNewWorkspaceErrors(t *testing.T) {
	if _, err := NewWorkspace(""); err == nil {
		t.Fatal("empty root should error")
	}
	if _, err := NewWorkspace(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("non-existent root should error")
	}
}

func TestWorkspaceWalk(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "sub"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"a.txt":       "x",
		".git/config": "y",
		"sub/b.txt":   "z",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	if err := ws.walk(context.Background(), func(rel string, d fs.DirEntry) error {
		seen[rel] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !seen["a.txt"] || !seen["sub/b.txt"] {
		t.Fatalf("walk missed expected files: %v", seen)
	}
	if seen["."] {
		t.Fatal("walk should skip the root '.' entry")
	}
	for rel := range seen {
		if strings.HasPrefix(rel, ".git") {
			t.Fatalf("walk did not skip the ignore-set .git dir: saw %q", rel)
		}
	}
}

func TestResolveFileRejectsSymlinkAndKind(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	reg := filepath.Join(root, "reg.txt")
	if err := os.WriteFile(reg, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(reg, filepath.Join(root, "ptr")); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := ws.resolveFile("reg.txt"); err != nil {
		t.Fatalf("resolveFile on regular file errored: %v", err)
	}
	if _, _, err := ws.resolveFile("sub"); !errors.Is(err, errNotRegular) {
		t.Fatalf("resolveFile on a directory: got %v, want errNotRegular", err)
	}
	// "evil" is an in-root symlink whose target is OUTSIDE root; rejected as a
	// symlink by Lstat, not by containment (cleanRel passes the in-root link path).
	if _, _, err := ws.resolveFile("evil"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveFile on out-of-root-pointing symlink: got %v, want errSymlink", err)
	}
	// "ptr" is an in-root symlink to an in-root file; still never followed.
	if _, _, err := ws.resolveFile("ptr"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveFile on in-root symlink: got %v, want errSymlink", err)
	}
	if _, _, err := ws.resolveFile("missing.txt"); err == nil {
		t.Fatal("resolveFile on missing file should error")
	}
}

func TestOpenRegularFileRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "reg.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ws.openRegularFile("reg.txt")
	if err != nil {
		t.Fatalf("openRegularFile on regular file errored: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if f, err := ws.openRegularFile("evil"); !errors.Is(err, errSymlink) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("openRegularFile on symlink: got %v, want errSymlink", err)
	}
}

func TestResolveFileRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := ws.resolveFile("linkdir/secret.txt"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveFile through intermediate symlink: got %v, want errSymlink", err)
	}
	if f, err := ws.openRegularFile("linkdir/secret.txt"); !errors.Is(err, errSymlink) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("openRegularFile through intermediate symlink: got %v, want errSymlink", err)
	}
}

func TestResolveDirRejectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	regFile := filepath.Join(root, "f.txt")
	if err := os.WriteFile(regFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.resolveDir("."); err != nil {
		t.Fatalf("resolveDir(root) errored: %v", err)
	}
	if _, _, err := ws.resolveDir("real"); err != nil {
		t.Fatalf("resolveDir(real subdir) errored: %v", err)
	}
	if _, _, err := ws.resolveDir("linkdir"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveDir on symlinked dir: got %v, want errSymlink", err)
	}
	if _, _, err := ws.resolveDir("f.txt"); !errors.Is(err, errNotDir) {
		t.Fatalf("resolveDir on a regular file: got %v, want errNotDir", err)
	}
}

func TestResolveDirRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := ws.resolveDir("linkdir/nested"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveDir through intermediate symlink: got %v, want errSymlink", err)
	}
	if f, err := ws.openDir("linkdir/nested"); !errors.Is(err, errSymlink) {
		if err == nil {
			_ = f.Close()
		}
		t.Fatalf("openDir through intermediate symlink: got %v, want errSymlink", err)
	}
}

func TestNewFileTools(t *testing.T) {
	tools, err := NewFileTools(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileTools: %v", err)
	}
	want := map[string]bool{"read_file": false, "search": false, "glob": false, "list": false}
	for _, tl := range tools {
		name := tl.Spec().Name
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected tool %q", name)
		}
		want[name] = true
		// every B1 tool must be exactly Read / ApprovalNever
		if e := tl.Effect(); e.Class != agent.Read || e.Approval != agent.ApprovalNever {
			t.Fatalf("tool %q Effect = %+v, want Read/ApprovalNever", name, e)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("NewFileTools missing %q", name)
		}
	}
}

func TestNewFileToolsBadRoot(t *testing.T) {
	if _, err := NewFileTools(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("NewFileTools on a non-existent root should error")
	}
}

func TestResolveWriteTargetContainment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "exist.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "exist.txt"), filepath.Join(root, "linkleaf")); err != nil {
		t.Fatal(err)
	}
	ws := mustWorkspace(t, root)

	if _, exists, err := ws.resolveWriteTarget("exist.txt"); err != nil || !exists {
		t.Fatalf("existing regular file: err=%v exists=%v", err, exists)
	}
	if _, exists, err := ws.resolveWriteTarget("new.txt"); err != nil || exists {
		t.Fatalf("new file in existing dir: err=%v exists=%v", err, exists)
	}
	if _, _, err := ws.resolveWriteTarget("../escape.txt"); !errors.Is(err, errEscape) {
		t.Fatalf("escape: got %v want errEscape", err)
	}
	if _, _, err := ws.resolveWriteTarget("dir"); !errors.Is(err, errNotRegular) {
		t.Fatalf("dir leaf: got %v want errNotRegular", err)
	}
	if _, _, err := ws.resolveWriteTarget("linkleaf"); !errors.Is(err, errSymlink) {
		t.Fatalf("symlink leaf: got %v want errSymlink", err)
	}
	if _, _, err := ws.resolveWriteTarget("linkdir/x.txt"); !errors.Is(err, errSymlink) {
		t.Fatalf("symlink ancestor: got %v want errSymlink", err)
	}
	if _, _, err := ws.resolveWriteTarget("missingdir/x.txt"); err == nil {
		t.Fatal("missing parent dir must error")
	}
}

func TestWriteFileAtomicCreateAndOverwrite(t *testing.T) {
	root := t.TempDir()
	ws := mustWorkspace(t, root)

	if err := ws.WriteFileAtomic("a.txt", []byte("first")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil || string(got) != "first" {
		t.Fatalf("after create got %q err %v", got, err)
	}
	if err := ws.WriteFileAtomic("a.txt", []byte("second")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "second" {
		t.Fatalf("after overwrite got %q", got)
	}
	ents, _ := os.ReadDir(root)
	if len(ents) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(ents), ents)
	}
}

func TestWriteFileAtomicPreservesExistingMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := mustWorkspace(t, root)

	if err := ws.WriteFileAtomic("script.sh", []byte("#!/bin/sh\nexit 1\n")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode after overwrite = %v, want 0755", got)
	}
}

func TestWriteFileAtomicRejectsSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws := mustWorkspace(t, root)
	if err := ws.WriteFileAtomic("evil", []byte("x")); !errors.Is(err, errSymlink) {
		t.Fatalf("write through symlink leaf: got %v want errSymlink", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "SECRET" {
		t.Fatalf("symlink write escaped root: target now %q", got)
	}
}

func TestRemoveFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := mustWorkspace(t, root)
	if err := ws.RemoveFile("a.txt"); err != nil {
		t.Fatalf("remove regular: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("file not removed")
	}
	if err := ws.RemoveFile("dir"); !errors.Is(err, errNotRegular) {
		t.Fatalf("remove dir: got %v want errNotRegular", err)
	}
	if err := os.Symlink(filepath.Join(root, "keep.txt"), filepath.Join(root, "lnk")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ws.RemoveFile("lnk"); !errors.Is(err, errSymlink) {
		t.Fatalf("remove symlink leaf: got %v want errSymlink", err)
	}
	if err := ws.RemoveFile("../x"); !errors.Is(err, errEscape) {
		t.Fatalf("remove escape: got %v want errEscape", err)
	}
}

func TestWriteFileAtomicRejectsSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws := mustWorkspace(t, root)
	if err := ws.WriteFileAtomic("linkdir/new.txt", []byte("x")); !errors.Is(err, errSymlink) {
		t.Fatalf("write through symlink ancestor: got %v want errSymlink", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped root through symlinked ancestor")
	}
}

func TestWorkspace_ScopeGuard_DeniesReadAndWrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "plan.lock.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Guard: deny anything under .agent; deny writes outside a.txt.
	ws.SetScopeGuard(func(rel string, write bool) error {
		if rel == ".agent" || strings.HasPrefix(rel, ".agent/") {
			return errors.New("proof state")
		}
		if write && rel != "a.txt" {
			return errors.New("out of scope")
		}
		return nil
	})

	if _, _, err := ws.resolveFile(".agent/plan.lock.json"); err == nil {
		t.Fatal("expected read of .agent to be denied")
	}
	if _, _, err := ws.resolveWriteTarget("b.txt"); err == nil {
		t.Fatal("expected write of b.txt (out of scope) to be denied")
	}
	if _, _, err := ws.resolveWriteTarget("a.txt"); err != nil {
		t.Fatalf("write of a.txt should pass: %v", err)
	}
}

func TestScopeGuardPreservesHostErrorAndSanitizesFileTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	guardErr := &fs.PathError{Op: "scope", Path: "/host/policy/detail", Err: context.Canceled}
	denied := true
	writeOnly := false
	ws.SetScopeGuard(func(_ string, write bool) error {
		if denied && (!writeOnly || write) {
			return guardErr
		}
		return nil
	})

	err = ws.WriteFileAtomic("a.txt", []byte("changed"))
	if !errors.Is(err, guardErr) {
		t.Fatalf("WriteFileAtomic error = %v, want original guard error", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr != guardErr {
		t.Fatalf("WriteFileAtomic error = %v, want original *fs.PathError", err)
	}
	if err.Error() != guardErr.Error() {
		t.Fatalf("WriteFileAtomic error = %q, want %q", err, guardErr)
	}

	res, invokeErr := NewReadFile(ws).Invoke(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if invokeErr != nil {
		t.Fatalf("read_file Invoke: %v", invokeErr)
	}
	if !res.IsError || res.Content != "path denied by workspace policy" {
		t.Fatalf("read_file = %#v, want sanitized scope denial", res)
	}

	for _, tc := range []struct {
		name string
		tool agent.Tool
		args json.RawMessage
	}{
		{"write_file", NewWriteFile(ws, nil), json.RawMessage(`{"path":"a.txt","content":"changed"}`)},
		{"edit_file", NewEditFile(ws, nil), json.RawMessage(`{"path":"a.txt","old_string":"x","new_string":"changed"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			denied = true
			writeOnly = false
			planner := tc.tool.(agent.PlanningTool)
			if _, err := planner.Plan(context.Background(), tc.args); err == nil || err.Error() != errScopeDenied.Error() {
				t.Fatalf("Plan error = %v, want sanitized scope denial", err)
			}
			denied = false
			if _, err := planner.Plan(context.Background(), tc.args); err != nil {
				t.Fatalf("allowed Plan: %v", err)
			}
			denied = true
			writeOnly = tc.name == "edit_file"
			res, err := tc.tool.Invoke(context.Background(), tc.args)
			if err != nil || !res.IsError || res.Content != errScopeDenied.Error() {
				t.Fatalf("Invoke = %#v, %v, want sanitized scope denial", res, err)
			}
		})
	}
}

func TestWorkspace_NilGuard_BackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, _ := NewWorkspace(dir)
	if _, _, err := ws.resolveFile("a.txt"); err != nil {
		t.Fatalf("nil guard must allow: %v", err)
	}
}

func TestNewFileToolsForWorkspace_UsesExistingGuardedWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetScopeGuard(func(rel string, write bool) error {
		if rel == "a.txt" {
			return errors.New("guarded")
		}
		return nil
	})
	tools := NewFileToolsForWorkspace(ws)
	var read agent.Tool
	for _, tool := range tools {
		if tool.Spec().Name == "read_file" {
			read = tool
			break
		}
	}
	if read == nil {
		t.Fatal("read_file not found")
	}
	// ReadFile.Invoke reports expected failures via ToolResult.IsError with a nil
	// Go error (see readfile.go errResult / the invoke() helper in readfile_test.go),
	// so the guard denial must be observed on the result, not the error return.
	res, err := read.Invoke(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("Invoke returned a Go error (should be IsError ToolResult): %v", err)
	}
	if !res.IsError {
		t.Fatal("read_file must use the supplied guarded workspace")
	}
}
