package projectcontext

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A zero-configured loader (no global dir, no workspace root) finds nothing and
// does not error.
func TestLoadEmptyWhenNothingConfigured(t *testing.T) {
	l := &Loader{}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if docs != nil {
		t.Fatalf("Load: want nil docs, got %v", docs)
	}
}

func TestLoadWorkspaceDocument(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ws rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: root}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Load: want 1 doc, got %d", len(docs))
	}
	if docs[0].Source != "workspace" {
		t.Fatalf("Source=%q, want workspace", docs[0].Source)
	}
	if docs[0].Content != "ws rules" {
		t.Fatalf("Content=%q", docs[0].Content)
	}
	// canonicalDir resolves symlinks (e.g. /var → /private/var on macOS), so
	// compare against the resolved root for a stable assertion.
	canonRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if docs[0].Path != filepath.Join(canonRoot, "AGENTS.md") {
		t.Fatalf("Path=%q", docs[0].Path)
	}
}

func TestLoadOrdersGlobalBeforeWorkspace(t *testing.T) {
	global := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(global, "AGENTS.md"), []byte("global"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: ws, GlobalDir: global}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 docs, got %d", len(docs))
	}
	if docs[0].Source != "global" || docs[1].Source != "workspace" {
		t.Fatalf("order=[%s,%s], want [global,workspace]", docs[0].Source, docs[1].Source)
	}
}

func TestLoadIgnoresEscapingConfiguredFilename(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: root, Filenames: []string{"../AGENTS.md"}}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if docs != nil {
		t.Fatalf("escaping filename must be ignored, got docs=%+v", docs)
	}
}

func TestLoadIgnoresAbsoluteConfiguredFilename(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: root, Filenames: []string{outside}}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if docs != nil {
		t.Fatalf("absolute filename must be ignored, got docs=%+v", docs)
	}
}
