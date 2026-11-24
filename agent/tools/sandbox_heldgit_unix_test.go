//go:build darwin || linux

package tools

import (
	"fmt"
	"io/fs"
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

// runIn runs snippet under /bin/sh with root as its working directory.
func runIn(t *testing.T, root, snippet string) {
	t.Helper()
	if snippet == "" {
		return
	}
	if out, err := exec.Command("/bin/sh", "-c", "cd \"$1\" && "+snippet, "sh", root).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", snippet, err, out)
	}
}

// restoreModes makes every directory under root listable again, so the
// temp-dir cleanup can remove what a row made unreadable.
func restoreModes(t *testing.T, root string) {
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+rwx", root).Run() })
}

// entryIDOf is p's identity as the walks record it.
func entryIDOf(t *testing.T, p string) gitEntryID {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	id := gitEntryID{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Mode: fi.Mode().Type()}
	if id.Mode&fs.ModeSymlink != 0 {
		if id.Target, err = os.Readlink(p); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// relAll spells each path relative to root.
func relAll(t *testing.T, root string, ps []string) []string {
	t.Helper()
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		r, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

const validHEADBody = `printf 'ref: refs/heads/main\n'`

// recreateWithReusedInode replaces the symlink at rel with one to target that
// has the same inode number, as a command can by retrying: each attempt that
// gets another inode is set aside so the next draws a fresh one. It skips the
// test when the filesystem never reuses the inode (APFS does not).
func recreateWithReusedInode(rel, target string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		t.Helper()
		p := filepath.Join(root, rel)
		spare := filepath.Join(root, "spare")
		if err := os.Mkdir(spare, 0o755); err != nil {
			t.Fatal(err)
		}
		old := entryIDOf(t, p).Ino
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		for i := range 256 {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			if entryIDOf(t, p).Ino == old {
				return
			}
			if err := os.Rename(p, filepath.Join(spare, strconv.Itoa(i))); err != nil {
				t.Fatal(err)
			}
		}
		t.Skip("the filesystem did not reuse the inode")
	}
}

// hardLinkNoFollow gives the entry at rel, a symbolic link included, a second
// name at to (linkat without AT_SYMLINK_FOLLOW), making to's directory first.
// It is the portable form of GNU ln -P, which BusyBox lacks.
func hardLinkNoFollow(rel, to string) func(t *testing.T, root string) {
	return func(t *testing.T, root string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, to)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Linkat(unix.AT_FDCWD, filepath.Join(root, rel), unix.AT_FDCWD, filepath.Join(root, to), 0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNewGitMachinery: each row builds the git fixture, runs setup, takes the
// before-walk, runs change, takes the after-walk, and diffs the two. Only the
// topmost new git machinery is held, each item once, with the HEADs of the
// new repository layouts at or under it; moved entries, ordinary files and a
// HEAD git would reject are not held. The after-walk may open up no
// directory here (ownerAccessEuid stands for another user), so the unlisted
// row pins its best-effort path; TestHoldGitMachinery covers opening up.
func TestNewGitMachinery(t *testing.T) {
	ownerAccessEuid = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { ownerAccessEuid = os.Geteuid })
	type held struct {
		rel   string
		kind  heldKind
		heads []string
	}
	const link = `mkdir link && ln -s ../nested/.git link/.git` // a baseline .git symlink
	for _, c := range []struct {
		name, setup, change string
		changeFn            func(t *testing.T, root string) // runs after change
		want                []held
		unlisted            []string
		nonRoot             bool // the row makes a directory unreadable; root ignores that
	}{
		{name: "unchanged"},
		{name: "ordinary files and folders",
			change: `mkdir -p src/pkg build && echo x > src/pkg/y.go && echo x > README && mv src/x.go src/z.go`},
		{name: "new .git directory",
			change: `mkdir -p src/.git/objects src/.git/refs/heads src/.git/hooks src/.git/modules/sub/objects src/.git/modules/sub/refs && ` +
				validHEADBody + ` > src/.git/HEAD && ` + validHEADBody + ` > src/.git/modules/sub/HEAD && ` +
				`echo P > src/.git/hooks/pre-commit && echo '[core]' > src/.git/config`,
			want: []held{{"src/.git", heldDotGitDir, []string{"src/.git/HEAD", "src/.git/modules/sub/HEAD"}}}},
		{name: "new .git file", change: `echo 'gitdir: ../evil' > src/.git`,
			want: []held{{"src/.git", heldDotGitFile, nil}}},
		{name: "new .git symlink", change: `ln -s ../nested/.git src/.git`,
			want: []held{{"src/.git", heldDotGitLink, nil}}},
		{name: "new .git in an existing .git", change: `mkdir -p .git/x/.git/hooks`,
			want: []held{{".git/x/.git", heldDotGitDir, nil}}},
		{name: "new hooks in an existing git dir", setup: `rm -rf .git/hooks`,
			change: `mkdir .git/hooks && echo P > .git/hooks/pre-commit`,
			want:   []held{{".git/hooks", heldGitMetadata, nil}}},
		{name: "new config in an existing git dir", setup: `rm nested/.git/config`,
			change: `echo '[core]' > nested/.git/config`,
			want:   []held{{"nested/.git/config", heldGitMetadata, nil}}},
		{name: "new commondir in a submodule git dir", change: `echo ../evil > .git/modules/m/commondir`,
			want: []held{{".git/modules/m/commondir", heldGitMetadata, nil}}},
		{name: "new worktrees container", setup: `rm -rf .git/worktrees`,
			change: `mkdir -p .git/worktrees/wt2 && ` + validHEADBody + ` > .git/worktrees/wt2/HEAD && ` +
				`echo ../.. > .git/worktrees/wt2/commondir && echo /x > .git/worktrees/wt2/gitdir`,
			want: []held{{".git/worktrees", heldGitContainer, []string{".git/worktrees/wt2/HEAD"}}}},
		{name: "bare layout with a valid HEAD",
			change: `mkdir -p bare.git/objects bare.git/refs/heads bare.git/hooks && ` + validHEADBody + ` > bare.git/HEAD`,
			want:   []held{{"bare.git", heldRepoLayout, []string{"bare.git/HEAD"}}}},
		{name: "plain-text HEAD with objects and refs",
			change: `mkdir -p notes/objects notes/refs && echo 'remember the milk' > notes/HEAD`},
		{name: "HEAD plus commondir",
			change: `mkdir adm && printf '%064d\n' 0 > adm/HEAD && echo ../.git > adm/commondir`,
			want:   []held{{"adm", heldRepoLayout, []string{"adm/HEAD"}}}},
		{name: "moved checkout and a new .git at its old path",
			change: `mv nested n2 && mkdir -p nested/.git/objects nested/.git/refs && ` + validHEADBody + ` > nested/.git/HEAD`,
			want:   []held{{"nested/.git", heldDotGitDir, []string{"nested/.git/HEAD"}}}},
		{name: "moved bare repository and a new one at its old path",
			setup: `mkdir -p bare.git/objects bare.git/refs && ` + validHEADBody + ` > bare.git/HEAD`,
			change: `mv bare.git b2.git && mkdir -p bare.git/objects bare.git/refs && ` +
				validHEADBody + ` > bare.git/HEAD`,
			want: []held{{"bare.git", heldRepoLayout, []string{"bare.git/HEAD"}}}},
		// A layout is held by its HEADs alone, so it hides nothing under it:
		// the moved-in repository's new commondir is its own item, and the
		// repository's own HEAD, a baseline one, is left alone.
		{name: "new layout around a moved repository",
			setup: `mkdir nested/.git/refs`, // nested/.git is now a layout too
			change: `mkdir -p pack/objects pack/refs && ` + validHEADBody + ` > pack/HEAD && mv nested pack/nested && ` +
				`echo ../x > pack/nested/.git/commondir`,
			want: []held{{"pack", heldRepoLayout, []string{"pack/HEAD"}},
				{"pack/nested/.git/commondir", heldGitMetadata, nil}}},
		{name: "items inside a new layout are held on their own",
			change: `mkdir -p pack/objects pack/refs pack/inner/objects pack/inner/refs pack/src/.git/hooks && ` +
				validHEADBody + ` > pack/HEAD && ` + validHEADBody + ` > pack/inner/HEAD`,
			want: []held{{"pack", heldRepoLayout, []string{"pack/HEAD"}},
				{"pack/inner", heldRepoLayout, []string{"pack/inner/HEAD"}}, {"pack/src/.git", heldDotGitDir, nil}}},
		{name: "workspace root as a layout",
			change: `mkdir objects refs && ` + validHEADBody + ` > HEAD && echo 'gitdir: ../evil' > src/.git`,
			want:   []held{{".", heldRepoLayout, []string{"HEAD"}}, {"src/.git", heldDotGitFile, nil}}},
		// A new metadata entry or container is renamed whole even when it is
		// also a layout, so nothing planted beside its HEAD stays live.
		{name: "new hooks that is also a layout", setup: `rm -rf .git/hooks`,
			change: `mkdir -p .git/hooks/objects .git/hooks/refs && ` + validHEADBody + ` > .git/hooks/HEAD && ` +
				`printf '#!/bin/sh\n' > .git/hooks/pre-commit && chmod 755 .git/hooks/pre-commit`,
			want: []held{{".git/hooks", heldGitMetadata, []string{".git/hooks/HEAD"}}}},
		{name: "new info that is also a layout", setup: `rm -rf .git/info`,
			change: `mkdir -p .git/info/objects .git/info/refs && ` + validHEADBody + ` > .git/info/HEAD && ` +
				`echo x > .git/info/exclude`,
			want: []held{{".git/info", heldGitMetadata, []string{".git/info/HEAD"}}}},
		{name: "new worktrees container that is also a layout", setup: `rm -rf .git/worktrees`,
			change: `mkdir -p .git/worktrees/objects .git/worktrees/refs .git/worktrees/wt2 && ` +
				validHEADBody + ` > .git/worktrees/HEAD && ` + validHEADBody + ` > .git/worktrees/wt2/HEAD && ` +
				`echo ../.. > .git/worktrees/wt2/commondir && echo /x > .git/worktrees/wt2/gitdir`,
			want: []held{{".git/worktrees", heldGitContainer, []string{".git/worktrees/HEAD", ".git/worktrees/wt2/HEAD"}}}},
		// .git/modules/.git holds only directories, so it is a .git and an
		// intermediate; its new hooks and modules, each with a HEAD, are
		// metadata and a container as well as git dirs, and held whole.
		{name: "new metadata and container that are also git dirs", setup: `mkdir -p .git/modules/.git/keep`,
			change: `mkdir -p .git/modules/.git/hooks .git/modules/.git/modules/objects .git/modules/.git/modules/refs && ` +
				validHEADBody + ` > .git/modules/.git/hooks/HEAD && echo P > .git/modules/.git/hooks/pre-commit && ` +
				validHEADBody + ` > .git/modules/.git/modules/HEAD`,
			want: []held{{".git/modules/.git/hooks", heldGitMetadata, nil},
				{".git/modules/.git/modules", heldGitContainer, []string{".git/modules/.git/modules/HEAD"}}}},
		// At spawn a/sub/hooks is only a layout, which nothing mounts. The
		// command turns a into an intermediate and a/sub into a git dir, then
		// replaces a/sub/hooks: now metadata at a baseline path under the
		// unchanged .git. It was never mounted, so it is not a host-side change.
		{name: "baseline layout replaced as metadata",
			setup: `mkdir -p .git/modules/a/objects .git/modules/a/refs .git/modules/a/sub/hooks/objects ` +
				`.git/modules/a/sub/hooks/refs && ` + validHEADBody + ` > .git/modules/a/HEAD && ` +
				validHEADBody + ` > .git/modules/a/sub/hooks/HEAD`,
			change: `rm .git/modules/a/HEAD && ` + validHEADBody + ` > .git/modules/a/sub/HEAD && ` +
				`mkdir .git/modules/a/sub/hooks.new && echo P > .git/modules/a/sub/hooks.new/pre-commit && ` +
				`rm -rf .git/modules/a/sub/hooks && mv .git/modules/a/sub/hooks.new .git/modules/a/sub/hooks`,
			want: []held{{".git/modules/a/sub/hooks", heldGitMetadata, nil}}},
		{name: "new layout inside an existing .git",
			change: `mkdir -p .git/evil/objects .git/evil/refs .git/evil/hooks && ` + validHEADBody + ` > .git/evil/HEAD`,
			want:   []held{{".git/evil", heldRepoLayout, []string{".git/evil/HEAD"}}}},
		{name: "second name for an existing .git symlink", setup: link,
			changeFn: hardLinkNoFollow("link/.git", "other/.git"),
			want:     []held{{"other/.git", heldDotGitLink, nil}}},
		{name: "new .GIT directory", change: `mkdir -p src/.GIT/hooks`,
			want: []held{{"src/.GIT", heldDotGitDir, nil}}},
		{name: "baseline .git symlink replaced by a .git directory", setup: link,
			change: `rm link/.git && mkdir -p link/.git/hooks`,
			want:   []held{{"link/.git", heldDotGitDir, nil}}},
		{name: "baseline .git symlink replaced by a .git file", setup: link,
			change: `rm link/.git && echo 'gitdir: ../evil' > link/.git`,
			want:   []held{{"link/.git", heldDotGitFile, nil}}},
		// The same path, type and (reused) inode, but another target.
		{name: "baseline .git symlink recreated with another target", setup: link,
			changeFn: recreateWithReusedInode("link/.git", "../evil"),
			want:     []held{{"link/.git", heldDotGitLink, nil}}},
		// A baseline identity in another kind's slot is not a move.
		{name: "baseline .git symlink renamed into an absent commondir", setup: link,
			change: `mv link/.git nested/.git/commondir`,
			want:   []held{{"nested/.git/commondir", heldGitMetadata, nil}}},
		{name: "bare repository renamed to a .git",
			setup: `mkdir -p bare.git/objects bare.git/refs bare.git/hooks && ` + validHEADBody + ` > bare.git/HEAD && ` +
				`echo '[core]' > bare.git/config`,
			change: `mkdir X && mv bare.git X/.git`,
			want:   []held{{"X/.git", heldDotGitDir, []string{"X/.git/HEAD"}}}},
		// Host git rewrites config by lock and rename: a new inode at the same
		// path, in a .git that is still the baseline one.
		{name: "host rewrite of a baseline config",
			change: `cp .git/config .git/config.lock && echo '[x]' >> .git/config.lock && mv .git/config.lock .git/config`},
		{name: "planted .git with config at a moved checkout's path",
			change: `mv nested n2 && mkdir -p nested/.git && echo '[core]' > nested/.git/config`,
			want:   []held{{"nested/.git", heldDotGitDir, nil}}},
		// nested/.git is now the other checkout's (moved, not held), so a new
		// config at the baseline path nested/.git/config is held. It is written
		// beside and renamed over, so it cannot reuse a freed inode.
		{name: "swapped checkouts and a new config",
			setup: `mkdir other && cp -R nested/.git other/.git`,
			change: `mv nested t && mv other nested && mv t other && echo '[core]' > nested/.git/config.new && ` +
				`mv nested/.git/config.new nested/.git/config`,
			want: []held{{"nested/.git/config", heldGitMetadata, nil}}},
		{name: "unlisted new directory containing .git", nonRoot: true,
			change: `mkdir -p hidden/.git/objects hidden/.git/refs && ` + validHEADBody + ` > hidden/.git/HEAD && chmod 300 hidden`,
			want:   []held{{"hidden/.git", heldDotGitDir, []string{"hidden/.git/HEAD"}}}, unlisted: []string{"hidden"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			root := canonGitTempDir(t)
			restoreModes(t, root)
			writeGitFixture(t, root)
			runIn(t, root, c.setup)
			before, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			runIn(t, root, c.change)
			if c.changeFn != nil {
				c.changeFn(t, root)
			}
			after := inspectSandboxWorkspaceAfter(root)
			got := newGitMachinery(before, after)
			var gotHeld []held
			for _, h := range got {
				r := relAll(t, root, []string{h.Path})[0]
				var heads []string
				if len(h.InnerHEADs) > 0 {
					heads = relAll(t, root, h.InnerHEADs)
				}
				gotHeld = append(gotHeld, held{r, h.Kind, heads})
				if want := entryIDOf(t, h.Path); h.ID != want {
					t.Errorf("%s: ID %+v, want its current identity %+v", r, h.ID, want)
				}
			}
			if !reflect.DeepEqual(gotHeld, c.want) {
				t.Errorf("held:\n got %+v\nwant %+v", gotHeld, c.want)
			}
			if u := relAll(t, root, after.unlisted); !slices.Equal(u, c.unlisted) {
				t.Errorf("unlisted = %q, want %q", u, c.unlisted)
			}
		})
	}
}

// TestNewGitMachineryValidHEAD: a HEAD counts as git's validate_headref
// accepts it, and anything git cannot read is not one.
func TestNewGitMachineryValidHEAD(t *testing.T) {
	hex40 := "0123456789abcdefABCDEF0123456789abcdef01"
	for _, c := range []struct {
		name string
		make func(p string) error
		want bool
	}{
		{"symbolic ref", writeBody("ref: refs/heads/main\n"), true},
		{"symbolic ref without a space", writeBody("ref:refs/heads/main"), true},
		{"symbolic ref after a tab", writeBody("ref:\trefs/heads/main\n"), true},
		{"symbolic ref after a vertical tab", writeBody("ref:\vrefs/heads/main\n"), false}, // not git's isspace
		{"symbolic ref with trailing junk", writeBody("ref: refs/heads/main junk\x01\n"), true},
		{"oversized, valid start", writeBody("ref: refs/heads/main\n" + strings.Repeat("x", 1<<20)), true},
		{"valid ref after 255 bytes", writeBody(strings.Repeat("x", 255) + "ref: refs/heads/main\n"), false},
		{"NUL before ref", writeBody("\x00ref: refs/heads/main\n"), false},
		{"SHA-1 object ID", writeBody(hex40 + "\n"), true},
		{"SHA-256 object ID", writeBody(hex40 + hex40[:24] + "\n"), true},
		{"symlink into refs", func(p string) error { return os.Symlink("refs/heads/main", p) }, true},
		{"symlink elsewhere", func(p string) error { return os.Symlink("../HEAD", p) }, false},
		{"ref outside refs", writeBody("ref: heads/main\n"), false},
		{"39 hex digits", writeBody(hex40[:39] + "\n"), false},
		{"plain text", writeBody("remember the milk\n"), false},
		{"empty", writeBody(""), false},
		{"directory", func(p string) error { return os.Mkdir(p, 0o755) }, false},
		{"fifo", func(p string) error { return syscall.Mkfifo(p, 0o644) }, false},
		{"absent", func(string) error { return nil }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "HEAD")
			if err := c.make(p); err != nil {
				t.Fatal(err)
			}
			if got := validHEAD(p); got != c.want {
				t.Errorf("validHEAD = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNewGitMachinerySynthetic runs the diff on hand-built layouts, for
// identities a test cannot forge on every filesystem and for lists a walk does
// not produce today.
func TestNewGitMachinerySynthetic(t *testing.T) {
	dir := func(ino uint64) gitEntryID { return gitEntryID{Dev: 1, Ino: ino, Mode: fs.ModeDir} }
	for _, c := range []struct {
		name          string
		before, after sandboxGitLayout
		want          []heldItem
	}{
		// A baseline bare repository's identity (inode reuse can forge it) in
		// a new .git/hooks that is also a layout, listed in both kinds: hooks
		// is metadata, the baseline a layout, so it is not a move, and it is
		// held whole.
		{name: "forged layout identity in a hooks slot",
			before: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, repoLayouts: []string{"/ws/bare.git"},
				layoutHEADs: []string{"/ws/bare.git/HEAD"},
				ids:         map[string]gitEntryID{"/ws/.git": dir(1), "/ws/bare.git": dir(7)}},
			after: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, metadata: []string{"/ws/.git/hooks"},
				innerLayouts: []string{"/ws/.git/hooks"}, layoutHEADs: []string{"/ws/.git/hooks/HEAD"},
				ids: map[string]gitEntryID{"/ws/.git": dir(1), "/ws/.git/hooks": dir(7)}},
			want: []heldItem{{Path: "/ws/.git/hooks", ID: dir(7), Kind: heldGitMetadata,
				InnerHEADs: []string{"/ws/.git/hooks/HEAD"}}}},
		// An identity recorded for a path in no kind list (a list added to ids
		// but not to entryKinds) gets no host-side exemption: the layout
		// replaced at its baseline path keeps its HEAD in the held hooks.
		{name: "entry with no kind",
			before: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"},
				ids: map[string]gitEntryID{"/ws/.git": dir(1), "/ws/.git/hooks/x": dir(50)}},
			after: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, metadata: []string{"/ws/.git/hooks"},
				layoutHEADs: []string{"/ws/.git/hooks/x/HEAD"},
				ids:         map[string]gitEntryID{"/ws/.git": dir(1), "/ws/.git/hooks": dir(60), "/ws/.git/hooks/x": dir(51)}},
			want: []heldItem{{Path: "/ws/.git/hooks", ID: dir(60), Kind: heldGitMetadata,
				InnerHEADs: []string{"/ws/.git/hooks/x/HEAD"}}}},
		// Nor is such an entry a move: two unknown kinds do not match.
		{name: "entry with no kind at another path",
			before: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"},
				ids: map[string]gitEntryID{"/ws/.git": dir(1), "/ws/y": dir(50)}},
			after: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, metadata: []string{"/ws/.git/hooks"},
				layoutHEADs: []string{"/ws/.git/hooks/x/HEAD"},
				ids:         map[string]gitEntryID{"/ws/.git": dir(1), "/ws/.git/hooks": dir(60), "/ws/.git/hooks/x": dir(50)}},
			want: []heldItem{{Path: "/ws/.git/hooks", ID: dir(60), Kind: heldGitMetadata,
				InnerHEADs: []string{"/ws/.git/hooks/x/HEAD"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := newGitMachinery(c.before, c.after); !reflect.DeepEqual(got, c.want) {
				t.Errorf("held:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestEntryKindPrecedence pins, for every pair of kinds, what one new path
// listed under both counts as (.git entry > metadata > container > git dir >
// layout) and how it is held: whole, hiding the new .git file below it, or,
// as a git dir, not at all, which leaves that file held on its own. A layout
// alone is held by its HEAD only (TestNewGitMachinery); it never wins a pair.
// A pair that cannot share a path says why, and the table names every pair.
// Metadata and containers share a path with a git dir under a .git that is
// also an intermediate (a .git in a modules container holding only
// directories): its hooks or modules child with a HEAD is both.
func TestEntryKindPrecedence(t *testing.T) {
	const p, child = "/ws/.git/p", "/ws/.git/p/x/.git"
	dir := gitEntryID{Dev: 1, Ino: 10, Mode: fs.ModeDir}
	file := gitEntryID{Dev: 1, Ino: 11}
	whole := func(k heldKind, heads ...string) []heldItem {
		return []heldItem{{Path: p, ID: dir, Kind: k, InnerHEADs: heads}}
	}
	const (
		oneType = "a path has one type: a directory, a regular file or a symlink"
		names   = "the names differ: .git, a metadata name, modules or worktrees"
	)
	name := map[heldKind]string{heldDotGitDir: "dotGitDir", heldDotGitFile: "gitFile", heldDotGitLink: "dotGitLink",
		heldGitMetadata: "metadata", heldGitContainer: "container", kindGitDir: "gitDir", heldRepoLayout: "layout"}
	rows := []struct {
		a, b  heldKind
		never string
		want  []heldItem
	}{
		{a: heldDotGitDir, b: heldDotGitFile, never: oneType},
		{a: heldDotGitDir, b: heldDotGitLink, never: oneType},
		{a: heldDotGitDir, b: heldGitMetadata, never: names},
		{a: heldDotGitDir, b: heldGitContainer, never: names},
		{a: heldDotGitDir, b: kindGitDir, want: whole(heldDotGitDir)}, // .git/modules/.git
		{a: heldDotGitDir, b: heldRepoLayout, want: whole(heldDotGitDir, p+"/HEAD")},
		{a: heldDotGitFile, b: heldDotGitLink, never: oneType},
		{a: heldDotGitFile, b: heldGitMetadata, never: names},
		{a: heldDotGitFile, b: heldGitContainer, never: oneType},
		{a: heldDotGitFile, b: kindGitDir, never: oneType},
		{a: heldDotGitFile, b: heldRepoLayout, never: oneType},
		{a: heldDotGitLink, b: heldGitMetadata, never: names},
		{a: heldDotGitLink, b: heldGitContainer, never: oneType},
		{a: heldDotGitLink, b: kindGitDir, never: oneType},
		{a: heldDotGitLink, b: heldRepoLayout, never: oneType},
		{a: heldGitMetadata, b: heldGitContainer, never: names},
		{a: heldGitMetadata, b: kindGitDir, want: whole(heldGitMetadata)},
		{a: heldGitMetadata, b: heldRepoLayout, want: whole(heldGitMetadata, p+"/HEAD")},
		{a: heldGitContainer, b: kindGitDir, want: whole(heldGitContainer)},
		{a: heldGitContainer, b: heldRepoLayout, want: whole(heldGitContainer, p+"/HEAD")},
		{a: kindGitDir, b: heldRepoLayout, want: []heldItem{{Path: child, ID: file, Kind: heldDotGitFile}}},
	}
	seen := map[[2]heldKind]bool{}
	for _, r := range rows {
		key := [2]heldKind{min(r.a, r.b), max(r.a, r.b)}
		if seen[key] || r.a == r.b {
			t.Fatalf("pair %s+%s listed twice or with itself", name[r.a], name[r.b])
		}
		seen[key] = true
		if r.never != "" {
			continue
		}
		t.Run(name[r.a]+"+"+name[r.b], func(t *testing.T) {
			before := sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"},
				ids: map[string]gitEntryID{"/ws/.git": {Dev: 1, Ino: 1, Mode: fs.ModeDir}}}
			after := sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, gitFiles: []string{child},
				ids: map[string]gitEntryID{"/ws/.git": {Dev: 1, Ino: 1, Mode: fs.ModeDir}, p: dir, child: file}}
			for _, k := range []heldKind{r.a, r.b} {
				switch k {
				case heldDotGitDir:
					after.dotGitDirs = append(after.dotGitDirs, p)
				case heldGitMetadata:
					after.metadata = append(after.metadata, p)
				case heldGitContainer:
					after.containers = append(after.containers, p)
				case kindGitDir:
					after.gitDirs = append(after.gitDirs, p)
				case heldRepoLayout:
					after.innerLayouts = append(after.innerLayouts, p)
					after.layoutHEADs = append(after.layoutHEADs, p+"/HEAD")
				}
			}
			if got := newGitMachinery(before, after); !reflect.DeepEqual(got, r.want) {
				t.Errorf("held:\n got %+v\nwant %+v", got, r.want)
			}
		})
	}
	kinds := []heldKind{heldDotGitDir, heldDotGitFile, heldDotGitLink, heldGitMetadata, heldGitContainer,
		kindGitDir, heldRepoLayout}
	for i, a := range kinds {
		for _, b := range kinds[i+1:] {
			if !seen[[2]heldKind{min(a, b), max(a, b)}] {
				t.Errorf("pair %s+%s is not in the table", name[a], name[b])
			}
		}
	}
}

// writeBody returns a maker that writes body to its path.
func writeBody(body string) func(string) error {
	return func(p string) error { return os.WriteFile(p, []byte(body), 0o644) }
}

// TestInspectSandboxWorkspaceAfterMatchesBefore: on an unchanged tree both
// walks record the same layout, so the diff holds nothing, and the before-walk
// records .git symlinks (with their targets), repository layouts outside .git
// directories (bare, and HEAD with commondir) and one inside .git, without
// adding them to any mounted list.
func TestInspectSandboxWorkspaceAfterMatchesBefore(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	runIn(t, root, `mkdir -p link bare.git/objects bare.git/refs/heads wtadmin .git/evil/objects .git/evil/refs && `+
		`ln -s ../nested/.git link/.git && `+validHEADBody+` > bare.git/HEAD && `+validHEADBody+` > .git/evil/HEAD && `+
		`printf '%040d\n' 0 > wtadmin/HEAD && echo ../.git > wtadmin/commondir`)
	before, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		field     string
		got, want []string
	}{
		{"dotGitLinks", before.dotGitLinks, joinAll(root, "link/.git")},
		{"repoLayouts", before.repoLayouts, joinAll(root, "bare.git", "wtadmin")},
		{"innerLayouts", before.innerLayouts, joinAll(root, ".git", ".git/evil", ".git/modules/m", ".git/worktrees/wt")},
		{"layoutHEADs", before.layoutHEADs, joinAll(root, ".git/HEAD", ".git/evil/HEAD", ".git/modules/m/HEAD",
			".git/worktrees/wt/HEAD", "bare.git/HEAD", "wtadmin/HEAD")},
		{"dotGitDirs", before.dotGitDirs, joinAll(root, ".git", "nested/.git")},
		{"gitFiles", before.gitFiles, joinAll(root, "libs/a/.git", "m/.git", "wt/.git")},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.field, c.got, c.want)
		}
	}
	// The layouts inside .git keep their stronger kinds; only .git/evil is a layout.
	kinds := entryKinds(before)
	for rel, want := range map[string]heldKind{".git": heldDotGitDir, ".git/modules/m": kindGitDir,
		".git/worktrees/wt": kindGitDir, ".git/evil": heldRepoLayout, "bare.git": heldRepoLayout} {
		if got := kinds[filepath.Join(root, rel)]; got != want {
			t.Errorf("kind of %s = %d, want %d", rel, got, want)
		}
	}
	if id := before.ids[filepath.Join(root, "link/.git")]; id.Target != "../nested/.git" {
		t.Errorf("link/.git target = %q, want ../nested/.git", id.Target)
	}
	for _, list := range [][]string{before.dotGitDirs, before.gitFiles, before.dotGitLinks, before.gitDirs,
		before.containers, before.metadata, before.repoLayouts, before.innerLayouts} {
		for _, p := range list {
			if id, ok := before.ids[p]; !ok || id != entryIDOf(t, p) {
				t.Errorf("ids[%s] = %+v, %v; want its identity", p, id, ok)
			}
		}
	}
	after := inspectSandboxWorkspaceAfter(root)
	// Every recorded identity has a kind, so none falls to the zero kind.
	for _, l := range []sandboxGitLayout{before, after} {
		kinds := entryKinds(l)
		for p := range l.ids {
			if _, ok := kinds[p]; !ok {
				t.Errorf("ids[%s] has no kind", p)
			}
		}
	}
	after.afterWalk = false
	if !reflect.DeepEqual(after, before) {
		t.Errorf("after-walk differs on an unchanged tree:\n got %+v\nwant %+v", after, before)
	}
	if got := newGitMachinery(before, inspectSandboxWorkspaceAfter(root)); len(got) != 0 {
		t.Errorf("unchanged tree held %+v", got)
	}
}

// TestInspectSandboxWorkspaceAfterBestEffort: where the before-walk fails
// closed, the after-walk records what it could not list, looks up .git and
// the layout names there by name, and keeps everything it found. The
// directories here may not be opened up (ownerAccessEuid stands for another
// user), so each stays unlisted.
func TestInspectSandboxWorkspaceAfterBestEffort(t *testing.T) {
	ownerAccessEuid = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { ownerAccessEuid = os.Geteuid })
	t.Run("root missing", func(t *testing.T) {
		root := filepath.Join(canonGitTempDir(t), "gone")
		got := inspectSandboxWorkspaceAfter(root)
		if !slices.Equal(got.unlisted, []string{root}) || len(got.ids) != 0 {
			t.Errorf("unlisted %q, ids %v; want only the root unlisted", got.unlisted, got.ids)
		}
	})
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for _, c := range []struct {
		name, setup, chmod string // chmod: the directory made unlistable (mode 0300)
		check              func(t *testing.T, root string, got sandboxGitLayout)
	}{
		{"root", "", ".", func(t *testing.T, root string, got sandboxGitLayout) {
			// The root's own .git is found by name and scanned.
			requireListed(t, "dotGitDirs", got.dotGitDirs, root, ".git", true)
			requireListed(t, "metadata", got.metadata, root, ".git/hooks", true)
			requireListed(t, "gitDirs", got.gitDirs, root, ".git/modules/m", true)
		}},
		{"modules container", "", ".git/modules", func(t *testing.T, root string, got sandboxGitLayout) {
			// Kept as a container (it exists), contents unknown.
			requireListed(t, "containers", got.containers, root, ".git/modules", true)
			requireListed(t, "gitDirs", got.gitDirs, root, ".git/modules/m", false)
			requireListed(t, "metadata", got.metadata, root, ".git/worktrees/wt/commondir", true)
		}},
		{"layout", `mkdir nested/.git/refs`, "nested/.git", func(t *testing.T, root string, got sandboxGitLayout) {
			// Its HEAD, objects and refs are looked up by name.
			requireListed(t, "layoutHEADs", got.layoutHEADs, root, "nested/.git/HEAD", true)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			restoreModes(t, root)
			writeGitFixture(t, root)
			runIn(t, root, c.setup)
			if err := os.Chmod(filepath.Join(root, c.chmod), 0o300); err != nil {
				t.Fatal(err)
			}
			got := inspectSandboxWorkspaceAfter(root)
			if want := joinAll(root, c.chmod); !slices.Equal(got.unlisted, want) {
				t.Errorf("unlisted = %q, want %q", got.unlisted, want)
			}
			c.check(t, root, got)
		})
	}
}

// syntheticMachinery returns an empty before-layout and an after-layout with
// n new entries, each in its own directory under root: n/2 .git directories
// and n/2 bare layouts, every one a layout with a HEAD, as a command that
// plants many cheaply would leave them.
func syntheticMachinery(root string, n int) (before, after sandboxGitLayout) {
	before = sandboxGitLayout{root: root, ids: map[string]gitEntryID{}}
	after = sandboxGitLayout{root: root, ids: make(map[string]gitEntryID, n)}
	for i := range n {
		dir := filepath.Join(root, fmt.Sprintf("l%07d", i))
		if i%2 == 0 {
			dir = filepath.Join(root, fmt.Sprintf("d%07d", i), ".git")
			after.dotGitDirs = append(after.dotGitDirs, dir)
		} else {
			after.repoLayouts = append(after.repoLayouts, dir)
		}
		after.layoutHEADs = append(after.layoutHEADs, filepath.Join(dir, "HEAD"))
		after.ids[dir] = gitEntryID{Dev: 1, Ino: uint64(i + 1), Mode: fs.ModeDir}
	}
	slices.Sort(after.layoutHEADs)
	return before, after
}

// BenchmarkNewGitMachinery times the diff over 10^4 and 10^5 new entries; its
// cost must grow with entries times path depth, not with their square.
func BenchmarkNewGitMachinery(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			before, after := syntheticMachinery("/ws", n)
			for b.Loop() {
				if got := newGitMachinery(before, after); len(got) != n {
					b.Fatalf("held %d, want %d", len(got), n)
				}
			}
		})
	}
}

// BenchmarkNewGitMachineryDeep times the diff over 10^4 new entries at the
// bottom of one directory chain d levels deep (paths of about 2d bytes): its
// cost must grow with entries times depth, not also with path length.
func BenchmarkNewGitMachineryDeep(b *testing.B) {
	for _, d := range []int{1, 500, 1000, 1900} {
		b.Run(fmt.Sprintf("depth=%d", d), func(b *testing.B) {
			before, after := syntheticMachinery("/ws"+strings.Repeat("/c", d), 10_000)
			for b.Loop() {
				newGitMachinery(before, after)
			}
		})
	}
}

// BenchmarkGitLayoutFinishDeep is BenchmarkNewGitMachineryDeep for finish, at
// absent paths.
func BenchmarkGitLayoutFinishDeep(b *testing.B) {
	for _, d := range []int{1, 500, 1000, 1900} {
		b.Run(fmt.Sprintf("depth=%d", d), func(b *testing.B) {
			_, after := syntheticMachinery(filepath.Join(b.TempDir(), "absent")+strings.Repeat("/c", d), 10_000)
			for b.Loop() {
				l := sandboxGitLayout{root: after.root, dotGitDirs: slices.Clone(after.dotGitDirs),
					layoutHEADs: slices.Clone(after.layoutHEADs)}
				l.finish()
			}
		})
	}
}

// BenchmarkGitLayoutFinish times finish (the before-walk's too) over 10^4 and
// 10^5 .git directories and as many layouts outside them, at absent paths so
// the stat of each is cheap.
func BenchmarkGitLayoutFinish(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			_, after := syntheticMachinery(filepath.Join(b.TempDir(), "absent"), 2*n)
			for b.Loop() {
				l := sandboxGitLayout{root: after.root, dotGitDirs: slices.Clone(after.dotGitDirs),
					layoutHEADs: slices.Clone(after.layoutHEADs)}
				l.finish()
				if len(l.repoLayouts) != n {
					b.Fatalf("repoLayouts %d, want %d", len(l.repoLayouts), n)
				}
			}
		})
	}
}
