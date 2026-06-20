package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
