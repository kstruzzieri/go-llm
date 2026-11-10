package tools

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// harnessRefRow is one reference-set fixture (#784). Paths are relative to
// the workspace root; "../" reaches the folders above it.
type harnessRefRow struct {
	name  string
	root  string            // the root below the temp folder; "" means "ws"
	files map[string]string // "{tmp}" and "{root}" in a content become slash paths
	links map[string]string // link -> target as written, relative to the link's folder
	unix  bool              // links, FIFOs, modes or the exec bit: skip on Windows
	// prep runs after the Workspace is built and before anything is checked.
	prep func(t *testing.T, tmp, root string, ws *Workspace)
	// Plan-level rows: targets that plan "" and WriteClassApprovalKey.
	covered, ordinary []string
	// Set-level rows: a substring of the incomplete set's cause.
	want string
}

// harnessRefsWorkspace creates root and returns harnessWorkspace on it, so
// the ancestor scan stops at root's parent.
func harnessRefsWorkspace(t *testing.T, root string) *Workspace {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return harnessWorkspace(t, root)
}

// harnessRefFixture builds r's files and links, with notes.txt holding
// "old\n" as the ordinary control, and returns the temp folder, the root and
// the Workspace.
func harnessRefFixture(t *testing.T, r harnessRefRow) (string, string, *Workspace) {
	t.Helper()
	if r.unix && runtime.GOOS == "windows" {
		t.Skip("symlink, FIFO, mode or exec-bit fixture")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, filepath.FromSlash(cmp.Or(r.root, "ws")))
	files := map[string]string{"notes.txt": "old\n"}
	for rel, content := range r.files {
		files[rel] = strings.NewReplacer("{tmp}", filepath.ToSlash(tmp), "{root}", filepath.ToSlash(root)).Replace(content)
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range r.links {
		p := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.FromSlash(target), p); err != nil {
			t.Fatal(err)
		}
	}
	ws := harnessRefsWorkspace(t, root)
	if r.prep != nil {
		r.prep(t, tmp, root, ws)
	}
	return tmp, root, ws
}

// harnessChmod sets p's mode for the rest of the test (skipped as root or on
// Windows) and restores it on cleanup, so t.TempDir can remove it.
func harnessChmod(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("mode fixture: Windows, or root bypasses permissions")
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
}

// harnessChain returns CLAUDE.md importing docs/h1.md and docs/hN.md
// importing docs/hN+1.md up to docs/h<n>.md, each holding "old\n".
func harnessChain(n int) map[string]string {
	files := map[string]string{"CLAUDE.md": "@docs/h1.md\n", "docs/h" + strconv.Itoa(n+1) + ".md": "old\n"}
	for i := 1; i <= n; i++ {
		files["docs/h"+strconv.Itoa(i)+".md"] = "@h" + strconv.Itoa(i+1) + ".md\nold\n"
	}
	return files
}

// TestHarnessRefsNestedRepository (acceptance 8): a nested checkout under
// .claude/worktrees is not read, so what its CLAUDE.md imports and what its
// link reaches stay ordinary, and the set is complete.
func TestHarnessRefsNestedRepository(t *testing.T) {
	r := harnessRefRow{files: map[string]string{
		".claude/worktrees/x/.git":      "gitdir: elsewhere\n",
		".claude/worktrees/x/CLAUDE.md": "@../../../docs/a.md\n",
		"docs/a.md":                     "old\n",
		"docs/b/x.md":                   "old\n",
	}}
	if runtime.GOOS != "windows" {
		r.links = map[string]string{".claude/worktrees/x/shared": "../../../docs/b"}
	}
	_, _, ws := harnessRefFixture(t, r)
	refs := ws.harnessReferences(context.Background())
	if !refs.complete {
		t.Fatalf("set incomplete: %s", refs.cause)
	}
	for _, rel := range []string{"docs/a.md", "docs/b/x.md", "notes.txt"} {
		// containment: PATH-HARNESS-REF
		if ok, why := refs.covers(rel); ok {
			t.Errorf("%s covered (%s), want ordinary", rel, why)
		}
	}
}

// TestHarnessRefsFailClosed (acceptance 9, failure half): each cause makes the
// set incomplete, names what failed, and makes notes.txt plan "".
func TestHarnessRefsFailClosed(t *testing.T) {
	rows := []harnessRefRow{
		{name: "oversized .mcp.json", files: map[string]string{".mcp.json": strings.Repeat(" ", readFileMaxBytes+1)}, want: ".mcp.json is over the size limit"},
		{name: "malformed .mcp.json", files: map[string]string{".mcp.json": `{"a":`}, want: ".mcp.json does not parse"},
		{name: "unterminated TOML string", files: map[string]string{".codex/config.toml": `x = "y`}, want: ".codex/config.toml does not parse"},
		{name: "unreadable settings", files: map[string]string{".claude/settings.json": "{}"}, want: ".claude/settings.json cannot be read",
			prep: func(t *testing.T, _, root string, _ *Workspace) {
				harnessChmod(t, filepath.Join(root, ".claude", "settings.json"), 0)
			}},
		{name: "searchable but unlistable folder", files: map[string]string{"locked/CLAUDE.md": "old\n"}, want: "locked folder cannot be listed",
			prep: func(t *testing.T, _, root string, _ *Workspace) {
				harnessChmod(t, filepath.Join(root, "locked"), 0o111)
			}},
		{name: "harness link to a FIFO", unix: true, links: map[string]string{".claude/rules/p": "../../fifo"}, want: ".claude/rules/p is not a regular file",
			prep: func(t *testing.T, _, root string, _ *Workspace) { harnessMkfifo(t, filepath.Join(root, "fifo")) }},
		{name: "entry bound", files: harnessFiles("f", 20), want: "walk stopped at the entry limit",
			prep: func(_ *testing.T, _, _ string, ws *Workspace) { ws.harnessMaxEntries = 10 }},
		{name: "symlink loop in rules", unix: true, links: map[string]string{".claude/rules/a": "b", ".claude/rules/b": "a"}, want: "link cannot be resolved"},
		{name: "imported file that cannot be read", files: map[string]string{"CLAUDE.md": "@docs/x.md\n", "docs/x.md": "x\n"}, want: "docs/x.md cannot be read",
			prep: func(t *testing.T, _, root string, _ *Workspace) {
				harnessChmod(t, filepath.Join(root, "docs", "x.md"), 0)
			}},
		{name: "dangling link chain past 40 hops", unix: true, links: harnessLinkChain(41), want: "CLAUDE.md link cannot be resolved"},
		{name: "malformed ancestor settings", root: "outer/ws", files: map[string]string{"../.claude/settings.json": `{"a":`}, want: "outer/.claude/settings.json does not parse",
			prep: func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp }},
		{name: "cancelled", want: "walk cancelled"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			_, _, ws := harnessRefFixture(t, r)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if r.name == "cancelled" {
				cancel()
			}
			refs := ws.harnessReferences(ctx)
			// containment: PATH-HARNESS-REF
			if refs.complete || !strings.Contains(refs.cause, r.want) {
				t.Fatalf("complete=%v cause=%q, want incomplete naming %q", refs.complete, refs.cause, r.want)
			}
			key, why := ws.fileToolApprovalKey(ctx, "notes.txt")
			if key != "" || !strings.HasPrefix(why, "could not check what harness files reference: ") {
				t.Fatalf("notes.txt = (%q, %q), want empty key and the could-not-check reason", key, why)
			}
		})
	}
}

// harnessFiles returns n files named <prefix><i>.txt.
func harnessFiles(prefix string, n int) map[string]string {
	files := map[string]string{}
	for i := range n {
		files[prefix+strconv.Itoa(i)+".txt"] = "x\n"
	}
	return files
}

// harnessLinkChain returns links CLAUDE.md -> l1 -> ... -> l<n>, with l<n>
// absent.
func harnessLinkChain(n int) map[string]string {
	links := map[string]string{"CLAUDE.md": "l1"}
	for i := 1; i < n; i++ {
		links["l"+strconv.Itoa(i)] = "l" + strconv.Itoa(i+1)
	}
	return links
}

// TestHarnessRefsNotFailures (acceptance 9, other half): none of these makes
// the set incomplete, and notes.txt stays ordinary.
func TestHarnessRefsNotFailures(t *testing.T) {
	rows := []harnessRefRow{
		{name: "folder removed mid-walk", files: map[string]string{"docs/x.md": "x\n"},
			prep: func(t *testing.T, _, root string, ws *Workspace) {
				ws.beforeHarnessReadDir = func(rel string) {
					if rel == "docs" {
						if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
							t.Error(err)
						}
					}
				}
			}},
		{name: "unsearchable folder", files: map[string]string{"sealed/CLAUDE.md": "@x.md\n"},
			prep: func(t *testing.T, _, root string, _ *Workspace) { harnessChmod(t, filepath.Join(root, "sealed"), 0) }},
		{name: "1 MiB skill document", files: map[string]string{".claude/skills/big.md": strings.Repeat("x", 1<<20)}},
		{name: "unknown variable", files: map[string]string{".claude/settings.json": `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"$OTHER/x.sh"}]}]}}`}},
		{name: "seven-hop import chain", files: harnessChain(7)},
		{name: "import inside an unsearchable folder", files: map[string]string{"CLAUDE.md": "@sealed/x.md\n", "sealed/x.md": "x\n"},
			prep: func(t *testing.T, _, root string, _ *Workspace) { harnessChmod(t, filepath.Join(root, "sealed"), 0) }},
		{name: "NUL in an import", files: map[string]string{"CLAUDE.md": "see @a\x00b.md\n"}},
		{name: "dangling link chain of 40 hops", unix: true, links: harnessLinkChain(40)},
		{name: "folder link cycle in rules", unix: true, files: map[string]string{".claude/rules/a.md": "x\n"},
			links: map[string]string{".claude/rules/up": ".."}},
		{name: "imports no file can have", unix: true,
			files: map[string]string{"CLAUDE.md": "@" + strings.Repeat("n", 300) + ".md @loop/x.md\n"},
			links: map[string]string{"loop": "loop"}},
		{name: "build output past the entry bound", files: harnessFiles("build/f", 20),
			prep: func(_ *testing.T, _, _ string, ws *Workspace) { ws.harnessMaxEntries = 10 }},
		{name: "ancestor folder, .DS_Store and a .codex file", root: "outer/ws",
			files: map[string]string{
				"../.claude/rules/sub/x.md": "x\n", "../.github/hooks/.DS_Store": "\x00\x01junk", "../.codex": "not a folder\n",
				"../.claude/rules/sub.md/x": "x\n", "../.github/hooks/d.json/x": "x\n",
			},
			prep: func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp }},
		{name: "ancestor .clinerules folder", root: "outer/ws",
			files: map[string]string{"../.clinerules/sub/x.md": "x\n", "../.clinerules/.DS_Store": "\x00\x01junk", "../.clinerules/ok.md": "x\n"},
			prep:  func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp }},
		{name: "folder link gone before the globs", unix: true, root: "outer/ws",
			files: map[string]string{"opencode.json": `{"instructions":["rules/*/p.md"]}`, "docs/team/p.md": "x\n"},
			links: map[string]string{"rules/team": "../docs/team"},
			prep: func(t *testing.T, tmp, root string, ws *Workspace) {
				ws.harnessAncestorStop = tmp
				ws.onHarnessAncestor = func(string) { _ = os.Remove(filepath.Join(root, "rules", "team")) }
			}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			_, _, ws := harnessRefFixture(t, r)
			refs := ws.harnessReferences(context.Background())
			// containment: PATH-HARNESS-REF
			if !refs.complete {
				t.Fatalf("set incomplete: %s", refs.cause)
			}
			if key, why := ws.fileToolApprovalKey(context.Background(), "notes.txt"); key != WriteClassApprovalKey {
				t.Fatalf("notes.txt = (%q, %q), want %q", key, why, WriteClassApprovalKey)
			}
		})
	}
}

// TestHarnessRefsAncestorDefaultStop: with no stop set, the ancestor scan
// runs from the root's parent up to, and including, the volume root. It is the
// only test that clears the helpers' default stop.
func TestHarnessRefsAncestorDefaultStop(t *testing.T) {
	ws := harnessRefsWorkspace(t, filepath.Join(t.TempDir(), "ws"))
	ws.harnessAncestorStop = ""
	var seen []string
	ws.onHarnessAncestor = func(dir string) { seen = append(seen, dir) }
	ws.harnessReferences(context.Background())
	vol := filepath.VolumeName(ws.root) + string(os.PathSeparator)
	if len(seen) == 0 || seen[0] != filepath.Dir(ws.root) || seen[len(seen)-1] != vol {
		t.Fatalf("ancestors %q, want %q up to %q", seen, filepath.Dir(ws.root), vol)
	}
}

// TestHarnessRefsHarnessFileFirst: #725's checks run before the set is
// built, so a harness file plans "" with no #784 reason even when the set is
// incomplete.
func TestHarnessRefsHarnessFileFirst(t *testing.T) {
	_, _, ws := harnessRefFixture(t, harnessRefRow{files: map[string]string{".mcp.json": `{"a":`}})
	ctx := context.Background()
	// containment: PATH-HARNESS-REF
	if key, why := ws.fileToolApprovalKey(ctx, "CLAUDE.md"); key != "" || why != "" {
		t.Fatalf("CLAUDE.md = (%q, %q), want (\"\", \"\")", key, why)
	}
	if key, why := ws.fileToolApprovalKey(ctx, "notes.txt"); key != "" || why == "" {
		t.Fatalf("control notes.txt = (%q, %q), want the could-not-check reason", key, why)
	}
}

// TestHarnessRefsOpenHarnessFile: a regular file opens; a FIFO is refused as
// not regular without blocking. errFileChanged needs the file swapped between
// the two stats, which no seam reaches deterministically, so it is not tested.
func TestHarnessRefsOpenHarnessFile(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.md")
	if err := os.WriteFile(reg, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, fi, err := openHarnessFile(reg)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("regular file: info %v, err %v", fi, err)
	}
	_ = f.Close()
	fifo := filepath.Join(dir, "fifo")
	harnessMkfifo(t, fifo)
	done := make(chan error, 1)
	go func() {
		f, _, err := openHarnessFile(fifo)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNotRegular) {
			t.Fatalf("FIFO: err %v, want errNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openHarnessFile blocked on a FIFO")
	}
}

// TestHarnessRefsVanishedLink: a link that is gone by the time its target is
// read is a vanished entry, not a failure (spec D8).
func TestHarnessRefsVanishedLink(t *testing.T) {
	hops, vanished, err := danglingTargets(filepath.Join(t.TempDir(), "gone"))
	if !vanished || hops != nil {
		t.Fatalf("danglingTargets(gone) = (%q, %v, %v), want vanished", hops, vanished, err)
	}
}

// TestHarnessRefsDanglingVirtualLink: a dangling .github link's lexical
// target still classifies what is created there (ruling R20).
func TestHarnessRefsDanglingVirtualLink(t *testing.T) {
	_, _, ws := harnessRefFixture(t, harnessRefRow{unix: true, links: map[string]string{".github": "cfg"}})
	refs := ws.harnessReferences(context.Background())
	if !refs.complete {
		t.Fatalf("set incomplete: %s", refs.cause)
	}
	// containment: PATH-HARNESS-REF
	if ok, why := refs.covers("cfg/hooks/new.json"); !ok || why != ".github links to cfg/hooks/new.json" {
		t.Fatalf("covers(cfg/hooks/new.json) = (%v, %q), want the .github reason", ok, why)
	}
	if ok, why := refs.covers("cfg/notes.txt"); ok {
		t.Fatalf("cfg/notes.txt covered: %s", why)
	}
}
