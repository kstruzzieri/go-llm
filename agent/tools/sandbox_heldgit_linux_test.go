//go:build linux

package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// heldNote spells holdGitMachinery's note for a rename from one
// root-relative path to another.
func heldNote(from, to string) string {
	return "held back new git metadata: " + from + " → " + to + " (rename it back to restore)"
}

// heldWarn spells holdGitMachinery's warning for an entry at rel it could not
// set aside, for the reason why.
func heldWarn(rel, why string) string {
	return "WARNING: could not set aside new git metadata at " + rel + " (" + why +
		"); do not run git in that directory until you have checked it"
}

// heldMoved spells heldWarn's form for an entry, or a folder above it, that
// was moved or replaced during the run.
func heldMoved(rel, why string) string {
	return heldWarn(rel+" (it was moved or replaced during the run)", why)
}

// checkWarn spells holdGitMachinery's warning for a directory at rel the
// after-walk could not check, for the reason why.
func checkWarn(rel, why string) string {
	return "WARNING: could not check " + rel + " for new git metadata (" + why +
		"); do not run git in it or below it until you have checked it"
}

// gitDirAt is the git dir real git finds from root/rel, relative to root, or
// "" when it finds none. GIT_CEILING_DIRECTORIES, set to root by the caller,
// keeps it from climbing out of the workspace; any other git failure fails
// the test, so a refusal (dubious ownership, say) cannot pass as "none".
func gitDirAt(t *testing.T, root, rel string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = filepath.Join(root, rel)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return ""
		}
		t.Fatalf("git rev-parse in %q: %v\n%s", rel, err, stderr.String())
	}
	return relAll(t, root, []string{strings.TrimSpace(string(out))})[0]
}

// treeEntry is one entry of snapshotTree: its inode, its mode and, for a
// regular file or a symbolic link, its content or target.
type treeEntry struct {
	ino  uint64
	mode fs.FileMode
	data string
}

// snapshotTree records every entry under dir it can reach, by path, without
// following a symbolic link. What a directory's mode keeps it from reading
// is left out, as it is from both snapshots a row compares.
func snapshotTree(t *testing.T, dir string) map[string]treeEntry {
	t.Helper()
	m := make(map[string]treeEntry)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			err = func() error {
				fi, err := os.Lstat(p)
				if err != nil {
					return err
				}
				e := treeEntry{ino: fi.Sys().(*syscall.Stat_t).Ino, mode: fi.Mode()}
				switch {
				case fi.Mode().IsRegular():
					b, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					e.data = string(b)
				case fi.Mode()&fs.ModeSymlink != 0:
					if e.data, err = os.Readlink(p); err != nil {
						return err
					}
				}
				m[p] = e
				return nil
			}()
		}
		if errors.Is(err, fs.ErrPermission) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestHoldGitMachinery: each row builds a workspace (src/x.go plus setup),
// takes the before-walk, runs change as the sandboxed command would, diffs
// the after-walk (newGitMachinery), runs between and edit (what can happen
// before the hold), then holds what the diff found with run id r1. The
// after-walk and the hold open up locked directories through one ownerAccess
// each; the seams stand for another owner, a lower limit, a swap as a
// directory is opened up and a mode that cannot be put back. Every row
// checks the notes exactly, that every entry in the workspace it can reach
// keeps its inode, mode and content (nothing deleted, nothing overwritten, no
// mode left changed), and that nothing outside it changed, modes included.
// modes pins the exact mode of directories a row locked, outermost first,
// opening each up after its check so the next can be reached. Real git, kept
// inside the workspace by GIT_CEILING_DIRECTORIES, finds the before
// repositories before the hold (the control) and the after ones, or none,
// after it.
func TestHoldGitMachinery(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("these rows run real git: %v", err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "LC_ALL": "C",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	const store = `git init -q --bare store.git` // a baseline repository for a .git file or link to name
	taken := `: > src/.git.golem-held-r1`        // every name holdGitMachinery may try for src/.git
	for n := 2; n <= heldNameTries; n++ {
		taken += fmt.Sprintf(" && : > src/.git.golem-held-r1-%d", n)
	}
	// twin adds a second item with the held .git/hooks directory's identity,
	// spelled .git/Hooks, as a case-insensitive filesystem's walk gives it.
	twin := func(first bool) func(*testing.T, string, []heldItem) []heldItem {
		return func(t *testing.T, root string, items []heldItem) []heldItem {
			if len(items) != 1 {
				t.Fatalf("want one held item, got %+v", items)
			}
			tw := heldItem{Path: filepath.Join(root, ".git/Hooks"), ID: items[0].ID, Kind: heldRepoLayout,
				InnerHEADs: []string{filepath.Join(root, ".git/Hooks/HEAD")}}
			if first {
				return append([]heldItem{tw}, items...)
			}
			return append(items, tw)
		}
	}
	const hooksLayout = `mkdir -p .git/hooks/objects .git/hooks/refs && printf 'ref: refs/heads/main\n' > .git/hooks/HEAD && ` +
		`echo P > .git/hooks/pre-commit`
	for _, c := range []struct {
		name, setup, change string
		between             string                                          // shell, after the diff and before the hold
		edit                func(*testing.T, string, []heldItem) []heldItem // after between
		atRename            string                                          // shell, at the hold's first rename (heldGitBeforeRename)
		atOpenUp            string                                          // shell, as the first directory is opened up (ownerAccessBeforeChange)
		unlisted            map[string]string                               // directory: reason, added to the after-walk's
		notOwned            bool                                            // ownerAccessEuid stands for another user, from the after-walk on
		limit               int                                             // ownerAccessLimit, when not zero
		failRestore         bool                                            // the first mode put back (to 0644) fails (ownerAccessChmod)
		nonRoot             bool                                            // the row needs permission checks, which root skips
		want                []string
		exist, gone         []string
		modes               map[string]fs.FileMode // a locked directory's exact mode after the hold
		before, after       map[string]string      // directory: the git dir real git finds there, "" for none
	}{
		{name: "nothing new", change: `echo y > src/y.go && mkdir build && echo x > build/out`},
		{name: "new .git directory with a nested repository",
			change: `git init -q src && git init -q --bare src/.git/modules/sub`,
			want: []string{heldNote("src/.git/HEAD", "src/.git/HEAD.golem-held-r1"),
				heldNote("src/.git/modules/sub/HEAD", "src/.git/modules/sub/HEAD.golem-held-r1"),
				heldNote("src/.git", "src/.git.golem-held-r1")},
			exist:  []string{"src/.git.golem-held-r1/HEAD.golem-held-r1", "src/.git.golem-held-r1/modules/sub/HEAD.golem-held-r1"},
			gone:   []string{"src/.git"},
			before: map[string]string{"src": "src/.git", "src/.git/modules/sub": "src/.git/modules/sub"},
			after:  map[string]string{"src": "", "src/.git.golem-held-r1": "", "src/.git.golem-held-r1/modules/sub": ""}},
		{name: "new .git file", setup: store, change: `echo 'gitdir: ../store.git' > src/.git`,
			want:   []string{heldNote("src/.git", "src/.git.golem-held-r1")},
			before: map[string]string{"src": "store.git"}, after: map[string]string{"src": ""}},
		{name: "new .git symlink", setup: store, change: `ln -s ../store.git src/.git`,
			want:   []string{heldNote("src/.git", "src/.git.golem-held-r1")},
			exist:  []string{"src/.git.golem-held-r1"},
			before: map[string]string{"src": "store.git"}, after: map[string]string{"src": ""}},
		{name: "new hooks in an existing git dir", setup: `git init -q . && rm -rf .git/hooks`,
			change: `mkdir .git/hooks && echo P > .git/hooks/pre-commit`,
			want:   []string{heldNote(".git/hooks", ".git/hooks.golem-held-r1")},
			exist:  []string{".git/hooks.golem-held-r1/pre-commit"}, gone: []string{".git/hooks"}},
		{name: "new worktrees container", setup: `git init -q . && git commit -q --allow-empty -m i`,
			change: `git worktree add -q wt2`,
			want: []string{heldNote(".git/worktrees/wt2/HEAD", ".git/worktrees/wt2/HEAD.golem-held-r1"),
				heldNote(".git/worktrees", ".git/worktrees.golem-held-r1"), heldNote("wt2/.git", "wt2/.git.golem-held-r1")},
			before: map[string]string{"wt2": ".git/worktrees/wt2"},
			after:  map[string]string{"wt2": "", ".git/worktrees.golem-held-r1/wt2": ".git"}},
		// A repository layout keeps its directory and the user's files in it;
		// only its HEAD is set aside.
		{name: "new bare repository", change: `git init -q --bare bare.git`,
			want:  []string{heldNote("bare.git/HEAD", "bare.git/HEAD.golem-held-r1")},
			exist: []string{"bare.git/config", "bare.git/objects"}, gone: []string{"bare.git/HEAD"},
			before: map[string]string{"bare.git": "bare.git"}, after: map[string]string{"bare.git": ""}},
		{name: "workspace root as a layout", setup: `echo hi > README`, change: `git init -q --bare .`,
			want:  []string{heldNote("HEAD", "HEAD.golem-held-r1")},
			exist: []string{"README", "src/x.go", "config", "objects"}, gone: []string{"HEAD"},
			before: map[string]string{".": "."}, after: map[string]string{".": "", "src": ""}},
		{name: "new layout inside an existing .git", setup: `git init -q .`, change: `git init -q --bare .git/evil`,
			want:   []string{heldNote(".git/evil/HEAD", ".git/evil/HEAD.golem-held-r1")},
			before: map[string]string{".git/evil": ".git/evil"}, after: map[string]string{".git/evil": ".git"}},
		{name: "a clash with a file takes -2", setup: store + ` && echo keep > src/.git.golem-held-r1`,
			change:  `echo 'gitdir: ../store.git' > src/.git && git init -q --bare bare.git`,
			between: `echo keep > bare.git/HEAD.golem-held-r1`,
			want: []string{heldNote("bare.git/HEAD", "bare.git/HEAD.golem-held-r1-2"),
				heldNote("src/.git", "src/.git.golem-held-r1-2")},
			exist: []string{"src/.git.golem-held-r1", "bare.git/HEAD.golem-held-r1"},
			after: map[string]string{"src": "", "bare.git": ""}},
		// A directory renamed onto an empty one would replace it without
		// RENAME_NOREPLACE.
		{name: "a clash with an empty directory takes -2", setup: `mkdir src/.git.golem-held-r1`,
			change: `git init -q src`,
			want: []string{heldNote("src/.git/HEAD", "src/.git/HEAD.golem-held-r1"),
				heldNote("src/.git", "src/.git.golem-held-r1-2")},
			exist: []string{"src/.git.golem-held-r1", "src/.git.golem-held-r1-2/HEAD.golem-held-r1"},
			after: map[string]string{"src": ""}},
		{name: "every name taken", setup: taken, change: `echo 'gitdir: ../store.git' > src/.git`,
			want:  []string{heldWarn("src/.git", fmt.Sprintf("file exists (tried %d names)", heldNameTries))},
			exist: []string{"src/.git"}},
		// The failure skips only its own item: src/.git is still held.
		{name: "a parent swapped for a symlink before the hold", setup: store,
			change:  `mkdir -p a/sub && echo 'gitdir: ../../store.git' > a/sub/.git && echo 'gitdir: ../store.git' > src/.git`,
			between: `mv a a.moved && ln -s "$OUTSIDE" a`,
			want: []string{heldMoved("a/sub/.git", "open the folder holding it: not a directory"),
				heldNote("src/.git", "src/.git.golem-held-r1")},
			exist: []string{"a.moved/sub/.git"}, after: map[string]string{"src": ""}},
		// The parent is pinned by descriptor, so the renames land in the
		// directory the diff saw, now at a.moved, and nothing in OUTSIDE moves.
		{name: "a parent swapped for a symlink after it is pinned",
			change:   `mkdir a && git init -q a/sub`,
			atRename: `mv a a.moved && ln -s "$OUTSIDE" a`,
			want: []string{heldNote("a/sub/.git/HEAD", "a/sub/.git/HEAD.golem-held-r1"),
				heldNote("a/sub/.git", "a/sub/.git.golem-held-r1")},
			exist: []string{"a.moved/sub/.git.golem-held-r1/HEAD.golem-held-r1"}, gone: []string{"a.moved/sub/.git"},
			after: map[string]string{"a.moved/sub": ""}},
		{name: "an item replaced after the diff is left alone", setup: store,
			change:  `echo 'gitdir: ../store.git' > src/.git`,
			between: `mv src/.git src/.git.old && echo 'gitdir: ../store.git' > src/.git`,
			want:    []string{heldMoved("src/.git", "a different entry is there now")},
			exist:   []string{"src/.git", "src/.git.old"}},
		// ext4 can hand a link recreated with another target its old inode
		// number back; only the target tells them apart.
		{name: "a .git symlink with another target is left alone", setup: store,
			change: `ln -s ../store.git src/.git`,
			edit: func(t *testing.T, _ string, items []heldItem) []heldItem {
				if len(items) != 1 || items[0].Kind != heldDotGitLink {
					t.Fatalf("want one held .git symlink, got %+v", items)
				}
				items[0].ID.Target = "../elsewhere.git"
				return items
			},
			want:  []string{heldMoved("src/.git", "a different entry is there now")},
			exist: []string{"src/.git"}},
		{name: "a case-folded twin listed first", setup: `git init -q . && rm -rf .git/hooks`, change: hooksLayout,
			edit: twin(true),
			want: []string{heldNote(".git/hooks/HEAD", ".git/hooks/HEAD.golem-held-r1"),
				heldNote(".git/hooks", ".git/hooks.golem-held-r1")},
			exist: []string{".git/hooks.golem-held-r1/HEAD.golem-held-r1"}},
		{name: "a case-folded twin listed second", setup: `git init -q . && rm -rf .git/hooks`, change: hooksLayout,
			edit: twin(false),
			want: []string{heldNote(".git/hooks/HEAD", ".git/hooks/HEAD.golem-held-r1"),
				heldNote(".git/hooks", ".git/hooks.golem-held-r1")},
			exist: []string{".git/hooks.golem-held-r1/HEAD.golem-held-r1"}},
		// A file is never merged (it may be a hard link): its second spelling
		// finds nothing once the first is set aside, and that is not a failure.
		{name: "a case-folded twin of a .git file", setup: store, change: `echo 'gitdir: ../store.git' > src/.git`,
			edit: func(t *testing.T, root string, items []heldItem) []heldItem {
				if len(items) != 1 {
					t.Fatalf("want one held item, got %+v", items)
				}
				return append(items, heldItem{Path: filepath.Join(root, "src/.GIT"), ID: items[0].ID, Kind: heldDotGitFile})
			},
			want: []string{heldNote("src/.git", "src/.git.golem-held-r1")}},
		// Hard links share an identity but are two names: each is held.
		{name: "hard links to one .git file", setup: store,
			change: `mkdir a b && echo 'gitdir: ../store.git' > a/.git && ln a/.git b/.git`,
			want:   []string{heldNote("a/.git", "a/.git.golem-held-r1"), heldNote("b/.git", "b/.git.golem-held-r1")},
			before: map[string]string{"a": "store.git", "b": "store.git"}, after: map[string]string{"a": "", "b": ""}},
		{name: "a name with a newline is escaped", setup: store,
			change: `d="$(printf 'x\nheld back new git metadata: y')" && mkdir "$d" && echo 'gitdir: ../store.git' > "$d/.git"`,
			want: []string{heldNote(`"x\nheld back new git metadata: y/.git"`,
				`"x\nheld back new git metadata: y/.git.golem-held-r1"`)}},
		{name: "a name that is not UTF-8 is escaped", setup: store,
			change: `d="$(printf 'x\377')" && mkdir "$d" && echo 'gitdir: ../store.git' > "$d/.git"`,
			want:   []string{heldNote(`"x\xff/.git"`, `"x\xff/.git.golem-held-r1"`)}},
		{name: "a relative path is outside the workspace", setup: store, change: `echo 'gitdir: ../store.git' > src/.git`,
			edit: func(t *testing.T, _ string, items []heldItem) []heldItem {
				if len(items) != 1 {
					t.Fatalf("want one held item, got %+v", items)
				}
				items[0].Path = "src/.git"
				return items
			},
			want: []string{heldWarn("src/.git", "outside the workspace")}, exist: []string{"src/.git"}},
		// The missing-name excuse covers another spelling of a name already
		// set aside, not another hard link that went missing.
		{name: "a hard link moved away after the diff is reported", setup: store,
			change:  `mkdir a b && echo 'gitdir: ../store.git' > a/.git && ln a/.git b/.git`,
			between: `mv b/.git b/.git.moved`,
			want: []string{heldNote("a/.git", "a/.git.golem-held-r1"),
				heldMoved("b/.git", "no such file or directory")}},
		// A command can take write or search permission off its own
		// directories. Each is opened up for one more try and then given its
		// exact mode back.
		{name: "a directory without write permission is held", nonRoot: true,
			change: `git init -q sub && chmod 0555 sub/.git sub`,
			want: []string{heldNote("sub/.git/HEAD", "sub/.git/HEAD.golem-held-r1"),
				heldNote("sub/.git", "sub/.git.golem-held-r1")},
			modes:  map[string]fs.FileMode{"sub": 0o555, "sub/.git.golem-held-r1": 0o555},
			exist:  []string{"sub/.git.golem-held-r1/HEAD.golem-held-r1"},
			before: map[string]string{"sub": "sub/.git"}, after: map[string]string{"sub": ""}},
		// The setgid and sticky bits are part of the exact mode put back.
		{name: "a setgid directory without write permission keeps its mode", nonRoot: true,
			change: `git init -q sub && chmod 02555 sub`,
			want:   heldInitNotes("sub", "r1"),
			modes:  map[string]fs.FileMode{"sub": fs.ModeSetgid | 0o555},
			before: map[string]string{"sub": "sub/.git"}, after: map[string]string{"sub": ""}},
		{name: "a sticky directory without write permission keeps its mode", nonRoot: true,
			change: `git init -q sub && chmod 01555 sub`,
			want:   heldInitNotes("sub", "r1"),
			modes:  map[string]fs.FileMode{"sub": fs.ModeSticky | 0o555},
			before: map[string]string{"sub": "sub/.git"}, after: map[string]string{"sub": ""}},
		// The lock comes after the after-walk here, so the hold opens it up.
		{name: "a directory locked against search after the walk is held", nonRoot: true,
			change: `git init -q sub`, between: `chmod 0644 sub`,
			want: []string{heldNote("sub/.git/HEAD", "sub/.git/HEAD.golem-held-r1"),
				heldNote("sub/.git", "sub/.git.golem-held-r1")},
			modes: map[string]fs.FileMode{"sub": 0o644},
			exist: []string{"sub/.git.golem-held-r1/HEAD.golem-held-r1", "sub/.git.golem-held-r1/config"},
			gone:  []string{"sub/.git"}, after: map[string]string{"sub": ""}},
		{name: "a parent locked against search after the walk is held", nonRoot: true,
			change: `mkdir a && git init -q a/sub`, between: `chmod 0555 a/sub && chmod 0644 a`,
			want: []string{heldNote("a/sub/.git/HEAD", "a/sub/.git/HEAD.golem-held-r1"),
				heldNote("a/sub/.git", "a/sub/.git.golem-held-r1")},
			modes: map[string]fs.FileMode{"a": 0o644, "a/sub": 0o555},
			exist: []string{"a/sub/.git.golem-held-r1/HEAD.golem-held-r1"}, gone: []string{"a/sub/.git"},
			after: map[string]string{"a/sub": ""}},
		// The command locks its own directories before the after-walk, which
		// opens them up to list them, outermost first, and puts their modes
		// back; the hold opens them up again to rename.
		{name: "a directory the command locked against search is held", nonRoot: true,
			change: `git init -q sub && chmod 0644 sub`,
			want: []string{heldNote("sub/.git/HEAD", "sub/.git/HEAD.golem-held-r1"),
				heldNote("sub/.git", "sub/.git.golem-held-r1")},
			modes: map[string]fs.FileMode{"sub": 0o644},
			exist: []string{"sub/.git.golem-held-r1/HEAD.golem-held-r1"}, gone: []string{"sub/.git"},
			after: map[string]string{"sub": ""}},
		{name: "a directory the command locked against reading is held", nonRoot: true,
			change: `mkdir -p d/e && git init -q d/e && chmod 0311 d`,
			want: []string{heldNote("d/e/.git/HEAD", "d/e/.git/HEAD.golem-held-r1"),
				heldNote("d/e/.git", "d/e/.git.golem-held-r1")},
			modes: map[string]fs.FileMode{"d": 0o311},
			exist: []string{"d/e/.git.golem-held-r1/HEAD.golem-held-r1"}, gone: []string{"d/e/.git"},
			after: map[string]string{"d/e": ""}},
		{name: "two nested directories the command locked are held", nonRoot: true,
			change: `mkdir -p a/b && git init -q a/b/c && chmod 0 a/b a`,
			want: []string{heldNote("a/b/c/.git/HEAD", "a/b/c/.git/HEAD.golem-held-r1"),
				heldNote("a/b/c/.git", "a/b/c/.git.golem-held-r1")},
			modes: map[string]fs.FileMode{"a": 0, "a/b": 0},
			exist: []string{"a/b/c/.git.golem-held-r1/HEAD.golem-held-r1"}, gone: []string{"a/b/c/.git"},
			after: map[string]string{"a/b/c": ""}},
		// A directory swapped for a symlink to a locked OUTSIDE directory as
		// the after-walk is about to open it up is not opened up, and neither
		// is anything outside.
		{name: "a directory swapped before the walk opens it up gives a warning", nonRoot: true,
			setup:    `mkdir "$OUTSIDE/locked" && chmod 0 "$OUTSIDE/locked"`,
			change:   `git init -q d && chmod 0 d`,
			atOpenUp: `mv d d.moved && ln -s "$OUTSIDE/locked" d`,
			want:     []string{checkWarn("d", "it was moved or replaced during the run")},
			modes:    map[string]fs.FileMode{"d.moved": 0}},
		{name: "a locked directory another user owns gives a warning", nonRoot: true, notOwned: true,
			change: `git init -q sub && chmod 0 sub`,
			want:   []string{checkWarn("sub", "permission denied")},
			modes:  map[string]fs.FileMode{"sub": 0}},
		// Past the limit, a directory stays locked and gets the warning.
		{name: "a locked directory past the limit gives a warning", nonRoot: true, limit: 1,
			change: `git init -q a && git init -q b && chmod 0 a b`,
			want: []string{heldNote("a/.git/HEAD", "a/.git/HEAD.golem-held-r1"), heldNote("a/.git", "a/.git.golem-held-r1"),
				checkWarn("b", "too many locked directories to open up")},
			modes: map[string]fs.FileMode{"a": 0, "b": 0}, after: map[string]string{"a": ""}},
		// The after-walk's restore fails, so sub keeps u+x, and the warning
		// says so; the hold then needs to open nothing up.
		{name: "a mode that cannot be put back gives a warning", nonRoot: true, failRestore: true,
			change: `git init -q sub && chmod 0644 sub`,
			want: []string{heldNote("sub/.git/HEAD", "sub/.git/HEAD.golem-held-r1"), heldNote("sub/.git", "sub/.git.golem-held-r1"),
				"WARNING: could not restore the mode of sub to 0644 (input/output error); set it back by hand"},
			modes: map[string]fs.FileMode{"sub": 0o744}},
		// A directory is opened up only while the root still reaches it, so a
		// parent swapped for a symlink to OUTSIDE (whose sub is locked too) is
		// left as it is, and the warning stands; HEAD, renamed first, already
		// keeps git from the repository.
		{name: "a parent swapped for a symlink before its mode is changed", nonRoot: true,
			setup:    `chmod 0555 "$OUTSIDE/sub"`,
			change:   `mkdir a && git init -q a/sub && chmod 0555 a/sub`,
			atRename: `mv a a.moved && ln -s "$OUTSIDE" a`,
			want: []string{heldNote("a/sub/.git/HEAD", "a/sub/.git/HEAD.golem-held-r1"),
				heldMoved("a/sub/.git", "permission denied")},
			modes: map[string]fs.FileMode{"a.moved/sub": 0o555}, after: map[string]string{"a.moved/sub": ""}},
		// Only a directory the user owns is opened up. This row pins the
		// warning, and that only a taken name moves on to the next.
		{name: "a directory another user owns gives a warning", nonRoot: true, notOwned: true,
			change: `git init -q src && chmod 0555 src`,
			want: []string{heldNote("src/.git/HEAD", "src/.git/HEAD.golem-held-r1"),
				heldWarn("src/.git", "permission denied")},
			modes:  map[string]fs.FileMode{"src": 0o555},
			before: map[string]string{"src": "src/.git"}, after: map[string]string{"src": ""}},
		{name: "unlisted directories", unlisted: map[string]string{".": "permission denied", "locked": "permission denied",
			"a\tb": "input/output error"},
			want: []string{checkWarn(".", "permission denied"), checkWarn(`"a\tb"`, "input/output error"),
				checkWarn("locked", "permission denied")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			root, outside := canonGitTempDir(t), t.TempDir()
			restoreModes(t, root)
			restoreModes(t, outside)
			t.Setenv("GIT_CEILING_DIRECTORIES", root)
			t.Setenv("OUTSIDE", outside)
			runIn(t, outside, `git init -q sub`)
			runIn(t, root, `mkdir src && echo x > src/x.go`)
			runIn(t, root, c.setup)
			out := snapshotTree(t, outside)
			before, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			runIn(t, root, c.change)
			if c.notOwned {
				ownerAccessEuid = func() int { return os.Geteuid() + 1 }
				t.Cleanup(func() { ownerAccessEuid = os.Geteuid })
			}
			if c.limit != 0 {
				ownerAccessLimit = c.limit
				t.Cleanup(func() { ownerAccessLimit = 4096 })
			}
			if c.atOpenUp != "" {
				swapped := false
				ownerAccessBeforeChange = func(string) {
					if !swapped {
						swapped = true
						runIn(t, root, c.atOpenUp)
					}
				}
				t.Cleanup(func() { ownerAccessBeforeChange = nil })
			}
			if c.failRestore {
				failed := false
				ownerAccessChmod = func(p string, mode uint32) error {
					if mode == 0o644 && !failed {
						failed = true
						return unix.EIO
					}
					return unix.Chmod(p, mode)
				}
				t.Cleanup(func() { ownerAccessChmod = unix.Chmod })
			}
			after := inspectSandboxWorkspaceAfter(root)
			items := newGitMachinery(before, after)
			runIn(t, root, c.between)
			if c.edit != nil {
				items = c.edit(t, root, items)
			}
			for _, u := range slices.Sorted(maps.Keys(c.unlisted)) {
				p := filepath.Join(root, u)
				after.unlisted = append(after.unlisted, p)
				if after.unlistedWhy == nil {
					after.unlistedWhy = make(map[string]string)
				}
				after.unlistedWhy[p] = c.unlisted[u]
			}
			for at, want := range c.before {
				if got := gitDirAt(t, root, at); got != want {
					t.Fatalf("control: before the hold, git in %q finds %q, want %q", at, got, want)
				}
			}
			inside := snapshotTree(t, root)
			if c.atRename != "" {
				swapped := false
				heldGitBeforeRename = func(string) {
					if !swapped {
						swapped = true
						runIn(t, root, c.atRename)
					}
				}
				t.Cleanup(func() { heldGitBeforeRename = nil })
			}

			notes := holdGitMachinery(after, items, "r1")

			if !slices.Equal(notes, c.want) {
				t.Errorf("notes:\n got %q\nwant %q", notes, c.want)
			}
			now := make(map[uint64]treeEntry)
			for _, e := range snapshotTree(t, root) {
				now[e.ino] = e
			}
			for p, e := range inside {
				if n, ok := now[e.ino]; !ok || n != e {
					t.Errorf("%q was deleted, overwritten or left with another mode", relAll(t, root, []string{p})[0])
				}
			}
			if !maps.Equal(snapshotTree(t, outside), out) {
				t.Errorf("something outside the workspace changed")
			}
			for _, p := range slices.Sorted(maps.Keys(c.modes)) {
				fi, err := os.Lstat(filepath.Join(root, p))
				if err != nil {
					t.Fatal(err)
				}
				if got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != c.modes[p] {
					t.Errorf("mode of %q is %v, want %v", p, got, c.modes[p])
				}
				if err := os.Chmod(filepath.Join(root, p), 0o755); err != nil { // so the next path, and git, can reach in
					t.Fatal(err)
				}
			}
			for _, p := range c.exist {
				if _, err := os.Lstat(filepath.Join(root, p)); err != nil {
					t.Errorf("want %q: %v", p, err)
				}
			}
			for _, p := range c.gone {
				if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
					t.Errorf("want %q gone", p)
				}
			}
			for at, want := range c.after {
				if got := gitDirAt(t, root, at); got != want {
					t.Errorf("after the hold, git in %q finds %q, want %q", at, got, want)
				}
			}
		})
	}
}

// TestHoldGitMachineryMergesTwins: mergeHeldDirs lists a directory once,
// after the last item at or under any of its spellings, so an item the walk
// found inside it is held before the directory is renamed whole, whichever
// spelling sorts first. The merged item has the stronger kind and every
// HEAD; files, which may be hard links, are never merged.
func TestHoldGitMachineryMergesTwins(t *testing.T) {
	hooks := gitEntryID{Dev: 1, Ino: 10, Mode: fs.ModeDir}
	inner := gitEntryID{Dev: 1, Ino: 11, Mode: fs.ModeDir}
	file := gitEntryID{Dev: 1, Ino: 12}
	item := func(p string, id gitEntryID, k heldKind, heads ...string) heldItem {
		it := heldItem{Path: "/w/" + p, ID: id, Kind: k}
		for _, h := range heads {
			it.InnerHEADs = append(it.InnerHEADs, "/w/"+h)
		}
		return it
	}
	for _, c := range []struct {
		name     string
		in, want []heldItem
	}{
		{"the stored spelling, an item under it, then the name git looks up",
			[]heldItem{item(".git/Hooks", hooks, heldRepoLayout, ".git/Hooks/HEAD"),
				item(".git/Hooks/x/.git", inner, heldDotGitDir, ".git/Hooks/x/.git/HEAD"),
				item(".git/hooks", hooks, heldGitMetadata)},
			[]heldItem{item(".git/Hooks/x/.git", inner, heldDotGitDir, ".git/Hooks/x/.git/HEAD"),
				item(".git/hooks", hooks, heldGitMetadata, ".git/hooks/HEAD")}},
		// A Kelvin sign folds to k on a casefold directory, and its UTF-8 sorts
		// after k, so the stored spelling and the item under it come last.
		{"a stored spelling that sorts after the name git looks up",
			[]heldItem{item(".git/hooks", hooks, heldGitMetadata),
				item(".git/hooKs", hooks, heldRepoLayout, ".git/hooKs/HEAD"),
				item(".git/hooKs/x/.git", inner, heldDotGitDir, ".git/hooKs/x/.git/HEAD")},
			[]heldItem{item(".git/hooKs/x/.git", inner, heldDotGitDir, ".git/hooKs/x/.git/HEAD"),
				item(".git/hooks", hooks, heldGitMetadata, ".git/hooks/HEAD")}},
		{"the stronger kind first",
			[]heldItem{item(".git/hooks", hooks, heldGitMetadata, ".git/hooks/HEAD"),
				item(".git/Hooks", hooks, heldRepoLayout, ".git/Hooks/HEAD", ".git/Hooks/y/HEAD")},
			[]heldItem{item(".git/hooks", hooks, heldGitMetadata, ".git/hooks/HEAD", ".git/hooks/y/HEAD")}},
		{"hard links stay two items",
			[]heldItem{item("a/.git", file, heldDotGitFile), item("b/.git", file, heldDotGitFile)},
			[]heldItem{item("a/.git", file, heldDotGitFile), item("b/.git", file, heldDotGitFile)}},
		{"no twins",
			[]heldItem{item("bare.git", hooks, heldRepoLayout, "bare.git/HEAD"), item("src/.git", inner, heldDotGitDir)},
			[]heldItem{item("bare.git", hooks, heldRepoLayout, "bare.git/HEAD"), item("src/.git", inner, heldDotGitDir)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := mergeHeldDirs(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}
