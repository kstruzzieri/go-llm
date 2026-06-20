package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false}, // * does not cross a separator (root-anchored)
		{"sub/*.go", "sub/main.go", true},
		{"**/*.go", "main.go", true},  // ** matches zero segments
		{"**/*.go", "a/b/c.go", true}, // ** matches many segments
		{"**", "anything/at/all.txt", true},
		{"a/**/d.go", "a/b/c/d.go", true},
		{"a/**/d.go", "a/d.go", true},    // ** zero segments in the middle
		{"a/**/d.go", "x/b/d.go", false}, // anchored: must start with a/
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"cmd/golem/*", "cmd/golem/main.go", true},
		{"cmd/golem/*", "cmd/other/main.go", false},
		{"[.go", "a.go", false},          // malformed bracket pattern: returns false, never panics
		{"*", ".env", true},              // path.Match matches dotfiles (no shell dot-exclusion)
		{"", "", true},                   // degenerate: both empty
		{"**/**/*.go", "a/b/c.go", true}, // redundant ** still matches
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestGlobMatchesAndMarksDirs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.go":      "x",
		"sub/util.go":  "y",
		"sub/data.txt": "z",
	})
	g := NewGlob(mustWorkspace(t, root))

	res := invoke(t, g, map[string]any{"pattern": "**/*.go"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "main.go") || !strings.Contains(res.Content, "sub/util.go") {
		t.Fatalf("missing go files: %q", res.Content)
	}
	if strings.Contains(res.Content, "data.txt") {
		t.Fatalf("matched a non-go file: %q", res.Content)
	}
}

func TestGlobMarksSymlinkUnresolved(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "s.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "s.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	g := NewGlob(mustWorkspace(t, root))
	res := invoke(t, g, map[string]any{"pattern": "*.txt"})
	if !strings.Contains(res.Content, "link.txt") || !strings.Contains(res.Content, "symlink") {
		t.Fatalf("symlink entry should be emitted marked: %q", res.Content)
	}
	if strings.Contains(res.Content, "secret") {
		t.Fatal("glob must not resolve/read the symlink target")
	}
}

func TestGlobDoesNotDescendSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"hidden.go": "package hidden\n"})
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeTree(t, root, map[string]string{"real.go": "package real\n"})
	g := NewGlob(mustWorkspace(t, root))
	res := invoke(t, g, map[string]any{"pattern": "**/*.go"})
	if strings.Contains(res.Content, "hidden.go") || strings.Contains(res.Content, "linkdir/") {
		t.Fatalf("glob descended into symlinked dir: %q", res.Content)
	}
	if !strings.Contains(res.Content, "real.go") {
		t.Fatalf("missed real in-root match: %q", res.Content)
	}
}

func TestGlobEffect(t *testing.T) {
	g := NewGlob(mustWorkspace(t, t.TempDir()))
	e := g.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("Effect = %+v, want Read/ApprovalNever", e)
	}
}

func TestListHappyAndMarkers(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"f.txt":       "x",
		"sub/deep.go": "y",
	})
	l := NewList(mustWorkspace(t, root))

	res := invoke(t, l, map[string]any{"path": "."})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "f.txt") || !strings.Contains(res.Content, "sub/") {
		t.Fatalf("list of root wrong (sub should be marked dir): %q", res.Content)
	}
	if strings.Contains(res.Content, "deep.go") {
		t.Fatalf("list must be single-level, not recursive: %q", res.Content)
	}
}

func TestListDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"only.txt": "x"})
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{}) // no path
	if res.IsError || !strings.Contains(res.Content, "only.txt") {
		t.Fatalf("empty path should list root: %+v", res)
	}
}

func TestListSymlinkedDirTargetRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{"path": "linkdir"})
	if !res.IsError {
		t.Fatalf("listing a symlinked dir target must be rejected, got %q", res.Content)
	}
	if strings.Contains(res.Content, "secret") {
		t.Fatal("list leaked out-of-root contents via symlinked dir")
	}
}

func TestListMarksSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real.txt": "x"})
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "ptr.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{"path": "."})
	if !strings.Contains(res.Content, "ptr.txt (symlink)") {
		t.Fatalf("symlink entry inside a real dir should be marked, got %q", res.Content)
	}
}

func TestListEffect(t *testing.T) {
	l := NewList(mustWorkspace(t, t.TempDir()))
	e := l.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("Effect = %+v, want Read/ApprovalNever", e)
	}
}
