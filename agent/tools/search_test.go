package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchLiteralMatch(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.go":     "package main\nfunc Foo() {}\n",
		"sub/b.go": "var Foo = 1\n",
		"c.txt":    "nothing here\n",
	})
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "Foo"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:2:") || !strings.Contains(res.Content, "sub/b.go:1:") {
		t.Fatalf("missing matches with slash paths: %q", res.Content)
	}
	if strings.Contains(res.Content, "c.txt") {
		t.Fatalf("matched a file it should not: %q", res.Content)
	}
}

func TestSearchRegexAndBadRegex(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "func Bar() {}\nfunc Baz() {}\n"})
	s := NewSearch(mustWorkspace(t, root))

	res := invoke(t, s, map[string]any{"pattern": "Ba[rz]", "regex": true})
	if res.IsError || !strings.Contains(res.Content, "Bar") || !strings.Contains(res.Content, "Baz") {
		t.Fatalf("regex match failed: %+v", res)
	}
	bad := invoke(t, s, map[string]any{"pattern": "(unclosed", "regex": true})
	if !bad.IsError {
		t.Fatalf("invalid regex should be IsError, got %q", bad.Content)
	}
}

func TestSearchSkipsBinaryAndIgnoreDirs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"keep.go":        "needle here\n",
		".git/config":    "needle in git\n",
		"node_modules/x": "needle in deps\n",
	})
	if err := os.WriteFile(filepath.Join(root, "bin"), []byte("nee\x00dle"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "needle"})
	if strings.Contains(res.Content, ".git") || strings.Contains(res.Content, "node_modules") {
		t.Fatalf("ignore set not honored: %q", res.Content)
	}
	if strings.Contains(res.Content, "bin:") {
		t.Fatalf("binary file not skipped: %q", res.Content)
	}
	if !strings.Contains(res.Content, "keep.go") {
		t.Fatalf("missed the real match: %q", res.Content)
	}
}

func TestSearchBinarySniffCoversFullWindow(t *testing.T) {
	root := t.TempDir()
	// NUL appears after the default 4 KiB bufio.Reader buffer size. The tool must
	// still treat this as binary by sniffing the full binarySniffBytes window.
	body := strings.Repeat("x", 5000) + "\x00needle\n"
	if err := os.WriteFile(filepath.Join(root, "bin.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "needle"})
	if strings.Contains(res.Content, "bin.txt") || strings.Contains(res.Content, "needle") {
		t.Fatalf("binary file with late NUL should be skipped, got %q", res.Content)
	}
}

func TestSearchSkipsSymlinkedFile(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("needle SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeTree(t, root, map[string]string{"real.txt": "needle real\n"})
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "needle"})
	if strings.Contains(res.Content, "SECRET") || strings.Contains(res.Content, "link.txt") {
		t.Fatalf("symlinked file leaked into search: %q", res.Content)
	}
	if !strings.Contains(res.Content, "real.txt") {
		t.Fatalf("missed the real match: %q", res.Content)
	}
}

func TestSearchDoesNotDescendSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"hidden.txt": "needle SECRET\n"})
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeTree(t, root, map[string]string{"real.txt": "needle real\n"})
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "needle"})
	if strings.Contains(res.Content, "SECRET") || strings.Contains(res.Content, "hidden.txt") || strings.Contains(res.Content, "linkdir") {
		t.Fatalf("search descended into symlinked dir or leaked it: %q", res.Content)
	}
	if !strings.Contains(res.Content, "real.txt") {
		t.Fatalf("missed the real match: %q", res.Content)
	}
}

func TestSearchMatchCapTruncates(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < searchMaxMatches+50; i++ {
		b.WriteString("hit\n")
	}
	writeTree(t, root, map[string]string{"many.txt": b.String()})
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "hit"})
	if res.IsError {
		t.Fatalf("cap should be partial success, got IsError: %s", res.Content)
	}
	if !res.Truncated || !strings.Contains(res.Content, "truncated") {
		t.Fatal("match cap should set Truncated and an in-band marker")
	}
}

func TestSearchEffect(t *testing.T) {
	s := NewSearch(mustWorkspace(t, t.TempDir()))
	e := s.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("Effect = %+v, want Read/ApprovalNever", e)
	}
	if e.OutputCap <= searchMaxBytes {
		t.Fatalf("OutputCap %d must exceed searchMaxBytes %d to avoid runtime re-truncation", e.OutputCap, searchMaxBytes)
	}
}

func TestSearchEmptyPattern(t *testing.T) {
	s := NewSearch(mustWorkspace(t, t.TempDir()))
	res := invoke(t, s, map[string]any{"pattern": ""})
	if !res.IsError {
		t.Fatalf("empty pattern should be IsError, got %q", res.Content)
	}
}

func TestSearchLiteralQuotesMetacharacters(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "axb\na.b\n"})
	s := NewSearch(mustWorkspace(t, root))
	// literal "a.b" must match the line "a.b" but NOT "axb" (the '.' is escaped).
	res := invoke(t, s, map[string]any{"pattern": "a.b"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, ":2: a.b") {
		t.Fatalf("literal pattern should match the a.b line: %q", res.Content)
	}
	if strings.Contains(res.Content, "axb") {
		t.Fatalf("literal '.' must not match 'x' (QuoteMeta): %q", res.Content)
	}
}

func TestSearchByteCapTruncates(t *testing.T) {
	root := t.TempDir()
	// ~100 matching lines, each ~1 KiB, so total output exceeds searchMaxBytes
	// (64 KiB) well before the 200-match cap — exercises the byte-cap path.
	var b strings.Builder
	long := strings.Repeat("x", 1000)
	for i := 0; i < 100; i++ {
		b.WriteString("needle" + long + "\n")
	}
	writeTree(t, root, map[string]string{"big.txt": b.String()})
	s := NewSearch(mustWorkspace(t, root))
	res := invoke(t, s, map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("byte cap should be partial success, got IsError: %s", res.Content)
	}
	if !res.Truncated || !strings.Contains(res.Content, "truncated") {
		t.Fatal("byte cap should set Truncated and an in-band marker")
	}
	if len(res.Content) > searchMaxBytes+markerHeadroom {
		t.Fatalf("output %d exceeds searchMaxBytes+markerHeadroom %d", len(res.Content), searchMaxBytes+markerHeadroom)
	}
}
