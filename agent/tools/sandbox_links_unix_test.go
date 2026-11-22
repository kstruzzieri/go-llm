//go:build darwin || linux

package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Direct unit coverage for the shared workspace hard-link validator (#442,
// relocated for #441) and the git layout discovery that shares its walk
// (#721). Backend-level coverage of the darwin call site lives in
// exec_darwin_test.go; the Linux call site is covered in exec_linux_test.go.

func TestValidateSandboxWorkspaceLinksAllowsInternalLinks(t *testing.T) {
	ws := t.TempDir()
	first := filepath.Join(ws, "first")
	if err := os.WriteFile(first, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(ws, "second")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectSandboxWorkspace(ws); err != nil {
		t.Fatalf("wholly internal links rejected: %v", err)
	}
}

func TestValidateSandboxWorkspaceLinksRejectsExternalLink(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-canary")
	if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(ws, "pre-linked")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	_, err := inspectSandboxWorkspace(ws)
	if err == nil {
		t.Fatal("externally linked workspace entry accepted")
	}
	if !strings.Contains(err.Error(), "linked outside the workspace") ||
		!strings.Contains(err.Error(), `"pre-linked"`) {
		t.Fatalf("error must name the escaping entry: %v", err)
	}
}

func TestValidateSandboxWorkspaceLinksSkipsSymlinks(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "multi")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(outsideDir, "multi2")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "ptr")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := inspectSandboxWorkspace(ws); err != nil {
		t.Fatalf("symlink treated as hard link: %v", err)
	}
}

// joinAll spells each workspace-relative path under root.
func joinAll(root string, rels ...string) []string {
	out := make([]string, 0, len(rels))
	for _, r := range rels {
		out = append(out, filepath.Join(root, r))
	}
	return out
}

func TestInspectSandboxWorkspaceFindsGitLayout(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	for _, c := range []struct {
		field     string
		got, want []string
	}{
		{"dotGitDirs", got.dotGitDirs, joinAll(root, ".git", "nested/.git")},
		{"gitDirs", got.gitDirs, joinAll(root, ".git/modules/libs/a", ".git/modules/m",
			".git/modules/m/modules/inner", ".git/worktrees/wt")},
		{"containers", got.containers, joinAll(root, ".git/modules", ".git/modules/m/modules", ".git/worktrees")},
		{"intermediates", got.intermediates, joinAll(root, ".git/modules/libs")},
		{"gitFiles", got.gitFiles, joinAll(root, "libs/a/.git", "m/.git", "wt/.git")},
		// Absent on purpose: .git/commondir, .git/modules/libs/a/info.
		{"metadata", got.metadata, joinAll(root,
			".git/config", ".git/hooks", ".git/info",
			".git/modules/libs/a/config", ".git/modules/libs/a/hooks",
			".git/modules/m/config", ".git/modules/m/hooks", ".git/modules/m/info",
			".git/modules/m/modules/inner/config", ".git/modules/m/modules/inner/hooks",
			".git/worktrees/wt/commondir", ".git/worktrees/wt/gitdir",
			"nested/.git/config", "nested/.git/hooks")},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.field, c.got, c.want)
		}
	}
}

func TestInspectSandboxWorkspaceBrokenModuleIsProtectedNotDescended(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	broken := filepath.Join(root, ".git/modules/broken")
	if err := os.MkdirAll(filepath.Join(broken, "objects/ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if !slices.Contains(got.gitDirs, broken) {
		t.Errorf("broken module not a git dir: %q", got.gitDirs)
	}
	for _, d := range append(slices.Clone(got.intermediates), got.gitDirs...) {
		if d != broken && strings.HasPrefix(d, broken+"/") {
			t.Errorf("descended into the broken module: %q", d)
		}
	}
	if slices.Contains(got.intermediates, broken) {
		t.Errorf("broken module classed as an intermediate: %q", got.intermediates)
	}
}

// TestInspectSandboxWorkspaceSlashNamedHooksSubmodule: submodules named
// libs/hooks and libs/a are both git dirs under the intermediate libs; a
// sibling named hooks does not turn libs into a git dir that hides libs/a.
func TestInspectSandboxWorkspaceSlashNamedHooksSubmodule(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root) // .git/modules/libs/a has a HEAD file
	if err := os.MkdirAll(filepath.Join(root, ".git/modules/libs/hooks/objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git/modules/libs/hooks/HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	libs := filepath.Join(root, ".git/modules/libs")
	// containment: GITMETA-DISCOVERY
	for _, g := range joinAll(root, ".git/modules/libs/a", ".git/modules/libs/hooks") {
		if !slices.Contains(got.gitDirs, g) {
			t.Errorf("%s not a git dir: %q", g, got.gitDirs)
		}
	}
	if !slices.Contains(got.intermediates, libs) || slices.Contains(got.gitDirs, libs) {
		t.Errorf("libs: gitDirs %q, intermediates %q; want it an intermediate", got.gitDirs, got.intermediates)
	}
}

// TestInspectSandboxWorkspaceHostLitterKeepsIntermediate: Finder's .DS_Store
// and an AppleDouble ._ file in the intermediate libs do not make it a broken
// git dir that hides its submodule libs/a.
func TestInspectSandboxWorkspaceHostLitterKeepsIntermediate(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root) // .git/modules/libs/a has a HEAD file
	libs := filepath.Join(root, ".git/modules/libs")
	for _, name := range []string{".DS_Store", "._x"} {
		if err := os.WriteFile(filepath.Join(libs, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if !slices.Contains(got.intermediates, libs) || slices.Contains(got.gitDirs, libs) {
		t.Errorf("libs: gitDirs %q, intermediates %q; want it an intermediate", got.gitDirs, got.intermediates)
	}
	if a := filepath.Join(libs, "a"); !slices.Contains(got.gitDirs, a) {
		t.Errorf("%s not a git dir: %q", a, got.gitDirs)
	}
}

// TestInspectSandboxWorkspaceStrippedModuleIsIntermediate: a module git dir a
// command stripped of HEAD and every top-level file is an intermediate, and
// its hooks/ and info/ are moduleReadOnly, not git dirs.
func TestInspectSandboxWorkspaceStrippedModuleIsIntermediate(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	applyGitMetaVariant(t, root, "stripped")
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	s := filepath.Join(root, ".git/modules/s")
	if !slices.Contains(got.intermediates, s) {
		t.Errorf("s not an intermediate: %q", got.intermediates)
	}
	if want := joinAll(root, ".git/modules/s/hooks", ".git/modules/s/info"); !slices.Equal(got.moduleReadOnly, want) {
		t.Errorf("moduleReadOnly %q, want %q", got.moduleReadOnly, want)
	}
	for _, g := range joinAll(root, ".git/modules/s", ".git/modules/s/hooks", ".git/modules/s/info") {
		if slices.Contains(got.gitDirs, g) || slices.Contains(got.metadata, g) {
			t.Errorf("%s in gitDirs or metadata: %q, %q", g, got.gitDirs, got.metadata)
		}
	}
}

// TestInspectSandboxWorkspaceModuleNamedHooks: a submodule git dir named
// hooks (directly under a modules container, or as libs/Hooks under an
// intermediate) is a git dir while it has HEAD; once a command deletes HEAD it
// is moduleReadOnly, at its on-disk spelling, and neither a git dir nor
// metadata.
func TestInspectSandboxWorkspaceModuleNamedHooks(t *testing.T) {
	for _, c := range []struct {
		variant, rel string
		head         bool
	}{
		{"modhooks", ".git/modules/hooks", true},
		{"modhooks", ".git/modules/hooks", false},
		{"libshooks", ".git/modules/libs/Hooks", false},
	} {
		t.Run(fmt.Sprintf("%s head=%v", c.rel, c.head), func(t *testing.T) {
			root := canonGitTempDir(t)
			writeGitFixture(t, root)
			applyGitMetaVariant(t, root, c.variant)
			g := filepath.Join(root, c.rel)
			if !c.head {
				if err := os.Remove(filepath.Join(g, "HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			// containment: GITMETA-DISCOVERY
			inGitDirs, inReadOnly := slices.Contains(got.gitDirs, g), slices.Contains(got.moduleReadOnly, g)
			if inGitDirs != c.head || inReadOnly == c.head || slices.Contains(got.metadata, g) {
				t.Errorf("%s: gitDirs %q, moduleReadOnly %q, metadata %q", c.rel, got.gitDirs, got.moduleReadOnly, got.metadata)
			}
		})
	}
}

func TestInspectSandboxWorkspaceGitfileWorkspace(t *testing.T) {
	root := canonGitTempDir(t)
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if want := joinAll(root, ".git"); !slices.Equal(got.gitFiles, want) {
		t.Errorf("gitFiles = %q, want %q", got.gitFiles, want)
	}
	if len(got.dotGitDirs) != 0 || len(got.gitDirs) != 0 {
		t.Errorf("a gitfile is not a git dir: dotGitDirs %q gitDirs %q", got.dotGitDirs, got.gitDirs)
	}
}

func TestInspectSandboxWorkspaceCaseFoldedDotGit(t *testing.T) {
	root := canonGitTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "sub/.Git/hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if want := joinAll(root, "sub/.Git"); !slices.Equal(got.dotGitDirs, want) {
		t.Errorf("dotGitDirs = %q, want %q", got.dotGitDirs, want)
	}
}

func TestInspectSandboxWorkspaceScanErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, c := range []struct {
		name, chmod string
		mode        os.FileMode
		want        string // the path the scan's error must name
	}{
		{"modules", ".git/modules", 0o000, ".git/modules"},       // listDir(modules): ReadDir fails
		{"worktrees", ".git/worktrees", 0o000, ".git/worktrees"}, // listDir(worktrees): ReadDir fails
		{"metadata", ".git", 0o600, ".git/hooks"},                // readable, not searchable: Lstat fails
	} {
		t.Run(c.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			writeGitFixture(t, root)
			p := filepath.Join(root, c.chmod)
			if err := os.Chmod(p, c.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
			if _, err := inspectSandboxWorkspace(root); err == nil || !strings.Contains(err.Error(), c.chmod) {
				t.Fatalf("unreadable %s: err = %v, want a failure naming it", c.chmod, err)
			}
			// The workspace walk reads these before the scan does, so the
			// call above fails in the walk; the scan's own error paths are
			// pinned directly (they alone cover a change between the two).
			// containment: GITMETA-DISCOVERY
			l := sandboxGitLayout{root: root}
			err := scanGitDir(filepath.Join(root, ".git"), &l)
			if want := fmt.Sprintf("git metadata %q", c.want); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("scan with unreadable %s: err = %v, want a failure naming %s", c.chmod, err, c.want)
			}
		})
	}
}

// TestInspectSandboxWorkspaceSymlinkedDotGitIsNeither pins residual R9: a
// symlink named .git is neither a git dir nor a gitfile (a read-only bind of
// it would follow the link out of the user's layout).
func TestInspectSandboxWorkspaceSymlinkedDotGitIsNeither(t *testing.T) {
	root := canonGitTempDir(t)
	if err := os.Symlink("x", filepath.Join(root, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if len(got.gitFiles) != 0 || len(got.dotGitDirs) != 0 {
		t.Errorf("symlinked .git recorded: gitFiles %q dotGitDirs %q", got.gitFiles, got.dotGitDirs)
	}
}

func TestScanGitDirTreatsAbsentAsAbsent(t *testing.T) {
	root := canonGitTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git/HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := sandboxGitLayout{root: root}
	if err := scanGitDir(filepath.Join(root, ".git"), &l); err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if len(l.containers)+len(l.gitDirs)+len(l.metadata)+len(l.intermediates) != 0 {
		t.Errorf("absent entries recorded: %+v", l)
	}
}

func TestInspectSandboxWorkspaceManySubmodules(t *testing.T) {
	root := canonGitTempDir(t)
	for i := range 300 {
		d := filepath.Join(root, ".git/modules", fmt.Sprintf("s%03d", i))
		if err := os.MkdirAll(filepath.Join(d, "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-DISCOVERY
	if len(got.gitDirs) != 300 {
		t.Fatalf("len(gitDirs) = %d, want 300", len(got.gitDirs))
	}
}

// inspectWithChange inspects root, applying change once, at the first visit
// of rel (workspace-relative; "." is the root): after its parent listed it and
// before the walk reads it, as git's automatic gc deletes loose objects. It
// also reports how many passes the inspection made.
func inspectWithChange(t *testing.T, root, rel string, change func(p string) error) (sandboxGitLayout, int, error) {
	t.Helper()
	target := filepath.Join(root, rel)
	passes, fired := 0, false
	got, err := inspectSandboxWorkspaceWithHook(root, func(name string) {
		if name == root {
			passes++
		}
		if name != target || fired {
			return
		}
		fired = true
		if err := change(name); err != nil {
			t.Errorf("change %s: %v", rel, err)
		}
	})
	if !fired {
		t.Fatalf("the walk never visited %s", rel)
	}
	return got, passes, err
}

// inspectSandboxWorkspaceWithHook supplies directory entries whose Info is
// cached, as on filesystems without directory-entry types. The hook changes
// the workspace after the listing and before the inspector visits the entry.
func inspectSandboxWorkspaceWithHook(root string, beforeVisit func(string)) (sandboxGitLayout, error) {
	return inspectSandboxWorkspaceWithWalk(root, func(root string, visit fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return visit(path, entry, err)
			}
			info, err := entry.Info()
			if err != nil {
				return visit(path, entry, err)
			}
			beforeVisit(path)
			return visit(path, fs.FileInfoToDirEntry(info), nil)
		})
	})
}

// requireListed fails t unless rel's presence in field matches want.
func requireListed(t *testing.T, field string, list []string, root, rel string, want bool) {
	t.Helper()
	if got := slices.Contains(list, filepath.Join(root, rel)); got != want {
		t.Errorf("%s holds %s = %v, want %v: %q", field, rel, got, want, list)
	}
}

// A pass that sees an entry its parent listed no longer resolve is discarded
// and the walk starts again: the second pass sees the workspace as it now
// stands, a renamed entry under its new name.
func TestInspectSandboxWorkspaceRewalksAfterChange(t *testing.T) {
	remove := func(string, string) func(string) error { return os.RemoveAll }
	renameTo := func(root, rel string) func(string) error {
		return func(p string) error { return os.Rename(p, filepath.Join(root, rel)) }
	}
	for _, c := range []struct {
		name, change string
		do           func(root, arg string) func(string) error
		arg          string
		check        func(t *testing.T, root string, got sandboxGitLayout)
	}{
		{name: "file removed", change: "src/b.go", do: remove},
		{name: "directory removed", change: "build", do: remove},
		{name: "loose object pruned", change: ".git/objects/ab/cdef0123", do: remove},
		{name: "emptied fan-out directory removed", change: ".git/objects/ab", do: remove},
		{name: "worktree git dir pruned", change: ".git/worktrees/wt", do: remove,
			check: func(t *testing.T, root string, got sandboxGitLayout) {
				requireListed(t, "gitDirs", got.gitDirs, root, ".git/worktrees/wt", false)
				requireListed(t, "metadata", got.metadata, root, ".git/worktrees/wt/gitdir", false)
				requireListed(t, "gitFiles", got.gitFiles, root, "wt/.git", true)
				requireListed(t, "metadata", got.metadata, root, ".git/modules/m/config", true)
			}},
		{name: "gitfile removed", change: "libs/a/.git", do: remove,
			check: func(t *testing.T, root string, got sandboxGitLayout) {
				requireListed(t, "gitFiles", got.gitFiles, root, "libs/a/.git", false)
				requireListed(t, "gitDirs", got.gitDirs, root, ".git/modules/libs/a", true)
			}},
		{name: "nested .git removed", change: "nested/.git", do: remove,
			check: func(t *testing.T, root string, got sandboxGitLayout) {
				requireListed(t, "dotGitDirs", got.dotGitDirs, root, "nested/.git", false)
				requireListed(t, "metadata", got.metadata, root, "nested/.git/config", false)
				requireListed(t, "dotGitDirs", got.dotGitDirs, root, ".git", true)
			}},
		{name: "checkout renamed", change: "nested", do: renameTo, arg: "nested-moved",
			check: func(t *testing.T, root string, got sandboxGitLayout) {
				requireListed(t, "dotGitDirs", got.dotGitDirs, root, "nested-moved/.git", true)
				requireListed(t, "metadata", got.metadata, root, "nested-moved/.git/config", true)
				requireListed(t, "metadata", got.metadata, root, "nested-moved/.git/hooks", true)
				requireListed(t, "dotGitDirs", got.dotGitDirs, root, "nested/.git", false)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			writeGitFixture(t, root)
			for _, rel := range []string{"src/b.go", "build/out.o", ".git/objects/ab/cdef0123"} {
				p := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, passes, err := inspectWithChange(t, root, c.change, c.do(root, c.arg))
			// containment: GITMETA-DISCOVERY
			if err != nil {
				t.Fatalf("%s changed during the walk refused the spawn: %v", c.change, err)
			}
			if passes != 2 {
				t.Errorf("passes = %d, want 2: the changed pass is discarded and one clean pass follows", passes)
			}
			if c.check != nil {
				c.check(t, root, got)
			}
			want, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("layout after %s changed:\n got %+v\nwant %+v", c.change, got, want)
			}
		})
	}
}

// What is not a change to an entry the walk listed still fails the spawn in
// the first pass: the root gone, an entry that became another type. A change
// in every pass fails it after the last.
func TestInspectSandboxWorkspaceChangeRefusals(t *testing.T) {
	// containment: GITMETA-DISCOVERY
	t.Run("root missing", func(t *testing.T) {
		_, err := inspectSandboxWorkspace(filepath.Join(t.TempDir(), "gone"))
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSandboxWorkspaceChanged) || strings.HasSuffix(err.Error(), "; retry") {
			t.Fatalf("err = %v, want a not-exist failure, not a change", err)
		}
	})
	t.Run("root removed", func(t *testing.T) {
		root := canonGitTempDir(t)
		writeGitFixture(t, root)
		_, passes, err := inspectWithChange(t, root, ".", os.RemoveAll)
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errSandboxWorkspaceChanged) || strings.HasSuffix(err.Error(), "; retry") || passes != 1 {
			t.Fatalf("err = %v after %d passes, want a not-exist failure in pass 1", err, passes)
		}
	})
	t.Run("directory became a file", func(t *testing.T) {
		root := canonGitTempDir(t)
		writeGitFixture(t, root)
		_, passes, err := inspectWithChange(t, root, "src", func(p string) error {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			return os.WriteFile(p, []byte("x"), 0o644)
		})
		if !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), `"src"`) || passes != 1 {
			t.Fatalf("err = %v after %d passes, want ENOTDIR naming src in pass 1", err, passes)
		}
	})
	t.Run("changes in every pass", func(t *testing.T) {
		root := canonGitTempDir(t)
		writeGitFixture(t, root)
		churn := filepath.Join(root, "churn")
		passes := 0
		_, err := inspectSandboxWorkspaceWithHook(root, func(name string) {
			var err error
			switch name {
			case root: // before the root is listed: churn is listed in every pass
				passes++
				err = os.Mkdir(churn, 0o755)
			case churn: // before churn is read
				err = os.Remove(churn)
			}
			if err != nil {
				t.Error(err)
			}
		})
		if !errors.Is(err, errSandboxWorkspaceChanged) || !strings.Contains(err.Error(), `"churn"`) ||
			!strings.HasSuffix(err.Error(), "; retry") || passes != 3 {
			t.Fatalf("err = %v after %d passes, want a change naming churn after 3", err, passes)
		}
	})
}

// A rename after the listing hides nothing from the hard-link refusal: the
// second pass finds the inode under its new name and the third confirms the shortfall.
func TestInspectSandboxWorkspaceRenamedOutsideLinkStillRefused(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-canary")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(ws, "a")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	_, passes, err := inspectWithChange(t, ws, "a", func(p string) error { return os.Rename(p, filepath.Join(ws, "b")) })
	// containment: GITMETA-DISCOVERY
	if err == nil || !strings.Contains(err.Error(), `"b" is linked outside the workspace`) || passes != 3 {
		t.Fatalf("err = %v after %d passes, want b refused in pass 3", err, passes)
	}
}

// A pass whose link counts change under it is discarded too, and the next
// pass counts every name again. A count left short is checked at the inode's
// first name, so a name removed during the pass is not taken for a link
// outside the workspace.
func TestInspectSandboxWorkspaceLinkChangeRewalks(t *testing.T) {
	write := func(t *testing.T, p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pair := func(first, second string) func(t *testing.T, ws string) {
		return func(t *testing.T, ws string) {
			write(t, filepath.Join(ws, first))
			if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, second)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(filepath.Join(ws, first), filepath.Join(ws, second)); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}
	}
	t.Run("different short counts on consecutive passes", func(t *testing.T) {
		ws := t.TempDir()
		names := []string{"one", "two"}
		for _, name := range names {
			pair("a/"+name, "b/"+name)(t, ws)
		}
		passes := 0
		_, err := inspectSandboxWorkspaceWithHook(ws, func(path string) {
			if path == ws {
				passes++
			}
			if path == filepath.Join(ws, "b") && passes <= len(names) {
				// Move a different internal name behind the walk in each pass.
				name := names[passes-1]
				if err := os.Rename(filepath.Join(path, name), filepath.Join(ws, "a", "moved-"+name)); err != nil {
					t.Fatal(err)
				}
			}
		})
		// containment: GITMETA-DISCOVERY
		if err != nil || passes != 3 {
			t.Fatalf("err = %v after %d passes, want success in pass 3", err, passes)
		}
	})
	for _, c := range []struct {
		name, change string
		setup        func(t *testing.T, ws string)
		do           func(ws, p string) error
	}{
		// a is counted with two links; a third appears before m is read.
		{"third link added", "m", pair("a", "m"),
			func(ws, p string) error { return os.Link(p, filepath.Join(ws, "z")) }},
		// a/f is counted with two links, then removed: b/g has one.
		{"counted name removed", "b", pair("a/f", "b/g"),
			func(ws, _ string) error { return os.Remove(filepath.Join(ws, "a/f")) }},
		// b/g is removed before b is listed: a/f is short one name.
		{"uncounted name removed", "b", pair("a/f", "b/g"),
			func(ws, _ string) error { return os.Remove(filepath.Join(ws, "b/g")) }},
		// Both links stay internal, but b/g moves into a after a was walked.
		{"internal name moved behind the walk", "b", pair("a/f", "b/g"),
			func(ws, _ string) error {
				return os.Rename(filepath.Join(ws, "b/g"), filepath.Join(ws, "a/g"))
			}},
		// a is walked, then moved into z before z is listed: both names count twice.
		{"directory walked twice", "z", func(t *testing.T, ws string) {
			pair("a/f", "a/g")(t, ws)
			if err := os.Mkdir(filepath.Join(ws, "z"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, func(ws, _ string) error { return os.Rename(filepath.Join(ws, "a"), filepath.Join(ws, "z/a")) }},
		// x is listed as a file and read as a directory, whose link count is 2 or more.
		{"file became a directory", "x", func(t *testing.T, ws string) { write(t, filepath.Join(ws, "x")) },
			func(_, p string) error {
				if err := os.Remove(p); err != nil {
					return err
				}
				return os.Mkdir(p, 0o755)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			c.setup(t, ws)
			_, passes, err := inspectWithChange(t, ws, c.change, func(p string) error { return c.do(ws, p) })
			if err != nil || passes != 2 {
				t.Fatalf("err = %v after %d passes, want success in pass 2", err, passes)
			}
		})
	}
}
