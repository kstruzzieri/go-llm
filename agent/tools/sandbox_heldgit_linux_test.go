//go:build linux

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// zwj is U+200D ZERO WIDTH JOINER, which Linux casefold lookups ignore
// (kernels before 6.12).
const zwj = "\u200d"

// casefoldLookup stands for gitLookupPath on a casefold directory: when dir
// has no entry named name, a listed name that equals it once U+200D is
// dropped and case is folded is the one the lookup resolves to.
func casefoldLookup(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Lstat(p); err == nil {
		return p
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.EqualFold(strings.ReplaceAll(e.Name(), zwj, ""), name) {
			return filepath.Join(dir, e.Name())
		}
	}
	return p
}

// fakeFS stands for openDirProbe: every directory is on device 1 of type typ
// with flags, except those at or under a component named below, on device 2
// of type belowTyp without flags. It counts statfs and XFS geometry reads.
type fakeFS struct {
	typ                         int64
	flags                       uint32
	flagsErr, statfsErr         bool
	openErr, devErr             bool
	below                       string
	belowTyp                    int64
	wantStatfs, statfs          int
	geometryFlags               uint32
	geometryErr                 bool
	wantGeometry, geometryReads int
}

// open is openDirProbe for f.
func (f *fakeFS) open(dir string) (dirProbe, error) {
	if f.openErr {
		return nil, unix.EACCES
	}
	if f.below != "" && strings.Contains(dir+"/", "/"+f.below+"/") {
		return fakeProbe{fs: f, d: 2, t: f.belowTyp}, nil
	}
	return fakeProbe{fs: f, d: 1, t: f.typ, f: f.flags, geometry: f.geometryFlags}, nil
}

// fakeProbe is one directory of a fakeFS.
type fakeProbe struct {
	fs       *fakeFS
	d        uint64
	t        int64
	f        uint32
	geometry uint32
}

// dev is the fake device, or fails.
func (p fakeProbe) dev() (uint64, error) {
	if p.fs.devErr {
		return 0, unix.EIO
	}
	return p.d, nil
}

// fsType counts the read and gives the fake type, or fails.
func (p fakeProbe) fsType() (int64, error) {
	p.fs.statfs++
	if p.fs.statfsErr {
		return 0, unix.EIO
	}
	return p.t, nil
}

// flags gives the fake flags, or fails.
func (p fakeProbe) flags() (uint32, error) {
	if p.fs.flagsErr {
		return 0, unix.ENOTTY
	}
	return p.f, nil
}

// xfsGeometryFlags counts the ioctl and returns this device's geometry flags.
func (p fakeProbe) xfsGeometryFlags() (uint32, error) {
	p.fs.geometryReads++
	if p.fs.geometryErr {
		return 0, unix.ENOTTY
	}
	return p.geometry, nil
}

// close does nothing.
func (fakeProbe) close() {}

// heldNote spells holdGitMachinery's note for a rename from one
// root-relative path to another.
func heldNote(from, to string) string {
	return "held back new git metadata: " + from + " → " + to + " (rename it back to restore)"
}

// preHEADWarn spells holdGitMachinery's warning for a HEAD that was there
// before the run, renamed from one root-relative path to another because the
// command made its folder a repository.
func preHEADWarn(from, to string) string {
	return "WARNING: moved a HEAD that was there before the run: " + from + " → " + to +
		" (the command made its folder a repository; rename it back once you have checked the folder)"
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
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
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
					if errors.Is(err, fs.ErrPermission) {
						b = []byte("(unreadable)") // a row locked it; its inode and mode still count
					} else if err != nil {
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
	// containment: GITMETA-POSTRUN-HOLD
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
				InnerHEADs: []heldHEAD{{Path: filepath.Join(root, ".git/Hooks/HEAD"), ID: items[0].InnerHEADs[0].ID}}}
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
		link                func(*testing.T, string)                        // hard link created by the command
		between             string                                          // shell, after the diff and before the hold
		edit                func(*testing.T, string, []heldItem) []heldItem // after between
		atRename            string                                          // shell, at the hold's first rename (heldGitBeforeRename)
		atOpenUp            string                                          // shell, as the first directory is opened up (ownerAccessBeforeChange)
		unlisted            map[string]string                               // directory: reason, added to the after-walk's
		notOwned            bool                                            // ownerAccessEuid stands for another user, from the after-walk on
		limit               int                                             // ownerAccessLimit, when not zero
		failRestore         bool                                            // the first mode put back (to 0644) fails (ownerAccessChmod)
		nonRoot             bool                                            // the row needs permission checks, which root skips
		casefold            bool                                            // git's names resolve as on a casefold directory (casefoldLookup)
		fs                  *fakeFS                                         // with casefold, what the gate sees; nil: ext4 with the casefold flag
		withheldRename      string                                          // expected source of a rename whose path is withheld
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
		// is anything outside. Use ext4 without casefold to isolate the owner
		// check. A parent using named lookups also detects the change of type.
		{name: "a directory swapped before the walk opens it up gives a warning", nonRoot: true,
			casefold: true, fs: &fakeFS{typ: unix.EXT4_SUPER_MAGIC},
			setup:    `mkdir "$OUTSIDE/locked" && chmod 0 "$OUTSIDE/locked"`,
			change:   `git init -q d && chmod 0 d`,
			atOpenUp: `mv d d.moved && ln -s "$OUTSIDE/locked" d`,
			want:     []string{checkWarn("d", "it was moved or replaced during the run")},
			modes:    map[string]fs.FileMode{"d.moved": 0}},
		{name: "a locked directory another user owns gives a warning", nonRoot: true, notOwned: true,
			change: `git init -q sub && chmod 0 sub`,
			want:   []string{checkWarn("sub", "owned by another user")},
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
				heldWarn("src/.git", "owned by another user")},
			modes:  map[string]fs.FileMode{"src": 0o555},
			before: map[string]string{"src": "src/.git"}, after: map[string]string{"src": ""}},
		{name: "unlisted directories", unlisted: map[string]string{".": "permission denied", "locked": "permission denied",
			"a\tb": "input/output error"},
			want: []string{checkWarn(".", "permission denied"), checkWarn(`"a\tb"`, "input/output error"),
				checkWarn("locked", "permission denied")}},
		// A casefold directory resolves git's names to spellings with an
		// invisible code point (here U+200D); the check looks up git's names,
		// so it finds them and renames each through the lookup spelling.
		{name: "casefold .git directory", casefold: true,
			change: `mkdir -p "src/.g` + zwj + `it/objects" "src/.g` + zwj + `it/refs" "src/.g` + zwj + `it/hooks" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/HEAD" && echo P > "src/.g` + zwj + `it/hooks/pre-commit"`,
			want: []string{heldNote(noteName("src/.git/HEAD"), noteName("src/.git/HEAD.golem-held-r1")),
				heldNote(noteName("src/.git"), noteName("src/.git.golem-held-r1"))}},
		{name: "casefold bare layout", casefold: true,
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs X/hooks && ` + validHEADBody + ` > "X/H` + zwj + `EAD" && ` +
				`echo P > X/hooks/pre-commit`,
			want: []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		{name: "casefold .git directory with a casefold HEAD", casefold: true,
			change: `mkdir -p "src/.g` + zwj + `it/objects" "src/.g` + zwj + `it/refs" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/H` + zwj + `EAD"`,
			want: []string{heldNote(noteName("src/.git/HEAD"), noteName("src/.git/HEAD.golem-held-r1")),
				heldNote(noteName("src/.git"), noteName("src/.git.golem-held-r1"))}},
		{name: "casefold .git file", casefold: true, setup: store,
			change: `echo 'gitdir: ../store.git' > "src/.g` + zwj + `it"`,
			want:   []string{heldNote(noteName("src/.git"), noteName("src/.git.golem-held-r1"))}},
		{name: "casefold .git symlink", casefold: true, setup: store,
			change: `ln -s ../store.git "src/.g` + zwj + `it"`,
			want:   []string{heldNote(noteName("src/.git"), noteName("src/.git.golem-held-r1"))}},
		// The gate (mayFoldNames). A directory whose flags cannot be read is
		// looked up all the same.
		{name: "casefold bare layout when the flags cannot be read", casefold: true,
			fs:     &fakeFS{typ: unix.EXT4_SUPER_MAGIC, flagsErr: true},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		// In an ext4 directory without the flag a listing is exact, so the
		// walks do not look the names up; the same spellings stay ordinary.
		{name: "ext4 without the casefold flag, no lookup", casefold: true, fs: &fakeFS{typ: unix.EXT4_SUPER_MAGIC},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`},
		{name: "btrfs, no lookup", casefold: true, fs: &fakeFS{typ: unix.BTRFS_SUPER_MAGIC},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`},
		// The nested XFS device uses exact names, even when its parent uses ascii-ci.
		{name: "xfs ascii-ci", casefold: true, setup: "mkdir exact",
			fs:     &fakeFS{typ: unix.XFS_SUPER_MAGIC, geometryFlags: 0x1000, below: "exact", belowTyp: unix.XFS_SUPER_MAGIC, wantGeometry: 4},
			change: "touch src/.GIT exact/.GIT", want: []string{heldNote("src/.git", "src/.git.golem-held-r1")}},
		{name: "xfs without ascii-ci", casefold: true,
			fs:     &fakeFS{typ: unix.XFS_SUPER_MAGIC, geometryFlags: 0x80, wantGeometry: 2},
			change: "touch src/.GIT"},
		{name: "xfs geometry error", casefold: true,
			fs:     &fakeFS{typ: unix.XFS_SUPER_MAGIC, geometryErr: true, wantGeometry: 2},
			change: "touch src/.GIT", want: []string{heldNote("src/.git", "src/.git.golem-held-r1")}},
		// FAT and other file systems that compare names without the flag, an
		// unknown type and a type statfs cannot read are looked up.
		{name: "vfat without a flag", casefold: true, fs: &fakeFS{typ: unix.MSDOS_SUPER_MAGIC},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		{name: "an unknown file system type", casefold: true, fs: &fakeFS{typ: 0x12345678},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		{name: "the directory cannot be opened", casefold: true, fs: &fakeFS{typ: unix.XFS_SUPER_MAGIC, openErr: true},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		{name: "fstat fails", casefold: true, fs: &fakeFS{typ: unix.XFS_SUPER_MAGIC, devErr: true},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		{name: "statfs fails", casefold: true, fs: &fakeFS{typ: unix.XFS_SUPER_MAGIC, statfsErr: true},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			want:   []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		// A vfat mount at X inside an ext4 root without the flag: its st_dev
		// differs, so its type is read too and X is looked up, Y is not. The
		// before-walk meets device 1, the after-walk both: three statfs reads.
		{name: "vfat mounted below an ext4 root", casefold: true,
			fs: &fakeFS{typ: unix.EXT4_SUPER_MAGIC, below: "X", belowTyp: unix.MSDOS_SUPER_MAGIC, wantStatfs: 3},
			change: `mkdir -p "X/ob` + zwj + `jects" X/refs "Y/ob` + zwj + `jects" Y/refs && ` +
				validHEADBody + ` > "X/H` + zwj + `EAD" && ` + validHEADBody + ` > "Y/H` + zwj + `EAD"`,
			want: []string{heldNote(noteName("X/HEAD"), noteName("X/HEAD.golem-held-r1"))}},
		// The before-walk looks up git's names too, so the user's own
		// repository spelled another way is baseline: left alone, while new
		// metadata in it is held.
		{name: "casefold .git directory from before the run", casefold: true,
			setup: `mkdir -p "src/.g` + zwj + `it/objects" "src/.g` + zwj + `it/refs" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/HEAD" && echo '[core]' > "src/.g` + zwj + `it/config"`,
			change: `echo y > src/y.go`},
		{name: "new hooks in a casefold .git directory from before the run", casefold: true,
			setup: `mkdir -p "src/.g` + zwj + `it/objects" "src/.g` + zwj + `it/refs" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/HEAD" && echo '[core]' > "src/.g` + zwj + `it/config"`,
			change: `mkdir "src/.g` + zwj + `it/hooks" && echo P > "src/.g` + zwj + `it/hooks/pre-commit"`,
			want:   []string{heldNote(noteName("src/.git/hooks"), noteName("src/.git/hooks.golem-held-r1"))}},
		// Nothing mounts such a repository's config, so its replacement in
		// place under the unchanged .git is the command's, not the host's.
		{name: "config replaced in a casefold .git directory from before the run", casefold: true,
			setup: `mkdir -p "src/.g` + zwj + `it/objects" "src/.g` + zwj + `it/refs" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/HEAD" && echo '[core]' > "src/.g` + zwj + `it/config"`,
			change:         `echo '[core]' > t && mv t "src/.g` + zwj + `it/config"`,
			withheldRename: "src/.git/config",
			want:           []string{"WARNING: held back new git metadata at " + withheldPath + " (its name now ends in .golem-held-r1; rename it back to restore)"}},
		{name: "casefold bare layout from before the run", casefold: true,
			setup:  `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			change: `echo y > X/notes`},
		{name: "new repository in a casefold bare layout from before the run", casefold: true,
			setup:  `mkdir -p "X/ob` + zwj + `jects" X/refs && ` + validHEADBody + ` > "X/H` + zwj + `EAD"`,
			change: `mkdir -p X/sub/.git/hooks && echo P > X/sub/.git/hooks/pre-commit`,
			want:   []string{heldNote("X/sub/.git", "X/sub/.git.golem-held-r1")}},
		// The lookup renames only the entry it resolves: an ordinary file the
		// command gave a second name that resolves as .git keeps its own.
		{name: "a casefold .git hard-linked to an ordinary file", casefold: true,
			setup:  `echo hi > src/notes.txt`,
			change: `ln src/notes.txt src/.GIT`,
			want:   []string{heldNote("src/.git", "src/.git.golem-held-r1")},
			exist:  []string{"src/notes.txt"}},
		// A new symbolic link at a container name is held, never followed.
		{name: "new worktrees symlink", setup: `git init -q && rm -rf .git/worktrees && mkdir -p "$OUTSIDE/wts"`,
			change: `ln -s "$OUTSIDE/wts" .git/worktrees`,
			want:   []string{heldNote(".git/worktrees", ".git/worktrees.golem-held-r1")},
			gone:   []string{".git/worktrees"}},
		// A baseline checkout moved into a new .git keeps its baseline HEAD,
		// so its new metadata is held on its own, before the .git is renamed.
		{name: "new metadata in a baseline checkout moved into a new .git", setup: `git init -q nested`,
			change: `mkdir -p n/.git && mv nested n/.git/x && echo '[core]' > n/.git/x/.git/config.worktree`,
			want: []string{heldNote("n/.git/x/.git/config.worktree", "n/.git/x/.git/config.worktree.golem-held-r1"),
				heldNote("n/.git", "n/.git.golem-held-r1")},
			gone: []string{"n/.git.golem-held-r1/x/.git/config.worktree"}},
		// A folder git would not take for a repository before the run (its
		// objects and refs are placeholder files) that the command completes is
		// held; its HEAD was there before, so that rename is a WARNING.
		{name: "placeholder layout completed",
			setup:  `mkdir fx && printf 'ref: refs/heads/main\n' > fx/HEAD && : > fx/objects && : > fx/refs && chmod 755 fx/objects fx/refs`,
			change: `rm fx/objects fx/refs && mkdir fx/objects fx/refs && echo '[core]' > fx/config`,
			want:   []string{preHEADWarn("fx/HEAD", "fx/HEAD.golem-held-r1")}},
		{name: "moved baseline HEAD", setup: "mkdir original && " + validHEADBody + " > original/HEAD",
			change: "mv original moved && mkdir moved/objects moved/refs",
			want:   []string{preHEADWarn("moved/HEAD", "moved/HEAD.golem-held-r1")}},
		{name: "nested gitfile and link in held tree", setup: "mkdir -p old/.git",
			change: "mkdir -p new/.git && mv old new/.git/old && mkdir -p new/.git/old/a new/.git/old/b && touch new/.git/old/a/.git && ln -s nowhere new/.git/old/b/.git",
			want:   []string{heldNote("new/.git/old/a/.git", "new/.git/old/a/.git.golem-held-r1"), heldNote("new/.git/old/b/.git", "new/.git/old/b/.git.golem-held-r1"), heldNote("new/.git", "new/.git.golem-held-r1")}},
		{name: "casefold metadata and container", casefold: true, setup: "mkdir .git",
			change: "mkdir .git/hoo" + zwj + "ks && ln -s nowhere .git/wor" + zwj + "ktrees",
			want:   []string{heldNote(".git/hooks", ".git/hooks.golem-held-r1"), heldNote(".git/worktrees", ".git/worktrees.golem-held-r1")},
			gone:   []string{".git/hoo" + zwj + "ks", ".git/wor" + zwj + "ktrees"}, exist: []string{".git/hooks.golem-held-r1", ".git/worktrees.golem-held-r1"}},
		{name: "ordinary hard link preserves baseline git name", casefold: true, setup: "ln -s elsewhere src/.GIT",
			link: hardLinkNoFollow("src/.GIT", "src/ordinary"), exist: []string{"src/.GIT", "src/ordinary"}},
		{name: "one container move allowance", casefold: true, setup: "mkdir -p a/.git b/.git c/.git && ln -s elsewhere a/.git/WORKTREES",
			change: "mv a/.git/WORKTREES b/.git/WORKTREES", link: hardLinkNoFollow("b/.git/WORKTREES", "c/.git/WORKTREES"),
			want: []string{heldNote("c/.git/worktrees", "c/.git/worktrees.golem-held-r1")}, exist: []string{"b/.git/WORKTREES", "c/.git/worktrees.golem-held-r1"}, gone: []string{"c/.git/WORKTREES"}},
		{name: "a HEAD from before the run in a layout the command completes",
			setup:  `mkdir fx && printf 'ref: refs/heads/main\n' > fx/HEAD`,
			change: `mkdir fx/objects fx/refs`,
			want:   []string{preHEADWarn("fx/HEAD", "fx/HEAD.golem-held-r1")}},
		// A HEAD replaced after the walk is not the one the walk found.
		{name: "a HEAD replaced before the hold",
			change:  `mkdir -p bare/objects bare/refs && printf 'ref: refs/heads/main\n' > bare/HEAD`,
			between: `printf 'ref: refs/heads/other\n' > bare/H2 && mv bare/H2 bare/HEAD`,
			want:    []string{heldMoved("bare/HEAD", "a different entry is there now")},
			exist:   []string{"bare/HEAD"}},
		// One identity that was a .git file and a metadata name before cannot
		// pass as a moved .git file when the .git file stays where it was.
		{name: "a metadata name of a baseline .git file becomes a second .git file",
			setup:  `git init -q && ` + store + ` && mkdir a b && echo 'gitdir: ../store.git' > a/.git && ln a/.git .git/CONFIG`,
			change: `mv .git/CONFIG b/.git`,
			want:   []string{heldNote("b/.git", "b/.git.golem-held-r1")}},
		// A hold whose parent cannot be opened up says why, beside the
		// refusal: here the limit of directories to open up.
		{name: "a hold past the limit says so", nonRoot: true, limit: 1,
			change: `git init -q a && git init -q b && chmod 0555 a b`,
			want: []string{heldNote("a/.git/HEAD", "a/.git/HEAD.golem-held-r1"), heldNote("a/.git", "a/.git.golem-held-r1"),
				heldNote("b/.git/HEAD", "b/.git/HEAD.golem-held-r1"),
				heldWarn("b/.git", "permission denied; too many locked directories to open up")},
			modes: map[string]fs.FileMode{"a": 0o555, "b": 0o555}, after: map[string]string{"a": ""}},
		// A HEAD the after-walk cannot read may be one git reads once its mode
		// allows: the folder gets a warning rather than passing as no layout.
		{name: "an unreadable HEAD in a new layout", nonRoot: true,
			change: `mkdir -p L/objects L/refs && printf 'ref: refs/heads/main\n' > L/HEAD && chmod 0 L/HEAD`,
			want:   []string{checkWarn("L", "permission denied")}},
		// Each rename attempt rechecks the entry first, and the held name is
		// checked after: a swap just before an attempt renames nothing more,
		// and one inside it is reported.
		{name: "an entry replaced while it is renamed",
			change:   `mkdir -p src/.git/hooks`,
			atRename: `mv src/.git src/.git.x && mkdir -p src/.git/other`,
			want: []string{heldNote("src/.git", "src/.git.golem-held-r1"),
				heldMoved("src/.git", "a different entry is there now; what was renamed is not what was checked")},
			exist: []string{"src/.git.x/hooks"}},
		{name: "an entry replaced between rename attempts",
			change:   `mkdir -p src/.git/hooks && : > src/.git.golem-held-r1`,
			atRename: `mv src/.git src/.git.x && mkdir -p src/.git/other`,
			want:     []string{heldMoved("src/.git", "a different entry is there now")},
			exist:    []string{"src/.git/other", "src/.git.x/hooks"}},
		// On a case-sensitive file system the same names are ordinary ones.
		{name: "invisible code points on a case-sensitive file system",
			change: `mkdir -p "src/.g` + zwj + `it/ob` + zwj + `jects" "src/.g` + zwj + `it/refs" "src/.g` + zwj + `it/hooks" && ` +
				validHEADBody + ` > "src/.g` + zwj + `it/H` + zwj + `EAD" && echo P > "src/.g` + zwj + `it/hooks/pre-commit"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			if c.casefold {
				fake := c.fs
				if fake == nil {
					fake = &fakeFS{typ: unix.EXT4_SUPER_MAGIC, flags: 0x40000000}
				}
				lookup, probe := gitLookupPath, openDirProbe
				gitLookupPath, openDirProbe = casefoldLookup, fake.open
				component := holdComponent
				holdComponent = func(fd int, name string) string { return filepath.Base(casefoldLookup(procFd(fd), name)) }
				t.Cleanup(func() {
					gitLookupPath, openDirProbe, holdComponent = lookup, probe, component
					if fake.wantStatfs != 0 && fake.statfs != fake.wantStatfs {
						t.Errorf("statfs read %d times, want %d", fake.statfs, fake.wantStatfs)
					}
					if fake.geometryReads != fake.wantGeometry {
						t.Errorf("XFS geometry read %d times, want %d", fake.geometryReads, fake.wantGeometry)
					}
				})
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
			if c.link != nil {
				c.link(t, root)
			}
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

			expected := maps.Clone(inside)
			for _, note := range c.want {
				if c.withheldRename != "" && strings.HasPrefix(note, "WARNING: held back new git metadata at "+withheldPath) {
					note = heldNote(c.withheldRename, c.withheldRename+".golem-held-r1")
				}
				rename, found := strings.CutPrefix(note, "held back new git metadata: ")
				if !found {
					rename, found = strings.CutPrefix(note, "WARNING: moved a HEAD that was there before the run: ")
				}
				if !found {
					continue
				}
				from, dest, _ := strings.Cut(rename, " → ")
				dest, _, _ = strings.Cut(dest, " (")
				decode := func(v string) string {
					if s, err := strconv.Unquote(v); err == nil {
						return s
					}
					return v
				}
				src := root
				for _, component := range strings.Split(decode(from), "/") {
					if c.casefold {
						src = casefoldLookup(src, component)
					} else {
						src = filepath.Join(src, component)
					}
				}
				to := filepath.Join(filepath.Dir(src), filepath.Base(decode(dest)))
				for path, entry := range maps.Clone(expected) {
					if path == src || strings.HasPrefix(path, src+"/") {
						delete(expected, path)
						expected[to+strings.TrimPrefix(path, src)] = entry
					}
				}
			}

			notes := holdGitMachinery(after, items, "r1")

			if !slices.Equal(notes, c.want) {
				t.Errorf("notes:\n got %q\nwant %q", notes, c.want)
			}
			tree := snapshotTree(t, root)
			if c.atRename == "" && !maps.Equal(expected, tree) {
				t.Errorf("disposition differs from the expected rename of each path: got %v, want %v", tree, expected)
			}
			now := make(map[uint64]treeEntry)
			for _, e := range tree {
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
				if _, err := os.Lstat(filepath.Join(root, p)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("want %q gone, got %v", p, err)
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
	// containment: GITMETA-POSTRUN-HOLD
	hooks := gitEntryID{Dev: 1, Ino: 10, Mode: fs.ModeDir}
	inner := gitEntryID{Dev: 1, Ino: 11, Mode: fs.ModeDir}
	file := gitEntryID{Dev: 1, Ino: 12}
	item := func(p string, id gitEntryID, k heldKind, heads ...string) heldItem {
		it := heldItem{Path: "/w/" + p, ID: id, Kind: k}
		for _, h := range heads {
			it.InnerHEADs = append(it.InnerHEADs, heldHEAD{Path: "/w/" + h})
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
			if got := mergeHeldDirs(context.Background(), c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestLookupDirectoryKeys compares the full comparison key set to the successful git lookups.
func TestLookupDirectoryKeys(t *testing.T) {
	// containment: GITMETA-POSTRUN-HOLD
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir -p .GIT/HOOKS .GIT/WORKTREES .GIT/OBJECTS .GIT/REFS sub && "+validHEADBody+" > .GIT/head && touch .GIT/HOOKS/ordinary .GIT/config .GIT/CONFIG .GIT/config.extra && ln -s elsewhere sub/.GIT")
	hardLinkNoFollow("sub/.GIT", "sub/ordinary")(t, root)
	probe, lookup := openDirProbe, gitLookupPath
	openDirProbe, gitLookupPath = (&fakeFS{typ: unix.TMPFS_MAGIC, flags: 0x40000000}).open, casefoldLookup
	t.Cleanup(func() { openDirProbe, gitLookupPath = probe, lookup })
	before, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]gitEntryID)
	for key, stored := range map[string]string{".git": ".GIT", ".git/hooks": ".GIT/HOOKS", ".git/config": ".GIT/config", ".git/worktrees": ".GIT/WORKTREES", "sub/.git": "sub/.GIT"} {
		want[filepath.Join(root, key)] = entryIDOf(t, filepath.Join(root, stored))
	}
	for _, layout := range []sandboxGitLayout{*before.baseline, inspectSandboxWorkspaceAfter(root)} {
		if !maps.Equal(layout.ids, want) {
			t.Errorf("keys and identities %v, want %v", layout.ids, want)
		}
		if !maps.Equal(layout.headIDs, map[string]gitEntryID{filepath.Join(root, ".git/HEAD"): entryIDOf(t, filepath.Join(root, ".GIT/head"))}) {
			t.Errorf("HEAD keys %v", layout.headIDs)
		}
		if len(entryKinds(context.Background(), layout)) != len(want) {
			t.Errorf("unexpected git names %v", entryKinds(context.Background(), layout))
		}
	}
}

// TestCasefoldFlagsPerDirectory distinguishes siblings sharing the same filesystem.
func TestCasefoldFlagsPerDirectory(t *testing.T) {
	// containment: GITMETA-POSTRUN-HOLD
	for _, typ := range []int64{unix.EXT4_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC, unix.TMPFS_MAGIC} {
		f := &fakeFS{typ: typ}
		original := openDirProbe
		openDirProbe = func(dir string) (dirProbe, error) {
			var flags uint32
			if dir == "fold" {
				flags = 0x40000000
			}
			return fakeProbe{fs: f, d: 1, t: typ, f: flags}, nil
		}
		l := sandboxGitLayout{}
		exact, fold := l.mayFoldNames("exact"), l.mayFoldNames("fold")
		openDirProbe = original
		if exact || !fold || f.statfs != 1 {
			t.Errorf("type %x: exact=%v fold=%v statfs=%d", typ, exact, fold, f.statfs)
		}
	}
}

// TestLayoutUsesOriginalSearchMode pins classification during a temporary owner grant.
func TestLayoutUsesOriginalSearchMode(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir OBJECTS REFS && touch OBJECTS/file REFS/file && "+validHEADBody+" > head && chmod 0600 OBJECTS REFS")
	restoreModes(t, root)
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	pin, err := pinWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pin.Close() }()
	acc := newOwnerAccess(pin)
	defer acc.close()
	defer acc.restore()
	for _, name := range []string{"OBJECTS", "REFS"} {
		if _, err := acc.openUpPath(name); err != nil {
			t.Fatal(err)
		}
	}
	old := gitLookupPath
	gitLookupPath = casefoldLookup
	t.Cleanup(func() { gitLookupPath = old })
	l := sandboxGitLayout{root: root, access: acc, lookupDirs: map[string]bool{root: true}}
	l.noteLayout(root)
	if len(l.layoutHEADs) != 0 {
		t.Fatal("temporary search grant activated the layout")
	}
}

// TestHeldHEADMergedIdentity keeps the original observation when respelling a HEAD.
func TestHeldHEADMergedIdentity(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir hooks && touch hooks/HEAD hooks/next")
	dir := filepath.Join(root, "hooks")
	alias := filepath.Join(root, "HOOKS")
	head := filepath.Join(dir, "HEAD")
	items := []heldItem{{Path: alias, Kind: heldRepoLayout, ID: entryIDOf(t, dir), InnerHEADs: []heldHEAD{{Path: alias + "/HEAD", ID: entryIDOf(t, head)}}}, {Path: dir, Kind: heldGitMetadata, ID: entryIDOf(t, dir)}}
	if err := os.Rename(dir+"/next", head); err != nil {
		t.Fatal(err)
	}
	got := holdGitMachinery(sandboxGitLayout{root: root, screen: testScreen()}, items, "r")
	want := []string{heldMoved("hooks/HEAD", "a different entry is there now"), heldNote("hooks", "hooks.golem-held-r")}
	if !slices.Equal(got, want) {
		t.Fatalf("notes %q, want %q", got, want)
	}
	if _, err := os.Stat(dir + ".golem-held-r/HEAD"); err != nil {
		t.Fatal(err)
	}
}

// TestHoldWarningsAndContinuation checks individual failures and later renames.
func TestHoldWarningsAndContinuation(t *testing.T) {
	for _, name := range []string{"closed pin", "missing pin", "root rename", "pinned identity", "outside", "missing", "wrong type", "inaccessible"} {
		t.Run(name, func(t *testing.T) {
			root := canonGitTempDir(t)
			runIn(t, root, "mkdir -p item/inner && touch item/inner/HEAD item/later")
			pin, err := pinWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pin.Close() })
			layout := sandboxGitLayout{root: root, pin: pin, screen: testScreen()}
			dir := filepath.Join(root, "item")
			head := dir + "/inner/HEAD"
			later := dir + "/later"
			it := heldItem{Path: dir, ID: entryIDOf(t, dir), Kind: heldGitMetadata, InnerHEADs: []heldHEAD{{Path: head, ID: entryIDOf(t, head)}, {Path: later, ID: entryIDOf(t, later)}}}
			var warning string
			continues := false
			switch name {
			case "closed pin", "missing pin":
				if err := pin.Close(); err != nil {
					t.Fatal(err)
				}
				if name == "missing pin" {
					layout.pin = nil
				}
				warning = heldWarn("item", "open the workspace: bad file descriptor")
			case "root rename":
				it.Path = root
				warning = heldWarn(".", "the workspace root is never renamed")
			case "pinned identity":
				it.Path = root
				it.Kind = heldRepoLayout
				warning = heldMoved(".", "a different entry is there now")
			case "outside":
				it.InnerHEADs[0].Path = filepath.Join(root, "outside")
				warning = heldWarn("outside", "outside the item")
				continues = true
			case "missing", "wrong type":
				if err := os.Remove(head); err != nil {
					t.Fatal(err)
				}
				why := "no such file or directory"
				if name == "wrong type" {
					if err := os.Mkdir(head, 0755); err != nil {
						t.Fatal(err)
					}
					why = "a different entry is there now"
				}
				warning = heldMoved("item/inner/HEAD", why)
				continues = true
			case "inaccessible":
				runIn(t, root, "mv item/inner item/kept && touch item/inner")
				warning = heldMoved("item/inner/HEAD", "open the folder holding it: not a directory")
				continues = true
			}
			want := []string{warning}
			if continues {
				want = append(want, heldNote("item/later", "item/later.golem-held-r"), heldNote("item", "item.golem-held-r"))
			}
			var got []string
			reportGitHolds(context.Background(), layout, []heldItem{it}, "r", func(line string, _ bool) { got = append(got, line) })
			if !slices.Equal(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			at := dir
			if continues {
				at += ".golem-held-r"
				if _, err := os.Stat(at + "/later.golem-held-r"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(at); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestGitModeRestoration checks the final mode and diagnostic on each cleanup path.
func TestGitModeRestoration(t *testing.T) {
	for _, name := range []string{"walk panic", "hold panic", "hold inexact", "hold failure"} {
		t.Run(name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("requires ordinary owner permissions")
			}
			root := canonGitTempDir(t)
			runIn(t, root, "mkdir sub && ln -s nowhere sub/.git")
			restoreModes(t, root)
			l := inspectSandboxWorkspaceAfter(root)
			items := newGitMachinery(sandboxGitLayout{}, l)
			mode := os.FileMode(0555)
			call := func() []string { return holdGitMachinery(l, items, "r") }
			original := ownerAccessChmod
			switch name {
			case "walk panic":
				mode = 0400
				old := gitReadlink
				gitReadlink = func(string) (string, error) { panic("injected") }
				t.Cleanup(func() { gitReadlink = old })
				call = func() []string { return inspectSandboxWorkspaceAfter(root).warnings }
			case "hold panic":
				heldGitBeforeRename = func(string) {
					fi, err := os.Stat(root + "/sub")
					if err == nil && fi.Mode().Perm()&0200 != 0 {
						panic("injected")
					}
				}
				t.Cleanup(func() { heldGitBeforeRename = nil })
			default:
				ownerAccessChmod = func(p string, m uint32) error {
					if m == 0555 {
						if name == "hold failure" {
							return unix.EIO
						}
						return nil
					}
					return original(p, m)
				}
				t.Cleanup(func() { ownerAccessChmod = original })
			}
			if err := os.Chmod(root+"/sub", mode); err != nil {
				t.Fatal(err)
			}
			var notes []string
			panicked := func() (did bool) { defer func() { did = recover() == "injected" }(); notes = call(); return false }()
			expectPanic := strings.HasSuffix(name, "panic")
			if panicked != expectPanic {
				t.Fatalf("panic %v", panicked)
			}
			if !expectPanic {
				mode = 0755
				why := "it is 0755"
				if name == "hold failure" {
					why = "input/output error"
				}
				want := "WARNING: could not restore the mode of sub to 0555 (" + why + "); set it back by hand"
				if !slices.Contains(notes, want) {
					t.Errorf("notes %q, want %q", notes, want)
				}
			}
			fi, err := os.Stat(root + "/sub")
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != mode {
				t.Errorf("mode %o, want %o", fi.Mode().Perm(), mode)
			}
		})
	}
}

// holdGitMachinery collects every raw detail in rename order for the filesystem assertions.
func holdGitMachinery(after sandboxGitLayout, items []heldItem, runID string) (notes []string) {
	if after.pin == nil {
		pin, err := pinWorkspace(after.root)
		if err == nil {
			defer func() { _ = pin.Close() }()
			after.pin = pin
		}
	}
	reportGitHolds(context.Background(), after, items, runID, func(line string, _ bool) { notes = append(notes, line) })
	return notes
}

// TestHoldPermissionRetryChecksSource validates the source after a parent grant.
func TestHoldPermissionRetryChecksSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir sub && touch sub/.git sub/replacement")
	restoreModes(t, root)
	after := inspectSandboxWorkspaceAfter(root)
	items := newGitMachinery(sandboxGitLayout{}, after)
	old := ownerAccessChmod
	t.Cleanup(func() { ownerAccessChmod = old })
	ownerAccessChmod = func(p string, mode uint32) error {
		if err := old(p, mode); err != nil {
			return err
		}
		if mode == 0755 {
			return os.Rename(root+"/sub/replacement", root+"/sub/.git")
		}
		return nil
	}
	if err := os.Chmod(root+"/sub", 0555); err != nil {
		t.Fatal(err)
	}
	want := entryIDOf(t, root+"/sub/replacement")
	notes := holdGitMachinery(after, items, "r")
	if !slices.Equal(notes, []string{heldMoved("sub/.git", "a different entry is there now")}) || entryIDOf(t, root+"/sub/.git") != want {
		t.Fatalf("retry disposition, notes %q", notes)
	}
}

// TestOwnerAccessPreservesSpecialBits checks the mode and warning without requiring a group change.
func TestOwnerAccessPreservesSpecialBits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	for _, tc := range []struct {
		name    string
		mode    fs.FileMode
		warning bool
	}{
		{"listable empty directory", fs.ModeSetgid | 0400, false},
		{"setuid directory", fs.ModeSetuid, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			dir := filepath.Join(root, "sub")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			chmod := ownerAccessChmod
			t.Cleanup(func() { ownerAccessChmod = chmod })
			ownerAccessChmod = func(string, uint32) error {
				t.Error("attempted chmod")
				return unix.EPERM
			}
			l := inspectSandboxWorkspaceAfter(root)
			var want []string
			if tc.warning {
				want = []string{checkWarn("sub", "its special permission bits could not be restored")}
			}
			if notes := holdGitMachinery(l, nil, "r"); !slices.Equal(notes, want) {
				t.Errorf("notes %q, want %q", notes, want)
			}
			fi, err := os.Stat(dir)
			if err != nil || fi.Mode() != os.ModeDir|tc.mode {
				t.Errorf("expected directory mode %v, stat %v, error %v", tc.mode, fi, err)
			}
		})
	}
}

// TestOwnerAccessForeignSetgid refuses a grant even when chmod would silently clear setgid.
func TestOwnerAccessForeignSetgid(t *testing.T) {
	root := canonGitTempDir(t)
	t.Cleanup(func() { _ = os.Chmod(root, 0700) })
	if err := os.Chmod(root, fs.ModeSetgid|0500); err != nil {
		t.Fatal(err)
	}
	group, chmod := ownerAccessInGroup, ownerAccessChmod
	t.Cleanup(func() { ownerAccessInGroup, ownerAccessChmod = group, chmod })
	ownerAccessInGroup = func(uint32) bool { return false }
	ownerAccessChmod = func(string, uint32) error {
		t.Error("attempted chmod")
		return unix.EPERM
	}
	pin, err := pinWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pin.Close() }()
	acc := newOwnerAccess(pin)
	defer acc.close()
	err = acc.retry(acc.root, ".", 0300, func() error { return unix.EACCES })
	if !errors.Is(err, errAccessSpecial) {
		t.Fatalf("grant error %v", err)
	}
}

func TestHeldRenameRecoverySuffix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent string
		hidden bool
	}{
		{"shortened parent", strings.Repeat("d", noteNameMax+5), false},
		{"withheld parent", ".ssh", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			parent := filepath.Join(root, tc.parent)
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(parent, ".git")
			for _, path := range []string{entry, entry + ".golem-held-r1"} {
				if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			items := []heldItem{{Path: entry, ID: entryIDOf(t, entry), Kind: heldDotGitFile}}
			got := holdGitMachinery(sandboxGitLayout{root: root, screen: testScreen()}, items, "r1")
			source := noteName(filepath.Join(tc.parent, ".git"))
			prefix := "held back new git metadata at " + source
			if tc.hidden {
				prefix = "WARNING: held back new git metadata at " + withheldPath
			}
			const suffix = ".golem-held-r1-2"
			want := prefix + " (its name now ends in " + suffix + "; rename it back to restore)"
			if !slices.Equal(got, []string{want}) {
				t.Errorf("rename note %q, want %q", got, want)
			}
			if id := entryIDOf(t, entry+suffix); id != items[0].ID {
				t.Error("the reported suffix does not locate the original entry")
			}
		})
	}
}
