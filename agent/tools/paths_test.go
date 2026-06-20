package tools

import (
	"context"
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
	if _, err := ws.resolveDir("."); err != nil {
		t.Fatalf("resolveDir(root) errored: %v", err)
	}
	if _, err := ws.resolveDir("real"); err != nil {
		t.Fatalf("resolveDir(real subdir) errored: %v", err)
	}
	if _, err := ws.resolveDir("linkdir"); !errors.Is(err, errSymlink) {
		t.Fatalf("resolveDir on symlinked dir: got %v, want errSymlink", err)
	}
	if _, err := ws.resolveDir("f.txt"); !errors.Is(err, errNotDir) {
		t.Fatalf("resolveDir on a regular file: got %v, want errNotDir", err)
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
