//go:build darwin || linux

package tools

// The post-run check's diff (#802): which git machinery a sandboxed command
// created, from the workspace layouts walked before it (inspectSandboxWorkspace)
// and after it (inspectSandboxWorkspaceAfter). It reads only the two layouts.

import (
	"path/filepath"
	"slices"
)

// heldKind is what a held item is, for the note that reports it.
type heldKind int

const (
	// kindUnknown, the zero value, is a path with no recorded kind. It is
	// never held, never a move and never excused as a host-side change.
	kindUnknown      heldKind = iota
	heldDotGitDir             // a directory named .git
	heldDotGitFile            // a regular file named .git (a gitfile)
	heldDotGitLink            // a symbolic link named .git
	heldGitMetadata           // hooks, info, config, config.worktree, commondir or gitdir in a git dir
	heldGitContainer          // a modules or worktrees container in a git dir, held whole
	heldRepoLayout            // a directory git would treat as a repository, held by its HEADs alone
	// kindGitDir is a git dir in a modules or worktrees container (gitDirs).
	// It takes part in the identity comparison but is never held on its own:
	// a new one lies in a new container or a new .git, held whole.
	kindGitDir
)

// heldItem is one piece of new git machinery to set aside. Path is absolute,
// spelled as the after-walk found it; ID is its identity then, to recheck
// before renaming. InnerHEADs, for a directory, is the HEAD of each new
// repository layout at or under Path (its own included) that no nearer held
// item covers, spelled under Path, so git started inside the held copy cannot
// recognize it either. A heldRepoLayout is set aside by its InnerHEADs alone,
// never renamed itself: it may be a directory the user already had, or the
// workspace root. So it hides nothing under it, and each new item there is
// held on its own; every other held item is renamed whole and hides its
// contents.
type heldItem struct {
	Path       string
	ID         gitEntryID
	Kind       heldKind
	InnerHEADs []string
}

// newGitMachinery returns the topmost new git machinery in after, sorted by
// path. An entry is new unless it is a baseline entry (one in before.ids,
// matched by identity): unchanged at its path, or moved, which needs the same
// kind and no more names than it had. So a second hard link to a baseline .git
// symlink is new, and so is a baseline identity in another kind's slot (a
// .git symlink renamed to commondir) or a planted entry at a baseline path.
// One exception (hostSide): a baseline metadata entry, container or git dir
// replaced in place while its .git stayed the baseline one is not new. Its
// walks cost the entries times their depth (pathTree).
func newGitMachinery(before, after sandboxGitLayout) []heldItem {
	type place struct {
		path string
		kind heldKind
	}
	beforeKinds, afterKinds := entryKinds(before), entryKinds(after)
	baseline := make(map[gitEntryID][]place, len(before.ids))
	for p, id := range before.ids {
		baseline[id] = append(baseline[id], place{p, beforeKinds[p]})
	}
	names := make(map[gitEntryID]int, len(after.ids))
	for _, id := range after.ids {
		names[id]++
	}
	var tree pathTree
	for p := range afterKinds {
		tree.node(p)
	}
	heads := make([]int, len(after.layoutHEADs))
	for i, h := range after.layoutHEADs {
		heads[i] = tree.node(filepath.Dir(h))
	}
	dotGit := tree.marks(after.dotGitDirs)
	// hostSide reports a baseline metadata entry, container or git dir at p
	// (that kind both before and after) with a new identity, whose nearest
	// .git is still at its path with its baseline identity. The sandbox mounts
	// each of those over itself, so a command cannot replace one in place; with
	// the same .git still there, the change came from outside it, such as host
	// git rewriting config by lock and rename while a background job ran. A
	// command that moves the checkout away and plants another fails the .git
	// identity test, and a path that was only a layout before (never mounted)
	// fails the before-kind test.
	mounted := func(k heldKind) bool { return k == heldGitMetadata || k == heldGitContainer || k == kindGitDir }
	hostSide := func(p string) bool {
		if _, was := before.ids[p]; !was {
			return false
		}
		if !mounted(beforeKinds[p]) || !mounted(afterKinds[p]) {
			return false // kindUnknown included
		}
		i, ok := tree.index[p]
		if !ok {
			return false
		}
		g := tree.up(tree.parent[i], dotGit)
		if g < 0 {
			return false
		}
		now, nowOK := after.ids[tree.path[g]]
		was, wasOK := before.ids[tree.path[g]]
		return nowOK && wasOK && now == was
	}
	// isNew reports whether after's entry at p (one with an identity) is new.
	isNew := func(p string) bool {
		id, k := after.ids[p], afterKinds[p]
		from, known := baseline[id]
		switch {
		case !known:
			return !hostSide(p)
		case slices.ContainsFunc(from, func(b place) bool { return b.path == p }):
			return false
		default:
			return k == kindUnknown || names[id] > len(from) || !slices.ContainsFunc(from, func(b place) bool { return b.kind == k })
		}
	}

	cands := make([]string, 0, len(afterKinds))
	for p, k := range afterKinds {
		if k != kindGitDir {
			cands = append(cands, p)
		}
	}
	slices.Sort(cands) // a parent sorts before its children, so it is held first

	var held []heldItem
	heldAt := make(map[int]int) // each held node's index in held
	isHeld := make([]bool, len(tree.parent))
	hides := make([]bool, len(tree.parent)) // the held nodes renamed whole
	for _, p := range cands {
		if _, ok := after.ids[p]; !ok || !isNew(p) {
			continue
		}
		i := tree.index[p]
		if tree.up(tree.parent[i], hides) >= 0 {
			continue
		}
		heldAt[i], isHeld[i] = len(held), true
		held = append(held, heldItem{Path: p, ID: after.ids[p], Kind: afterKinds[p]})
		hides[i] = afterKinds[p] != heldRepoLayout
	}
	// Each new layout's HEAD goes to the nearest held item at or above it.
	for n, h := range after.layoutHEADs {
		dir := tree.path[heads[n]]
		if _, recorded := after.ids[dir]; recorded && !isNew(dir) {
			continue // a baseline repository, in place or moved in: the user's
		}
		if i := tree.up(heads[n], isHeld); i >= 0 {
			held[heldAt[i]].InnerHEADs = append(held[heldAt[i]].InnerHEADs, h)
		}
	}
	return held
}

// entryKinds returns the kind of each path l records an identity for. It is
// the one place a kind is derived. A path in several lists takes the first
// kind of this precedence, strongest first: a .git entry, metadata, a
// container, a git dir, a layout. So a new hooks directory or container that
// is also shaped like a repository is still renamed whole, and a git dir in a
// container (most are layouts) stays its container's to hold.
func entryKinds(l sandboxGitLayout) map[string]heldKind {
	kinds := make(map[string]heldKind, len(l.ids))
	for _, g := range []struct {
		kind  heldKind
		paths []string
	}{
		{heldDotGitDir, l.dotGitDirs}, {heldDotGitFile, l.gitFiles}, {heldDotGitLink, l.dotGitLinks},
		{heldGitMetadata, l.metadata},
		{heldGitContainer, l.containers},
		{kindGitDir, l.gitDirs},
		{heldRepoLayout, l.repoLayouts}, {heldRepoLayout, l.innerLayouts},
	} {
		for _, p := range g.paths {
			if _, taken := kinds[p]; !taken {
				kinds[p] = g.kind
			}
		}
	}
	return kinds
}
