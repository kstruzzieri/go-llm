package tools

import (
	"errors"
	"fmt"
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
	if e.OutputCap < listOutputCap {
		t.Fatalf("OutputCap %d must be >= listOutputCap %d", e.OutputCap, listOutputCap)
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

func TestListFiltersScopeGuardedEntries(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".agent/proof-pack.json": "{}",
		"main.go":                "package main\n",
	})

	// Without a guard, .agent is a visible entry (it is not in ignoreDirs).
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{"path": "."})
	if !strings.Contains(res.Content, ".agent") {
		t.Fatalf("without a guard .agent should be listed: %q", res.Content)
	}

	// With a guard that denies .agent, list must not surface it (matching
	// search/glob, whose walk consults the same guard), while normal entries
	// remain visible.
	ws := mustWorkspace(t, root)
	ws.SetScopeGuard(func(rel string, _ bool) error {
		if rel == ".agent" || strings.HasPrefix(rel, ".agent/") {
			return errors.New("proof")
		}
		return nil
	})
	res = invoke(t, NewList(ws), map[string]any{"path": "."})
	if strings.Contains(res.Content, ".agent") {
		t.Fatalf("guard-denied .agent must not be listed: %q", res.Content)
	}
	if !strings.Contains(res.Content, "main.go") {
		t.Fatalf("normal entry should still be listed: %q", res.Content)
	}
}

func TestListEffect(t *testing.T) {
	l := NewList(mustWorkspace(t, t.TempDir()))
	e := l.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("Effect = %+v, want Read/ApprovalNever", e)
	}
	if e.OutputCap < listOutputCap {
		t.Fatalf("OutputCap %d must be >= listOutputCap %d", e.OutputCap, listOutputCap)
	}
}

func TestGlobMarksDirectory(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/x.go": "y"})
	g := NewGlob(mustWorkspace(t, root))
	res := invoke(t, g, map[string]any{"pattern": "sub"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "sub/") {
		t.Fatalf("glob should mark a matched directory with a trailing slash: %q", res.Content)
	}
}

func TestGlobEntryCapTruncates(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < listMaxEntries+5; i++ {
		files[fmt.Sprintf("f%04d.txt", i)] = "x"
	}
	writeTree(t, root, files)
	g := NewGlob(mustWorkspace(t, root))
	res := invoke(t, g, map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("entry cap should be partial success, got IsError: %s", res.Content)
	}
	if !res.Truncated || !strings.Contains(res.Content, "truncated") {
		t.Fatal("glob entry cap should set Truncated and an in-band marker")
	}
}

func TestListEntryCapTruncates(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < listMaxEntries+5; i++ {
		files[fmt.Sprintf("f%04d.txt", i)] = "x"
	}
	writeTree(t, root, files)
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{"path": "."})
	if res.IsError {
		t.Fatalf("entry cap should be partial success, got IsError: %s", res.Content)
	}
	if !res.Truncated || !strings.Contains(res.Content, "truncated") {
		t.Fatal("list entry cap should set Truncated and an in-band marker")
	}
}

func TestListNestedSubdirPrefixes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/deep.go": "y", "sub/inner/x.go": "z"})
	l := NewList(mustWorkspace(t, root))
	res := invoke(t, l, map[string]any{"path": "sub"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "sub/deep.go") {
		t.Fatalf("subdir listing should prefix entries with sub/: %q", res.Content)
	}
	if !strings.Contains(res.Content, "sub/inner/") {
		t.Fatalf("subdir listing should show nested dir as sub/inner/: %q", res.Content)
	}
	// single-level: must not recurse into sub/inner
	if strings.Contains(res.Content, "x.go") {
		t.Fatalf("list must be single-level, not recursive: %q", res.Content)
	}
}
