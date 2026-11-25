//go:build darwin || linux

package tools

// The post-run check's diff (#802): which git machinery a sandboxed command
// created, from the workspace layouts walked before it (inspectSandboxWorkspace)
// and after it (inspectSandboxWorkspaceAfter). It reads only the two layouts.

import (
	"context"
	"maps"
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

// heldHEAD carries a HEAD observation through directory name merging.
type heldHEAD struct {
	Path        string
	ID          gitEntryID
	Preexisting bool
}

// heldItem is one piece of new git machinery to set aside. Path is absolute,
// spelled as the after-walk found it; ID is its identity then, to recheck
// before renaming. InnerHEADs, for a directory, is the HEAD of each new
// repository layout at or under Path (its own included) that no nearer held
// item covers, spelled under Path, so git started inside the held copy cannot
// recognize it either. A heldRepoLayout is set aside by its InnerHEADs alone,
// never renamed itself: it may be a directory the user already had, or the
// workspace root. So it hides nothing under it, and each new item there is
// held on its own; every other held item is renamed whole and hides its
// contents, except new items in a baseline repository moved under it, which
// are held on their own first. Each HEAD retains its observed identity.
type heldItem struct {
	Path       string
	ID         gitEntryID
	Kind       heldKind
	InnerHEADs []heldHEAD
}

// compareGitMachinery returns the topmost new git machinery in after, sorted by
// path. An entry is new unless it is a baseline entry (one in before.ids,
// matched by identity): unchanged at its path, or moved. Each unchanged path
// is set apart first; every other name of a baseline identity then needs a
// baseline name of the same kind that is no longer at its path, each such
// name excusing one move. So a second hard link to a baseline .git symlink is
// new, and so is a baseline identity in another kind's slot (a .git symlink
// renamed to commondir, a metadata name turned into a second .git file) or a
// planted entry at a baseline path. One exception (hostSide): a baseline
// metadata entry, container or git dir replaced in place while its .git
// stayed the baseline one is not new. An item inside another held item is
// held independently if it is a gitfile or .git symlink, or if a baseline .git or
// git dir lies between them: the repository moved there keeps its baseline
// HEAD, so its new items are held on their own, before the item above. Its
// walks cost the entries times their depth (pathTree). An unknown baseline
// excludes the affected entry and reports its path and reason to unchecked.
func compareGitMachinery(ctx context.Context, before, after sandboxGitLayout, unchecked func(string, string)) []heldItem {
	if ctx.Err() != nil {
		return nil
	}
	if before.baseline != nil {
		before = *before.baseline
	}
	beforeKinds, afterKinds := entryKinds(ctx, before), entryKinds(ctx, after)
	var tree pathTree
	for p := range afterKinds {
		if ctx.Err() != nil {
			return nil
		}
		tree.node(p)
	}
	unknown := maps.Clone(before.unknown)
	heads := make([]int, len(after.layoutHEADs))
	for i, h := range after.layoutHEADs {
		if ctx.Err() != nil {
			return nil
		}
		dir := filepath.Dir(h)
		heads[i] = tree.node(dir)
		if why := before.unknown[h]; why != "" {
			if _, seen := unknown[dir]; !seen {
				unknown[dir] = why
			}
		}
	}
	// An unknown slot also prevents a comparison of anything it encloses.
	reportUnknown := func(p string) bool {
		if len(unknown) == 0 {
			return false
		}
		why := unknown[p]
		if parent, ok := tree.index[filepath.Dir(p)]; ok {
			for i := parent; why == "" && i >= 0; i = tree.parent[i] {
				why = unknown[tree.path[i]]
			}
		}
		if why != "" && unchecked != nil {
			unchecked(p, why)
		}
		return why != ""
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
		if _, was := before.ids[p]; !was || before.mounted != nil && !before.mounted[p] {
			return false
		}
		if !mounted(beforeKinds[p]) || beforeKinds[p] != afterKinds[p] {
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
	// allow counts, per identity and kind, the baseline names no longer at
	// their path with that identity: each excuses one name moved elsewhere.
	type idKind struct {
		id   gitEntryID
		kind heldKind
	}
	allow := make(map[idKind]int)
	known := make(map[gitEntryID]bool, len(before.ids))
	for p, id := range before.ids {
		if ctx.Err() != nil {
			return nil
		}
		known[id] = true
		if now, ok := after.ids[p]; !ok || now != id || beforeKinds[p] != afterKinds[p] {
			allow[idKind{id, beforeKinds[p]}]++
		}
	}
	paths := make([]string, 0, len(after.ids))
	for p := range after.ids {
		if ctx.Err() != nil {
			return nil
		}
		paths = append(paths, p)
	}
	slices.Sort(paths) // allowances go to names in path order
	fresh := make(map[string]bool, len(paths))
	for _, p := range paths {
		if ctx.Err() != nil {
			return nil
		}
		id, k := after.ids[p], afterKinds[p]
		if reportUnknown(p) {
			continue
		}
		was, existed := before.ids[p]
		switch {
		case !known[id]:
			fresh[p] = !hostSide(p)
		case existed && was == id && beforeKinds[p] == k:
			// unchanged at its path
		case k != kindUnknown && allow[idKind{id, k}] > 0:
			allow[idKind{id, k}]--
		default:
			fresh[p] = true
		}
	}
	// isNew reports whether after's entry at p (one with an identity) is new.
	isNew := func(p string) bool { return fresh[p] }
	// baselineRepo marks each .git directory or git dir that is a baseline
	// entry (not new), wherever it is now.
	repos := make([]string, 0, len(after.dotGitDirs)+len(after.gitDirs))
	for _, d := range slices.Concat(after.dotGitDirs, after.gitDirs) {
		if ctx.Err() != nil {
			return nil
		}
		if _, ok := after.ids[d]; ok && !fresh[d] {
			repos = append(repos, d)
		}
	}

	cands := make([]string, 0, len(afterKinds))
	for p, k := range afterKinds {
		if ctx.Err() != nil {
			return nil
		}
		if k != kindGitDir {
			cands = append(cands, p)
		}
	}
	slices.Sort(cands) // a parent sorts before its children, so it is held first

	baselineRepo := tree.marks(repos) // each is in afterKinds, so already a node
	var held []heldItem
	heldAt := make(map[int]int) // each held node's index in held
	under := make(map[int]int)  // each held item kept inside a hiding one: that one's index in held
	isHeld := make([]bool, len(tree.parent))
	hides := make([]bool, len(tree.parent)) // the held nodes renamed whole
	for _, p := range cands {
		if ctx.Err() != nil {
			return nil
		}
		if _, ok := after.ids[p]; !ok || !isNew(p) {
			continue
		}
		i := tree.index[p]
		above := tree.up(tree.parent[i], hides)
		if above >= 0 {
			// Gitfiles and .git links remain discoverable within a renamed tree,
			// as does new metadata inside a baseline administrative directory.
			r := tree.up(tree.parent[i], baselineRepo)
			if afterKinds[p] != heldDotGitFile && afterKinds[p] != heldDotGitLink && (r < 0 || tree.up(r, hides) != above || r == above) {
				continue
			}
		}
		heldAt[i], isHeld[i] = len(held), true
		if above >= 0 {
			under[len(held)] = heldAt[above]
		}
		held = append(held, heldItem{Path: p, ID: after.ids[p], Kind: afterKinds[p]})
		hides[i] = afterKinds[p] != heldRepoLayout
	}
	// Each new layout's HEAD goes to the nearest held item at or above it.
	preexisting := make(map[gitEntryID]bool, len(before.headIDs))
	for _, id := range before.headIDs {
		if ctx.Err() != nil {
			return nil
		}
		preexisting[id] = true
	}
	for n, h := range after.layoutHEADs {
		if ctx.Err() != nil {
			return nil
		}
		dir := tree.path[heads[n]]
		if _, recorded := after.ids[dir]; recorded && !isNew(dir) {
			continue // a baseline repository, in place or moved in: the user's
		}
		if i := tree.up(heads[n], isHeld); i >= 0 && !reportUnknown(h) {
			it := &held[heldAt[i]]
			id := after.headIDs[h]
			if preexisting[id] {
				if _, granted := slices.BinarySearch(after.grantReachedHEADs, h); granted {
					if unchecked != nil {
						unchecked(h, "its parent was only reachable with temporary search permission")
					}
					continue
				}
			}
			it.InnerHEADs = append(it.InnerHEADs, heldHEAD{Path: h, ID: id, Preexisting: preexisting[id]})
		}
	}
	// Each item kept inside a hiding one goes just before it, so it is held
	// at its path before that one is renamed.
	inside := make(map[int][]int)
	for j, a := range under {
		if ctx.Err() != nil {
			return nil
		}
		inside[a] = append(inside[a], j)
	}
	out := make([]heldItem, 0, len(held))
	var emit func(j int)
	emit = func(j int) {
		if ctx.Err() != nil {
			return
		}
		kids := inside[j]
		slices.Sort(kids)
		for _, k := range kids {
			emit(k)
		}
		out = append(out, held[j])
	}
	for j := range held {
		if ctx.Err() != nil {
			return nil
		}
		if _, ok := under[j]; !ok {
			emit(j)
		}
	}
	return out
}

// entryKinds returns the kind of each path l records an identity for. It is
// the one place a kind is derived. A path in several lists takes the first
// kind of this precedence, strongest first: a .git entry, metadata, a
// container, a git dir, a layout. So a new hooks directory or container that
// is also shaped like a repository is still renamed whole, and a git dir in a
// container (most are layouts) stays its container's to hold.
func entryKinds(ctx context.Context, l sandboxGitLayout) map[string]heldKind {
	kinds := make(map[string]heldKind, len(l.ids))
	for _, g := range []struct {
		kind  heldKind
		paths []string
	}{
		{heldDotGitDir, l.dotGitDirs}, {heldDotGitFile, l.gitFiles}, {heldDotGitLink, l.dotGitLinks},
		{heldGitMetadata, l.metadata}, {heldGitContainer, l.containers}, {heldGitContainer, l.containerLinks},
		{kindGitDir, l.gitDirs},
		{heldRepoLayout, l.repoLayouts}, {heldRepoLayout, l.innerLayouts},
	} {
		for _, p := range g.paths {
			if ctx.Err() != nil {
				return nil
			}
			if _, taken := kinds[p]; !taken {
				kinds[p] = g.kind
			}
		}
	}
	return kinds
}
