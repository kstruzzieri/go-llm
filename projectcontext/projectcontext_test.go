package projectcontext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoadFullContentHash(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}

	docs, err := (&Loader{WorkspaceRoot: root, MaxBytes: 1}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Load: got %d documents, want 1", len(docs))
	}
	doc := docs[0]
	if doc.Content != "a" {
		t.Errorf("Load: Content = %q, want %q", doc.Content, "a")
	}
	if doc.Size != 3 {
		t.Errorf("Load: Size = %d, want 3", doc.Size)
	}
	if !doc.Truncated {
		t.Error("Load: Truncated = false, want true")
	}
	if got, want := fmtHash(doc.Hash), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("Load: Hash = %s, want %s", got, want)
	}
}

func TestLoadEmptyContentEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	docs, err := (&Loader{WorkspaceRoot: root}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Load: got %d documents, want 1", len(docs))
	}
	doc := docs[0]
	if doc.Content != "" || doc.Size != 0 || doc.Truncated {
		t.Errorf("Load: empty document = {Content:%q Size:%d Truncated:%t}, want zero content and size without truncation", doc.Content, doc.Size, doc.Truncated)
	}
	if got, want := fmtHash(doc.Hash), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; got != want {
		t.Errorf("Load: Hash = %s, want %s", got, want)
	}
}

func TestLoadHashIncludesTailBeyondRetainedContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	load := func(content string) Document {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		docs, err := (&Loader{WorkspaceRoot: root, MaxBytes: 1}).Load(context.Background())
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(docs) != 1 {
			t.Fatalf("Load: got %d documents, want 1", len(docs))
		}
		return docs[0]
	}

	first := load("abc")
	second := load("axc")
	if first.Content != second.Content {
		t.Fatalf("Load: retained contents = %q and %q, want equal", first.Content, second.Content)
	}
	if first.Size != second.Size {
		t.Fatalf("Load: sizes = %d and %d, want equal", first.Size, second.Size)
	}
	if first.Hash == second.Hash {
		t.Errorf("Load: hashes = %s and %s, want different hashes for different tails", fmtHash(first.Hash), fmtHash(second.Hash))
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
	if docs[0].Size != 6 || docs[1].Size != 9 {
		t.Errorf("sizes = [%d,%d], want [6,9]", docs[0].Size, docs[1].Size)
	}
	if docs[0].Hash == ([32]byte{}) || docs[1].Hash == ([32]byte{}) || docs[0].Hash == docs[1].Hash {
		t.Errorf("hashes = [%s,%s], want distinct non-zero hashes", fmtHash(docs[0].Hash), fmtHash(docs[1].Hash))
	}
}

func TestLoadContinuesPastGlobalErrors(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{GlobalDir: "bad\x00global", WorkspaceRoot: ws}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: global errors must not block workspace context: %v", err)
	}
	if len(docs) != 1 || docs[0].Source != "workspace" || docs[0].Content != "workspace" {
		t.Fatalf("want workspace doc after skipped global error, got %+v", docs)
	}
}

func TestLoadStrictReturnsGlobalErrors(t *testing.T) {
	l := &Loader{GlobalDir: "bad\x00global", Strict: true}
	if _, err := l.Load(context.Background()); err == nil {
		t.Fatal("Load: strict global error = nil, want non-nil")
	}
}

func TestLoadReturnsCanceledGlobalOnlyContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()

	tests := []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "canceled", ctx: canceled, want: context.Canceled},
		{name: "deadline", ctx: deadline, want: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&Loader{GlobalDir: t.TempDir()}).Load(tt.ctx)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Load: error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestLoadReturnsWorkspaceErrors(t *testing.T) {
	l := &Loader{WorkspaceRoot: "bad\x00workspace"}
	if _, err := l.Load(context.Background()); err == nil {
		t.Fatal("Load: want workspace errors to remain fatal")
	}
}

func TestLoadUsesFirstMatchingConfiguredFilename(t *testing.T) {
	ws := t.TempDir()
	// Only the second candidate exists; loader should fall through to it.
	if err := os.WriteFile(filepath.Join(ws, "CLAUDE.md"), []byte("claude rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: ws, Filenames: []string{"AGENTS.md", "CLAUDE.md"}}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 || docs[0].Content != "claude rules" {
		t.Fatalf("want CLAUDE.md content, got %+v", docs)
	}
	if filepath.Base(docs[0].Path) != "CLAUDE.md" {
		t.Fatalf("Path=%q, want CLAUDE.md", docs[0].Path)
	}
}

func TestLoadFirstFilenameWinsWhenBothExist(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("agents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "CLAUDE.md"), []byte("claude"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: ws, Filenames: []string{"AGENTS.md", "CLAUDE.md"}}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 || docs[0].Content != "agents" {
		t.Fatalf("want first-filename (AGENTS.md) to win, got %+v", docs)
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

func TestLoadTruncatesOversizeFile(t *testing.T) {
	ws := t.TempDir()
	big := make([]byte, 100)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: ws, MaxBytes: 10}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc, got %d", len(docs))
	}
	if !docs[0].Truncated {
		t.Fatal("want Truncated=true")
	}
	if len(docs[0].Content) != 10 {
		t.Fatalf("Content len=%d, want 10", len(docs[0].Content))
	}
}

// A WorkspaceRoot that points at a regular file (not a directory) yields no
// document and no error: canonicalDir rejects the non-directory root.
func TestLoadNonDirectoryRootYieldsNoDoc(t *testing.T) {
	parent := t.TempDir()
	rootFile := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(rootFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loader{WorkspaceRoot: rootFile}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if docs != nil {
		t.Fatalf("non-directory root must yield nil docs, got %+v", docs)
	}
}

func fmtHash(hash [32]byte) string {
	return fmt.Sprintf("%x", hash)
}
