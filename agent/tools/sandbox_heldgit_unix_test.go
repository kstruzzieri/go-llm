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

// collectForCheck makes the before-walk record what the post-run check uses
// for the rest of the test, as it does on Linux, so the shared check code
// runs on every platform (postRunCollect).
func collectForCheck(t *testing.T) {
	was := postRunCollect
	postRunCollect = true
	t.Cleanup(func() { postRunCollect = was })
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

// TestGitEntryIdentityIncludesTarget compares two observations of a reused inode.
func TestGitEntryIdentityIncludesTarget(t *testing.T) {
	// containment: GITMETA-POSTRUN-DIFF
	const p = "/ws/sub/.git"
	layout := func(target string) sandboxGitLayout {
		return sandboxGitLayout{dotGitLinks: []string{p}, ids: map[string]gitEntryID{
			p: {Dev: 1, Ino: 3, Mode: fs.ModeSymlink, Target: target},
		}}
	}
	for _, tc := range []struct {
		target string
		count  int
	}{{"original", 0}, {"replacement", 1}} {
		if got := newGitMachinery(layout("original"), layout(tc.target)); len(got) != tc.count {
			t.Errorf("target %q: got %+v, want %d holds", tc.target, got, tc.count)
		}
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
// HEAD git would reject are left alone.
func TestNewGitMachinery(t *testing.T) {
	// containment: GITMETA-POSTRUN-DIFF
	collectForCheck(t)
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
		{name: "new worktrees symlink", setup: `rm -rf .git/worktrees`, change: `ln -s ../elsewhere .git/worktrees`,
			want: []held{{".git/worktrees", heldGitContainer, nil}}},
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
		// A baseline layout reclassified as metadata is ineligible for the host-side exception.
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
		{name: "baseline .git symlink replaced by a .git directory", setup: link,
			change: `rm link/.git && mkdir -p link/.git/hooks`,
			want:   []held{{"link/.git", heldDotGitDir, nil}}},
		{name: "baseline .git symlink replaced by a .git file", setup: link,
			change: `rm link/.git && echo 'gitdir: ../evil' > link/.git`,
			want:   []held{{"link/.git", heldDotGitFile, nil}}},
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
	} {
		t.Run(c.name, func(t *testing.T) {
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
					for _, hd := range h.InnerHEADs {
						heads = append(heads, relAll(t, root, []string{hd.Path})[0])
					}
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
		{"valid ref after 255 bytes", writeBody("ref:" + strings.Repeat(" ", 251) + "refs/heads/main\n"), false},
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
				InnerHEADs: []heldHEAD{{Path: "/ws/.git/hooks/HEAD"}}}}},
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
				InnerHEADs: []heldHEAD{{Path: "/ws/.git/hooks/x/HEAD"}}}}},
		// Nor is such an entry a move: two unknown kinds do not match.
		{name: "entry with no kind at another path",
			before: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"},
				ids: map[string]gitEntryID{"/ws/.git": dir(1), "/ws/y": dir(50)}},
			after: sandboxGitLayout{root: "/ws", dotGitDirs: []string{"/ws/.git"}, metadata: []string{"/ws/.git/hooks"},
				layoutHEADs: []string{"/ws/.git/hooks/x/HEAD"},
				ids:         map[string]gitEntryID{"/ws/.git": dir(1), "/ws/.git/hooks": dir(60), "/ws/.git/hooks/x": dir(50)}},
			want: []heldItem{{Path: "/ws/.git/hooks", ID: dir(60), Kind: heldGitMetadata,
				InnerHEADs: []heldHEAD{{Path: "/ws/.git/hooks/x/HEAD"}}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := newGitMachinery(c.before, c.after); !reflect.DeepEqual(got, c.want) {
				t.Errorf("held:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestEntryKindPrecedence checks all overlapping roles for a single entry.
// Every independently discoverable nested gitfile is held before its parent.
func TestEntryKindPrecedence(t *testing.T) {
	// containment: GITMETA-POSTRUN-DIFF
	const p, child = "/ws/.git/p", "/ws/.git/p/x/.git"
	dir := gitEntryID{Dev: 1, Ino: 10, Mode: fs.ModeDir}
	file := gitEntryID{Dev: 1, Ino: 11}
	whole := func(k heldKind, heads ...string) []heldItem {
		var observed []heldHEAD
		for _, p := range heads {
			observed = append(observed, heldHEAD{Path: p})
		}
		return []heldItem{{Path: child, ID: file, Kind: heldDotGitFile}, {Path: p, ID: dir, Kind: k, InnerHEADs: observed}}
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
	collectForCheck(t)
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	runIn(t, root, `mkdir -p link bare.git/objects bare.git/refs/heads wtadmin .git/evil/objects .git/evil/refs && `+
		`ln -s ../nested/.git link/.git && `+validHEADBody+` > bare.git/HEAD && `+validHEADBody+` > .git/evil/HEAD && `+
		`printf '%040d\n' 0 > wtadmin/HEAD && echo ../.git > wtadmin/commondir`)
	before, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	before = *before.baseline
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
	after.screen = nil
	before.mounted = nil
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
				l := sandboxGitLayout{root: after.root, collect: true, dotGitDirs: slices.Clone(after.dotGitDirs),
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
				l := sandboxGitLayout{root: after.root, collect: true, dotGitDirs: slices.Clone(after.dotGitDirs),
					layoutHEADs: slices.Clone(after.layoutHEADs)}
				l.finish()
				if len(l.repoLayouts) != n {
					b.Fatalf("repoLayouts %d, want %d", len(l.repoLayouts), n)
				}
			}
		})
	}
}

// TestLayoutMarkersRequireDirectories distinguishes files, directories and symlinks.
func TestLayoutMarkersRequireDirectories(t *testing.T) {
	for _, c := range []struct {
		name, setup     string
		layout, warning bool
	}{
		{"executable files", "touch objects refs && chmod 755 objects refs", false, false},
		{"linked object directory", "mkdir refs target && ln -s target objects", false, true},
		{"linked refs directory", "mkdir objects target && ln -s target refs", false, true},
		{"directories", "mkdir objects refs", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GIT_OBJECT_DIRECTORY", "")
			root := canonGitTempDir(t)
			runIn(t, root, validHEADBody+" > HEAD && "+c.setup)
			l := inspectSandboxWorkspaceAfter(root)
			if (len(l.layoutHEADs) == 1) != c.layout || (len(l.unlisted) > 0) != c.warning {
				t.Errorf("HEADs %v warnings %v", l.layoutHEADs, l.unlisted)
			}
		})
	}
}

// TestGitInspectionFailures distinguishes disappearance, lookup failure and changed file type.
func TestGitInspectionFailures(t *testing.T) {
	root := canonGitTempDir(t)
	p := filepath.Join(root, "HEAD")
	runIn(t, root, "ln -s refs/main HEAD")
	old := gitReadlink
	t.Cleanup(func() { gitReadlink = old })
	for _, err := range []error{unix.ENOENT, unix.EIO} {
		gitReadlink = func(string) (string, error) { return "", err }
		l := sandboxGitLayout{root: root, afterWalk: true}
		l.noteLayout(root)
		if len(l.headIDs) != 0 || (len(l.unlisted) > 0) != (err == unix.EIO) {
			t.Errorf("%v: ids %v, warnings %v", err, l.headIDs, l.unlisted)
		}
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := writeBody("ref: refs/main")(p); err != nil {
		t.Fatal(err)
	}
	open := openGitHEAD
	t.Cleanup(func() { openGitHEAD = open })
	openGitHEAD = func(string, int, fs.FileMode) (*os.File, error) { return os.Open(root) }
	if valid, err := readGitHEAD(p, entryIDOf(t, p)); valid || err != nil {
		t.Errorf("directory after open: %v, %v", valid, err)
	}
}

// validHEAD reports whether the inspected path contains a Git-valid HEAD.
func validHEAD(p string) bool {
	id, err := lookupID(p)
	if err != nil {
		return false
	}
	valid, err := readGitHEAD(p, id)
	return err == nil && valid
}

// inspectSandboxWorkspaceAfter inspects a test workspace using an empty credential screen.
func inspectSandboxWorkspaceAfter(root string) sandboxGitLayout {
	pin, err := os.OpenFile(root, workspaceSearchFlags|unix.O_NOFOLLOW, 0)
	if err == nil {
		defer func() { _ = pin.Close() }()
	}
	return inspectSandboxWorkspaceAfterPinned(root, pin, testScreen())
}

// BenchmarkGitMachineryHardLinks measures unchanged names sharing one identity.
func BenchmarkGitMachineryHardLinks(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			l := sandboxGitLayout{ids: make(map[string]gitEntryID, n)}
			for i := range n {
				p := fmt.Sprintf("/w/%d/.git", i)
				l.gitFiles = append(l.gitFiles, p)
				l.ids[p] = gitEntryID{Dev: 1, Ino: 2}
			}
			for b.Loop() {
				if got := newGitMachinery(l, l); len(got) != 0 {
					b.Fatal("held unchanged names")
				}
			}
		})
	}
}

// TestMovedHEADObservations keeps each HEAD's identity and provenance across moves.
func TestMovedHEADObservations(t *testing.T) {
	before := sandboxGitLayout{headIDs: make(map[string]gitEntryID)}
	after := sandboxGitLayout{dotGitDirs: []string{"/w/.git"}, ids: map[string]gitEntryID{"/w/.git": {Ino: 10, Mode: fs.ModeDir}}, headIDs: make(map[string]gitEntryID)}
	for i := range 200 {
		p := fmt.Sprintf("/w/.git/%03d/HEAD", i)
		id := gitEntryID{Dev: 1, Ino: uint64(100 + i)}
		if i%2 == 0 {
			before.headIDs[fmt.Sprintf("/w/old/%03d/HEAD", i)] = id
		}
		after.headIDs[p] = id
		after.layoutHEADs = append(after.layoutHEADs, p)
	}
	items := newGitMachinery(before, after)
	if len(items) != 1 || len(items[0].InnerHEADs) != len(after.headIDs) {
		t.Fatalf("incomplete HEAD observations")
	}
	for i, h := range items[0].InnerHEADs {
		if h.ID != after.headIDs[h.Path] || h.Preexisting != (i%2 == 0) {
			t.Fatalf("incorrect HEAD observation %v", h)
		}
	}
}

// TestFinalLayoutLookupErrors distinguishes a vanished directory from an incomplete inspection.
func TestFinalLayoutLookupErrors(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "touch file")
	for _, c := range []struct {
		path   string
		warned bool
	}{{"missing", false}, {"file/child", true}} {
		p := filepath.Join(root, c.path)
		l := sandboxGitLayout{root: root, afterWalk: true, collect: true, gitDirs: []string{p}}
		l.finish()
		if _, ok := l.ids[p]; ok || (len(l.unlisted) > 0) != c.warned {
			t.Errorf("lookup %q: ids %v, warnings %v", c.path, l.ids, l.unlisted)
		}
	}
}

// TestLookupKeysIgnoreListedHardLinks checks both the complete key set and the recorded link identity.
func TestLookupKeysIgnoreListedHardLinks(t *testing.T) {
	// containment: GITMETA-POSTRUN-DIFF
	root := canonGitTempDir(t)
	runIn(t, root, "ln -s elsewhere .GIT")
	original := gitLookupPath
	gitLookupPath = func(dir, name string) string {
		if name == ".git" {
			return filepath.Join(dir, ".GIT")
		}
		return filepath.Join(dir, name)
	}
	t.Cleanup(func() { gitLookupPath = original })
	for _, links := range []bool{false, true} {
		if links {
			hardLinkNoFollow(".GIT", "ordinary")(t, root)
		}
		l := sandboxGitLayout{root: root, collect: true, lookupDirs: map[string]bool{root: true}}
		l.lookupGitNames(root)
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			l.visit(filepath.Join(root, e.Name()), e.Type(), false)
		}
		l.finish()
		key := filepath.Join(root, ".git")
		if len(l.dotGitLinks) != 1 || l.dotGitLinks[0] != key || len(l.ids) != 1 || l.ids[key] != entryIDOf(t, filepath.Join(root, ".GIT")) {
			t.Fatalf("ordinary link present %v: names %v, identities %v", links, l.dotGitLinks, l.ids)
		}
	}
}

// TestLayoutWithExternalObjects recognizes host discovery when an object directory is configured.
func TestLayoutWithExternalObjects(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir refs && "+validHEADBody+" > HEAD")
	for _, tc := range []struct {
		objects string
		count   int
	}{{"", 0}, {t.TempDir(), 1}} {
		t.Setenv("GIT_OBJECT_DIRECTORY", tc.objects)
		if got := inspectSandboxWorkspaceAfter(root); len(got.layoutHEADs) != tc.count {
			t.Errorf("HEADs %v, want %d", got.layoutHEADs, tc.count)
		}
	}
}

// TestGitWorkspaceAdministrativeRoot checks new metadata in an administrative workspace.
func TestGitWorkspaceAdministrativeRoot(t *testing.T) {
	collectForCheck(t)
	root := filepath.Join(canonGitTempDir(t), ".git")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	before, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	runIn(t, root, "mkdir hooks")
	items := newGitMachinery(before, inspectSandboxWorkspaceAfter(root))
	if len(items) != 1 || items[0].Path != filepath.Join(root, "hooks") {
		t.Fatalf("new metadata: %v", items)
	}
}

// TestNewGitMachineryUnlistableParent finds a named .git when the parent permits only search.
func TestNewGitMachineryUnlistableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	collectForCheck(t)
	original := ownerAccessEuid
	ownerAccessEuid = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { ownerAccessEuid = original })
	root := canonGitTempDir(t)
	restoreModes(t, root)
	before, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	runIn(t, root, "mkdir -p hidden/.git/objects hidden/.git/refs && "+validHEADBody+" > hidden/.git/HEAD && chmod 300 hidden")
	after := inspectSandboxWorkspaceAfter(root)
	items := newGitMachinery(before, after)
	dir := filepath.Join(root, "hidden/.git")
	if !slices.Equal(after.unlisted, []string{filepath.Dir(dir)}) || len(items) != 1 || items[0].Path != dir || len(items[0].InnerHEADs) != 1 || items[0].InnerHEADs[0].Path != dir+"/HEAD" {
		t.Fatalf("unlisted %v, items %v", after.unlisted, items)
	}
}

// newGitMachinery returns the entries selected by the comparison.
func newGitMachinery(before, after sandboxGitLayout) []heldItem {
	return compareGitMachinery(before, after, nil)
}

// TestUnknownGitBaselineKeepsEntries checks failed observations and a known-new sibling.
func TestUnknownGitBaselineKeepsEntries(t *testing.T) {
	collectForCheck(t)
	for _, tc := range []struct {
		name, setup, path, change string
	}{
		{"link target", "ln -s elsewhere .git", ".git", ""},
		{"enclosing entry", "ln -s elsewhere .git", ".git", "mv .git old-link && mkdir -p .git/hooks"},
		{"layout HEAD", "mkdir -p repo/objects repo/refs && " + validHEADBody + " > repo/HEAD", "repo/HEAD", "mkdir -p repo/nested/.git"},
		{"metadata target", "mkdir .git && ln -s elsewhere .git/info", ".git/info", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			runIn(t, root, tc.setup)
			path := filepath.Join(root, tc.path)
			link, open := gitReadlink, openGitHEAD
			t.Cleanup(func() { gitReadlink, openGitHEAD = link, open })
			gitReadlink = func(p string) (string, error) {
				if p == path {
					return "", unix.EIO
				}
				return link(p)
			}
			openGitHEAD = func(p string, flag int, mode fs.FileMode) (*os.File, error) {
				if p == path {
					return nil, unix.EIO
				}
				return open(p, flag, mode)
			}
			before, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			gitReadlink, openGitHEAD = link, open
			runIn(t, root, tc.change)
			runIn(t, root, "mkdir other && touch other/.git")
			got := newGitMachinery(before, inspectSandboxWorkspaceAfter(root))
			if len(got) != 1 || got[0].Path != filepath.Join(root, "other/.git") {
				t.Fatalf("holds %v, want only other/.git", got)
			}
		})
	}
}

// TestListedGitDirectoryMatchesLookupIdentity gives the named lookup and listing
// different spellings of the same directory while preserving its parent and inode.
func TestListedGitDirectoryMatchesLookupIdentity(t *testing.T) {
	for _, listed := range []string{".git", ".GIT"} {
		t.Run(listed, func(t *testing.T) {
			root := canonGitTempDir(t)
			runIn(t, root, "mkdir -p .git/refs .git/objects && "+validHEADBody+" > .git/HEAD")
			l := sandboxGitLayout{root: root, collect: true, lookupDirs: map[string]bool{root: true}}
			l.observeDotGit(root)
			lookup := filepath.Join(root, ".git")
			path := filepath.Join(root, listed)
			if path != lookup {
				if err := os.Rename(lookup, path); err != nil {
					t.Fatal(err)
				}
			}
			l.walkAfter(path)
			l.finish()
			if len(l.ids) != 1 || l.ids[lookup] != entryIDOf(t, path) ||
				!slices.Equal(l.layoutHEADs, []string{filepath.Join(lookup, "HEAD")}) {
				t.Fatalf("directory identities %v, layout HEADs %v", l.ids, l.layoutHEADs)
			}
		})
	}
}
