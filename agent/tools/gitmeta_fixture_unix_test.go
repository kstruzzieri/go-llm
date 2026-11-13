//go:build darwin || linux

package tools

// Shared synthetic git fixture, snapshot oracle and attack table for the
// git-metadata deny (#721). Task-level consumers: the discovery tests in
// sandbox_links_unix_test.go, and the Seatbelt and bwrap behavioral tests,
// which run each attack in the sandbox and require an unchanged snapshot.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// canonGitTempDir returns a fresh symlink-resolved temp directory, so paths
// the code under test reports compare equal to paths the test builds.
func canonGitTempDir(t *testing.T) string {
	t.Helper()
	_, canon := canonicalTempDir(t)
	return canon
}

// writeGitFixture builds, without a git binary, a workspace holding a
// top-level repository with a submodule m (which has its own nested
// submodule inner), a slash-named submodule libs/a (libs is an
// intermediate), a linked worktree wt, an independent nested repository and
// one ordinary source file.
func writeGitFixture(t *testing.T, root string) {
	t.Helper()
	const hook = "#!/bin/sh\nexit 0\n"
	files := []struct {
		rel, body string
		mode      os.FileMode
	}{
		{".git/HEAD", "ref: refs/heads/main\n", 0o644},
		{".git/config", "[core]\n\trepositoryformatversion = 0\n", 0o644},
		{".git/hooks/pre-commit", hook, 0o755},
		{".git/info/exclude", "# exclude\n", 0o644},
		{".git/modules/m/HEAD", "ref: refs/heads/main\n", 0o644},
		{".git/modules/m/config", "[core]\n\tworktree = ../../../m\n", 0o644},
		{".git/modules/m/hooks/pre-commit", hook, 0o755},
		{".git/modules/m/info/exclude", "# exclude\n", 0o644},
		{".git/modules/m/modules/inner/HEAD", "ref: refs/heads/main\n", 0o644},
		{".git/modules/m/modules/inner/config", "[core]\n\tbare = false\n", 0o644},
		{".git/modules/libs/a/HEAD", "ref: refs/heads/main\n", 0o644},
		{".git/modules/libs/a/config", "[core]\n\tworktree = ../../../../libs/a\n", 0o644},
		{".git/worktrees/wt/HEAD", "ref: refs/heads/wtbranch\n", 0o644},
		{".git/worktrees/wt/commondir", "../..\n", 0o644},
		{".git/worktrees/wt/gitdir", root + "/wt/.git\n", 0o644},
		{"m/.git", "gitdir: ../.git/modules/m\n", 0o644},
		{"libs/a/.git", "gitdir: ../../.git/modules/libs/a\n", 0o644},
		{"wt/.git", "gitdir: " + root + "/.git/worktrees/wt\n", 0o644},
		{"nested/.git/HEAD", "ref: refs/heads/main\n", 0o644},
		{"nested/.git/config", "[core]\n\tbare = false\n", 0o644},
		{"nested/.git/hooks/pre-commit", hook, 0o755},
		{"src/x.go", "package x\n", 0o644},
	}
	dirs := []string{
		".git/objects", ".git/refs/heads",
		".git/modules/m/objects", ".git/modules/m/refs/heads",
		".git/modules/m/modules/inner/objects", ".git/modules/m/modules/inner/hooks",
		".git/modules/libs/a/objects", ".git/modules/libs/a/hooks",
		"nested/.git/objects",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(root, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), f.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, f.mode); err != nil { // umask-proof
			t.Fatal(err)
		}
	}
}

// gitMetaSnapshot renders the git metadata a sandbox must not change in
// writeGitFixture's workspace as sorted "path kind inode mode extra" lines;
// equal before and after a command means the command changed none of it.
// The node list is writeGitFixture's fixed layout, never the discovery code
// under test. Each git dir records every top-level entry (stdlib has no NFKC
// fold, so listing all names is what catches a case, ligature or Kelvin
// spelling of a protected name), and hooks/ and info/ recursively.
func gitMetaSnapshot(t *testing.T, root string) string {
	t.Helper()
	gitDirs := []string{".git", ".git/modules/m", ".git/modules/m/modules/inner",
		".git/modules/libs/a", ".git/worktrees/wt", "nested/.git"}
	lines := []string{
		// containers and the intermediate: identity only
		gitMetaNode(t, root, ".git/modules"),
		gitMetaNode(t, root, ".git/modules/m/modules"),
		gitMetaNode(t, root, ".git/worktrees"),
		gitMetaNode(t, root, ".git/modules/libs"),
		// gitfiles
		gitMetaNode(t, root, "m/.git"),
		gitMetaNode(t, root, "libs/a/.git"),
		gitMetaNode(t, root, "wt/.git"),
		// must stay absent: an uninitialized submodule's and a second
		// slash-named submodule's git dir, and a new repository
		gitMetaNode(t, root, ".git/modules/u"),
		gitMetaNode(t, root, ".git/modules/libs/b"),
		gitMetaNode(t, root, "src/.git"),
	}
	for _, g := range gitDirs {
		lines = append(lines, gitMetaNode(t, root, g))
		if fi, err := os.Lstat(filepath.Join(root, g)); err != nil || !fi.IsDir() {
			continue // missing, a file or a symlink: the node line records it
		}
		entries, err := os.ReadDir(filepath.Join(root, g))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			rel := filepath.Join(g, e.Name())
			lines = append(lines, gitMetaNode(t, root, rel))
			if e.IsDir() && (e.Name() == "hooks" || e.Name() == "info") {
				err := filepath.WalkDir(filepath.Join(root, rel), func(p string, _ fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					r, err := filepath.Rel(root, p)
					if err != nil {
						return err
					}
					lines = append(lines, gitMetaNode(t, root, r))
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// A new repository anywhere: every name that folds to .git.
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), ".git") {
			r, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			lines = append(lines, "dotgit "+r)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return strings.Join(slices.Compact(lines), "\n") + "\n"
}

// gitMetaNode is one snapshot line: kind, inode, raw mode and, for a regular
// file, its content hash (a symlink records its target).
func gitMetaNode(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return rel + " missing"
	}
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: no stat_t", rel)
	}
	kind, extra := "other", "-"
	switch {
	case fi.IsDir():
		kind = "dir"
	case fi.Mode()&fs.ModeSymlink != 0:
		kind = "link"
		if extra, err = os.Readlink(p); err != nil {
			t.Fatal(err)
		}
	case fi.Mode().IsRegular():
		kind = "file"
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		extra = hex.EncodeToString(sum[:])
	}
	return fmt.Sprintf("%s %s %d %o %s", rel, kind, st.Ino, st.Mode, extra)
}

// gitMetaAttack is one way a sandboxed command could plant host-side git
// execution. snippet runs under /bin/sh -c with the workspace as its working
// directory, after variant is applied to a fresh fixture
// (applyGitMetaVariant). seatbeltOnly marks the rows bwrap does not refuse
// (#721 R1: metadata absent at spawn, or a new repository) or that need a
// case-insensitive filesystem.
type gitMetaAttack struct {
	name, variant, snippet string
	seatbeltOnly           bool
}

const plantedConfig = `printf '[spike]\n\tplanted = PLANTED\n'`

// gitMetaAttacks is the spike's attack list (evidence/721/spike/attacks.tsv)
// plus the design amendment's rows. Not ported: the swap-* rows (they need a
// renameat2/renamex_np helper binary; rename-parent and dotgit-impostor
// exercise the same rules), deep-ancestor-rename (no deep repository here;
// nested/.git covers any depth) and nested-impostor (residual R2).
var gitMetaAttacks = []gitMetaAttack{
	{name: "direct-hook", snippet: `echo PLANTED > .git/hooks/pre-commit`},
	{name: "direct-hook-new", snippet: `echo PLANTED > .git/hooks/post-checkout`},
	{name: "delete-hook", snippet: `rm -f .git/hooks/pre-commit`},
	// spike: chmod 755 on a non-executable hook the fixture does not have
	{name: "chmod-hook", snippet: `chmod 644 .git/hooks/pre-commit`},
	{name: "direct-config", snippet: plantedConfig + ` >> .git/config`},
	// spike names: direct-info-exclude, direct-info-attributes
	{name: "info-exclude", snippet: `echo PLANTED >> .git/info/exclude`},
	{name: "info-attributes", snippet: `echo '*.py -diff' > .git/info/attributes`},
	{name: "case-hook", snippet: `echo PLANTED > .GIT/HOOKS/pre-commit`, seatbeltOnly: true},
	{name: "case-config", snippet: plantedConfig + ` >> .GIT/CONFIG`, seatbeltOnly: true},
	{name: "rename-hooks", snippet: `mv .git/hooks .git/hooks2 && mkdir .git/hooks && echo PLANTED > .git/hooks/pre-commit`},
	{name: "rename-parent", snippet: `mv .git .g2 && echo PLANTED > .g2/hooks/pre-commit; mv .g2 .git`},
	{name: "dotgit-impostor", snippet: `mv .git .g2 && mkdir -p .git/hooks && echo P > .git/hooks/pre-commit`},
	{name: "symlink-replace-hooks", snippet: `mkdir evilhooks; echo PLANTED > evilhooks/pre-commit; rm -rf .git/hooks; ln -s ../evilhooks .git/hooks`},
	{name: "symlink-alias-hooks", snippet: `ln -s .git/hooks h && echo PLANTED > h/pre-commit`},
	{name: "symlink-alias-config", snippet: `ln -s .git/config c && ` + plantedConfig + ` >> c`},
	{name: "hardlink-config", snippet: `ln .git/config cfg-alias && ` + plantedConfig + ` >> cfg-alias`},
	{name: "hardlink-hook", snippet: `ln .git/hooks/pre-commit hk && echo PLANTED >> hk`},
	{name: "commondir-create", seatbeltOnly: true, snippet: `mkdir evil && cp .git/config evil/config && ` + plantedConfig +
		` >> evil/config && ln -s ../.git/objects evil/objects && ln -s ../.git/refs evil/refs && echo ../evil > .git/commondir`},
	{name: "commondir-case", seatbeltOnly: true, snippet: `mkdir evil && cp .git/config evil/config && ` + plantedConfig +
		` >> evil/config && ln -s ../.git/objects evil/objects && ln -s ../.git/refs evil/refs && echo ../evil > .git/COMMONDIR`},
	{name: "configworktree-create", variant: "wtcfg", seatbeltOnly: true, snippet: plantedConfig + ` > .git/config.worktree`},
	{name: "configworktree-case", variant: "wtcfg", seatbeltOnly: true, snippet: plantedConfig + ` > .git/CONFIG.WORKTREE`},
	{name: "configworktree-ligature", variant: "wtcfg", seatbeltOnly: true,
		snippet: plantedConfig + ` > "$(printf '.git/con\357\254\201g.worktree')"`},
	{name: "hooks-absent-create", variant: "nohooks", seatbeltOnly: true, snippet: `mkdir .git/hooks && echo PLANTED > .git/hooks/pre-commit`},
	{name: "hooks-absent-case", variant: "nohooks", seatbeltOnly: true, snippet: `mkdir .git/HOOKS && echo PLANTED > .git/HOOKS/pre-commit`},
	{name: "hooks-absent-kelvin", variant: "nohooks", seatbeltOnly: true,
		snippet: `d="$(printf '.git/hoo\342\204\252s')"; mkdir "$d" && echo PLANTED > "$d/pre-commit"`},
	{name: "info-absent-create", variant: "nohooks", seatbeltOnly: true, snippet: `mkdir .git/info && echo PLANTED > .git/info/exclude`},
	{name: "nested-direct", snippet: `echo PLANTED > nested/.git/hooks/pre-commit`},
	{name: "nested-config", snippet: plantedConfig + ` >> nested/.git/config`},
	{name: "nested-rename-dotgit", snippet: `mv nested/.git nested/.g2 && echo PLANTED > nested/.g2/hooks/pre-commit; mv nested/.g2 nested/.git`},
	{name: "nested-grandparent-rename", snippet: `mv nested n2 && echo PLANTED > n2/.git/hooks/pre-commit; mv n2 nested`},
	{name: "submodule-hook", snippet: `echo PLANTED > .git/modules/m/hooks/pre-commit`},
	{name: "submodule-config", snippet: plantedConfig + ` >> .git/modules/m/config`},
	// spike name: submodule-gitfile-rewrite
	{name: "gitfile-rewrite", snippet: `echo 'gitdir: ../evil' > m/.git`},
	{name: "submodule-gitfile-replace", snippet: `mv m/.git m/.g2 && echo 'gitdir: ../evil' > m/.git`},
	{name: "worktree-commondir", snippet: `echo ../../../evil > .git/worktrees/wt/commondir`},
	{name: "worktree-gitfile-rewrite", snippet: `echo 'gitdir: ../evil' > wt/.git`},
	{name: "worktree-gitfile-replace", snippet: `rm -f wt/.git && echo 'gitdir: ../evil' > wt/.git`},
	{name: "new-dotgit-subdir", seatbeltOnly: true, snippet: `mkdir -p src/.git/hooks && echo PLANTED > src/.git/hooks/pre-commit`},
	{name: "new-dotgit-case", seatbeltOnly: true, snippet: `mkdir -p src/.GIT/hooks && echo PLANTED > src/.GIT/hooks/pre-commit`},
	{name: "module-rename-into", snippet: `cp -R .git/modules/m evil && echo PLANTED > evil/hooks/pre-commit && mv .git/modules/m .git/modules/m-old && mv evil .git/modules/m`},
	{name: "module-symlink", snippet: `mv .git/modules/m .git/modules/m-old && cp -R .git/modules/m-old evil && echo PLANTED > evil/hooks/pre-commit && ln -s ../../evil .git/modules/m`},
	{name: "modules-container-rename", snippet: `mv .git/modules .git/modules-old && mkdir .git/modules && cp -R .git/modules-old/m evil && echo PLANTED > evil/hooks/pre-commit && mv evil .git/modules/m`},
	{name: "intermediate-rename", snippet: `mv .git/modules/libs .git/modules/libs-old && mkdir .git/modules/libs && cp -R .git/modules/libs-old/a evil && echo PLANTED > evil/hooks/pre-commit && mv evil .git/modules/libs/a`},
	{name: "intermediate-preplant", snippet: `mkdir -p .git/modules/libs/b/hooks && echo P > .git/modules/libs/b/hooks/post-checkout`},
	{name: "uninit-preplant", snippet: `cp -R .git/modules/m evil && echo PLANTED > evil/hooks/post-checkout && mv evil .git/modules/u`},
	{name: "uninit-preplant-symlink", snippet: `cp -R .git/modules/m evil && echo PLANTED > evil/hooks/post-checkout && ln -s ../../evil .git/modules/u`},
	{name: "nested-submodule-rename", snippet: `mv .git/modules/m/modules/inner x && mkdir -p .git/modules/m/modules/inner/hooks && echo P > .git/modules/m/modules/inner/hooks/pre-commit`},
	{name: "worktree-rename-into", snippet: `cp -R .git/worktrees/wt evilwt && echo ../../../evilcommon > evilwt/commondir && mv .git/worktrees/wt .git/worktrees/wt-old && mv evilwt .git/worktrees/wt`},
	{name: "worktrees-container-rename", snippet: `mv .git/worktrees .git/worktrees-old && mkdir .git/worktrees && cp -R .git/worktrees-old/wt evilwt && echo ../../../evilcommon > evilwt/commondir && mv evilwt .git/worktrees/wt`},
	{name: "libs-direct-hook", snippet: `echo PLANTED > .git/modules/libs/a/hooks/pre-commit`},
}

// applyGitMetaVariant prepares a fresh fixture for an attack row: wtcfg turns
// on extensions.worktreeConfig with no config.worktree file; nohooks removes
// .git/hooks and .git/info.
func applyGitMetaVariant(t *testing.T, root, variant string) {
	t.Helper()
	switch variant {
	case "":
	case "wtcfg":
		f, err := os.OpenFile(filepath.Join(root, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, werr := f.WriteString("[extensions]\n\tworktreeConfig = true\n")
		if err := errors.Join(werr, f.Close()); err != nil {
			t.Fatal(err)
		}
	case "nohooks":
		for _, d := range []string{"hooks", "info"} {
			if err := os.RemoveAll(filepath.Join(root, ".git", d)); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown attack variant %q", variant)
	}
}

// TestGitMetaAttacksChangeTheSnapshotUnsandboxed is the oracle's positive
// control: with no sandbox every row must change the snapshot, so an
// unchanged snapshot under a sandbox means the sandbox refused the row, not
// that the row is broken or the oracle is blind to it. Seatbelt-only rows run
// on darwin only (two of them need its case-insensitive filesystem).
func TestGitMetaAttacksChangeTheSnapshotUnsandboxed(t *testing.T) {
	for _, a := range gitMetaAttacks {
		t.Run(a.name, func(t *testing.T) {
			if a.seatbeltOnly && runtime.GOOS != "darwin" {
				t.Skip("Seatbelt-only row")
			}
			root := canonGitTempDir(t)
			writeGitFixture(t, root)
			applyGitMetaVariant(t, root, a.variant)
			before := gitMetaSnapshot(t, root)
			if again := gitMetaSnapshot(t, root); again != before {
				t.Fatalf("snapshot is not deterministic:\n%s\n---\n%s", before, again)
			}
			cmd := exec.Command("/bin/sh", "-c", a.snippet)
			cmd.Dir = root
			out, _ := cmd.CombinedOutput() // exit status is not the oracle
			if gitMetaSnapshot(t, root) == before {
				t.Fatalf("unsandboxed attack left the snapshot unchanged; output: %s", out)
			}
		})
	}
}
