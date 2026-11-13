//go:build darwin || linux

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestInspectSandboxWorkspaceGitfileWorkspace(t *testing.T) {
	root := canonGitTempDir(t)
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
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
	if want := joinAll(root, "sub/.Git"); !slices.Equal(got.dotGitDirs, want) {
		t.Errorf("dotGitDirs = %q, want %q", got.dotGitDirs, want)
	}
}

func TestInspectSandboxWorkspaceScanErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	modules := filepath.Join(root, ".git/modules")
	if err := os.Chmod(modules, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(modules, 0o755) })
	if _, err := inspectSandboxWorkspace(root); err == nil || !strings.Contains(err.Error(), ".git/modules") {
		t.Fatalf("unreadable .git/modules: err = %v, want a failure naming it", err)
	}
	// The workspace walk reads .git/modules before the scan does, so the
	// call above fails in the walk; the scan's own error path is pinned
	// directly (it alone covers a directory changed between the two).
	l := sandboxGitLayout{root: root}
	err := scanGitDir(filepath.Join(root, ".git"), &l)
	if err == nil || !strings.Contains(err.Error(), `git metadata ".git/modules"`) {
		t.Fatalf("scan of unreadable .git/modules: err = %v, want a failure naming it", err)
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
	if len(got.gitDirs) != 300 {
		t.Fatalf("len(gitDirs) = %d, want 300", len(got.gitDirs))
	}
}
