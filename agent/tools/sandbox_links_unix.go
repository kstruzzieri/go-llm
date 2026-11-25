//go:build darwin || linux

package tools

// Shared pre-spawn workspace walk for the pathname/bind-mount sandbox
// backends (#442 Seatbelt, #441 bwrap). Both authorize the workspace by
// pathname (SBPL subpath, bind mount), so an inode reachable through an
// outside name must be rejected before either lifetime spawns. The same walk
// finds the workspace's git metadata, which both backends refuse to let a
// child write (#721).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// sandboxGitLayout records mount candidates or one comparison snapshot. A
// before-walk collects both in one traversal. Only mount candidates affect
// pre-spawn policy. Comparison paths use Git's spelling in lookup directories.
type sandboxGitLayout struct {
	dotGitDirs    []string // each directory named .git (ASCII case-folded), any depth
	gitDirs       []string // each submodule / linked-worktree git dir (not named .git)
	containers    []string // each existing <D>/modules and <D>/worktrees, D in dotGitDirs or gitDirs
	intermediates []string // each directory between a modules container and a submodule git dir
	gitFiles      []string // each regular file named .git (ASCII case-folded)
	metadata      []string // each existing <D>/{hooks,info,config,config.worktree,commondir,gitdir}, D in dotGitDirs or gitDirs
	// moduleReadOnly is each directory in a modules tree named hooks or info
	// (ASCII case-folded, on-disk spelling) that is not a git dir
	// (descendModules clause 2); both backends make it read-only whole.
	moduleReadOnly []string

	// The post-run check (#802) compares two walks of the workspace by these.
	// No backend mounts them, so recording them changes no mount or refusal.
	// In the after-walk, which no backend mounts either, dotGitDirs, gitFiles
	// and dotGitLinks also hold each entry a lookup of .git resolves to
	// (lookupGitNames), whatever its spelling.
	dotGitLinks []string // each symbolic link named .git (ASCII case-folded)
	// layoutHEADs is the HEAD of each directory git would treat as a
	// repository (noteLayout), anywhere in the workspace, git dirs included;
	// it supplies the HEADs a held item must also set aside.
	layoutHEADs []string
	// grantReachedHEADs are valid layouts reached only through temporarily
	// opened workspace ancestors. Only a newly created HEAD may be held there.
	grantReachedHEADs []string
	// repoLayouts is each directory of layoutHEADs that is neither a directory
	// named .git nor inside one: a bare repository or a linked-worktree admin
	// directory, for example. One inside another layout is still listed.
	repoLayouts []string
	// innerLayouts is each directory of layoutHEADs at or inside a directory
	// named .git, whatever else it also is: the .git itself, a git dir, a
	// hooks directory, or a command-made .git/evil. entryKinds decides which
	// kind such a path counts as.
	innerLayouts []string
	// baseline is the comparison snapshot collected alongside the mount inspection.
	baseline       *sandboxGitLayout
	containerLinks []string
	headIDs, ids   map[string]gitEntryID
	// lookupDirs marks directories where git names are recorded solely by lookup.
	lookupDirs map[string]bool
	// Match listed directories to named lookups within the same parent inode.
	listedDirs map[string]gitDirKey
	namedDirs  map[gitDirKey]string
	// unknown records baseline observations that could not be compared.
	unknown map[string]string
	// mounted marks metadata protected during this invocation.
	mounted map[string]bool
	// unlisted is each directory the after-walk could not list or search
	// (inspectSandboxWorkspaceAfter); the before-walk fails instead.
	unlisted       []string
	unlistedSet    map[string]bool
	unreadableDirs []string // directories needing the named-lookup fallback
	// unlistedWhy is the first reason the after-walk recorded for a
	// directory, by path, with no path in its text.
	unlistedWhy map[string]string
	// warnings are the after-walk's own warnings: a mode it opened up and
	// could not put back.
	warnings []string
	// access opens up, during the after-walk only, a directory the user owns
	// whose mode keeps the walk out (ownerAccess).
	access *ownerAccess
	// lookupPolicies caches each device's Git name lookup policy for this walk.
	lookupPolicies map[uint64]fsLookupPolicy

	root          string // the workspace root; errors name paths relative to it
	workspacePath string // approved host spelling for absolute commondir pointers in a pinned walk
	// pin is the root's descriptor, taken by prepare before its walk, through
	// which the post-run check reaches the workspace; nil outside a bwrap run.
	pin *os.File
	// screen is the command's credential screen, through which every path a
	// held-git note shows passes; nil outside a run.
	screen    *outputScreen
	afterWalk bool // record unreadable directories in unlisted rather than fail
	// collect records what only the post-run check uses: .git links, layouts
	// and their HEADs (whose contents it reads), container links
	// and identities. The before-walk does so where that check runs
	// (postRunCollect); macOS's Seatbelt inspection reads only what it mounts.
	collect bool
}

// fsLookupPolicy selects exact listings, named lookups or a per-directory flag check.
type fsLookupPolicy uint8

// gitEntryID identifies a git entry across the before- and after-walks: an
// entry with the same identity at another path was moved, not created.
type gitEntryID struct {
	Dev, Ino uint64
	Mode     fs.FileMode // the type bits only
	// Target is a symbolic link's target, empty for anything else. A link
	// deleted and recreated with another target can get its inode number back
	// (ext4 and XFS reuse them), so the number alone would call it unchanged.
	Target string
}

// gitDirKey identifies a directory entry within the directory that contains it.
type gitDirKey struct {
	parent, entry gitEntryID
}

// inspectSandboxWorkspace walks the workspace before a sandboxed spawn.
// It rejects an inode with any directory entry outside root: pathname-based
// sandboxes authorize names, so an allowed workspace hard link would
// otherwise let a child mutate the same inode through an outside name.
// Internal-only hard links remain valid. It also returns the workspace's git
// layout: every entry named .git, then each .git directory's submodule and
// worktree structure (scanGitDir). A pass that sees the workspace change
// under it (errSandboxWorkspaceChanged: git's automatic gc pruning objects, an
// editor replacing a file) is discarded whole and the walk starts again, at
// most sandboxInspectPasses passes in total. A short hard-link count must
// recur on consecutive passes before it is reported as an outside link.
// Only a successful pass supplies a layout. Any other error or exhaustion
// of the pass limit fails the spawn. An unsandboxed same-UID process can
// still race this final host check under the documented threat model.
func inspectSandboxWorkspace(root string) (sandboxGitLayout, error) {
	return inspectSandboxWorkspaceWithWalk(root, filepath.WalkDir)
}

// sandboxInspectPasses bounds the workspace passes inspectSandboxWorkspace
// makes while the workspace keeps changing under it.
// ponytail: three passes, no wait between them; add a short backoff if a
// tool's churn routinely outlasts three walks.
const sandboxInspectPasses = 3

// errSandboxWorkspaceChanged marks a pass that saw the workspace change under
// it, or could not account for all names of a hard-linked inode. The next
// pass rebuilds the layout and the link counts from scratch.
var errSandboxWorkspaceChanged = errors.New("tools: sandbox workspace changed while inspecting")

// sandboxLinkShortfall identifies the same incomplete count across two walks.
type sandboxLinkShortfall struct {
	dev, ino    uint64
	seen, total uint64
	first       string
}

// Error names the workspace entry linked outside the root.
func (s *sandboxLinkShortfall) Error() string {
	return fmt.Sprintf("tools: sandbox workspace entry %q is linked outside the workspace", s.first)
}

// inspectSandboxWorkspaceWithWalk runs a fresh walk for each attempt.
func inspectSandboxWorkspaceWithWalk(root string, walk func(string, fs.WalkDirFunc) error) (sandboxGitLayout, error) {
	return inspectSandboxWorkspaceNamed(context.Background(), root, root, walk)
}

// inspectSandboxWorkspaceNamed walks root, whose approved host spelling is workspacePath.
func inspectSandboxWorkspaceNamed(ctx context.Context, root, workspacePath string, walk func(string, fs.WalkDirFunc) error) (sandboxGitLayout, error) {
	var previous *sandboxLinkShortfall
	var err error
	for range sandboxInspectPasses {
		var layout sandboxGitLayout
		layout, err = inspectSandboxWorkspaceOnce(ctx, root, workspacePath, walk)
		var short *sandboxLinkShortfall
		if errors.As(err, &short) && (previous == nil || *previous != *short) {
			// Another internal name may have moved into an already visited directory.
			err = fmt.Errorf("%w %q: found %d of %d hard links", errSandboxWorkspaceChanged, short.first, short.seen, short.total)
		}
		previous = short
		if !errors.Is(err, errSandboxWorkspaceChanged) {
			return layout, err
		}
	}
	return sandboxGitLayout{}, fmt.Errorf("%w; retry", err)
}

// inspectSandboxWorkspaceOnce is one pass of inspectSandboxWorkspace.
func inspectSandboxWorkspaceOnce(ctx context.Context, root, workspacePath string, walk func(string, fs.WalkDirFunc) error) (sandboxGitLayout, error) {
	type inode struct {
		dev uint64
		ino uint64
	}
	type linkRecord struct {
		total uint64
		seen  uint64
		first string
	}

	base := filepath.Base(workspacePath)
	layout := sandboxGitLayout{root: root}
	if postRunCollect {
		layout.baseline = &sandboxGitLayout{root: root, workspacePath: workspacePath, collect: true}
		layout.baseline.noteAdministrativeRoot(base)
	}
	links := make(map[inode]linkRecord)
	order := make([]inode, 0)
	// entryErr fails the pass on entry rel. An entry its parent listed that
	// no longer resolves marks the pass changed; the root never does.
	entryErr := func(rel string, err error) error {
		if rel != "." && errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w %q: %w", errSandboxWorkspaceChanged, rel, err)
		}
		return fmt.Errorf("tools: sandbox inspect workspace entry %q: %w", rel, err)
	}
	err := walk(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return fmt.Errorf("tools: sandbox inspect workspace links: %w", relErr)
		}
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			return entryErr(rel, walkErr)
		}
		// DirEntry.Info can return metadata cached when the parent was listed.
		// Classify the name as it exists at this visit.
		info, err := os.Lstat(name)
		if err != nil {
			return entryErr(rel, err)
		}
		kind := info.Mode().Type()
		if kind != entry.Type() {
			if entry.IsDir() {
				return entryErr(rel, syscall.ENOTDIR)
			}
			return fmt.Errorf("%w %q: entry type changed", errSandboxWorkspaceChanged, rel)
		}
		entryBase := filepath.Base(name)
		if rel == "." {
			entryBase = base
		}
		layout.noteEntry(name, entryBase, kind)
		if layout.baseline != nil {
			layout.baseline.visit(name, kind, false)
		}
		if info.IsDir() || kind&os.ModeSymlink != 0 {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: link metadata unavailable", rel)
		}
		total := uint64(stat.Nlink)
		if total <= 1 {
			return nil
		}
		key := inode{dev: uint64(stat.Dev), ino: uint64(stat.Ino)}
		record, exists := links[key]
		if !exists {
			record = linkRecord{total: total, first: rel}
			order = append(order, key)
		} else if record.total != total {
			return fmt.Errorf("%w %q: link count changed", errSandboxWorkspaceChanged, rel)
		}
		record.seen++
		links[key] = record
		return nil
	})
	if err != nil {
		return sandboxGitLayout{}, err
	}
	for _, key := range order {
		record := links[key]
		if record.seen < record.total {
			// A changed first name already explains the short count. Otherwise
			// the caller checks whether a fresh walk sees the same shortfall.
			fi, err := os.Lstat(filepath.Join(root, record.first))
			if err != nil {
				return sandboxGitLayout{}, entryErr(record.first, err)
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok &&
				(uint64(st.Dev) != key.dev || uint64(st.Ino) != key.ino || uint64(st.Nlink) != record.total) {
				return sandboxGitLayout{}, fmt.Errorf("%w %q: link count changed", errSandboxWorkspaceChanged, record.first)
			}
			return sandboxGitLayout{}, &sandboxLinkShortfall{
				dev: key.dev, ino: key.ino, seen: record.seen, total: record.total, first: record.first,
			}
		}
		if record.seen > record.total {
			return sandboxGitLayout{}, fmt.Errorf("%w %q: link count changed", errSandboxWorkspaceChanged, record.first)
		}
	}
	for _, d := range layout.dotGitDirs {
		if err := scanGitDir(ctx, d, &layout); err != nil {
			return sandboxGitLayout{}, err
		}
	}
	if check := layout.baseline; check != nil {
		if err := check.scanObservedDirs(ctx); err != nil {
			return sandboxGitLayout{}, err
		}
		check.mounted = make(map[string]bool)
		names := make(map[string]string)
		for _, p := range slices.Concat(layout.metadata, layout.containers, layout.gitDirs) {
			check.mounted[check.canonical(p, names)] = true
		}
		check.finish(ctx)
	}
	layout.finish(ctx)
	return layout, ctx.Err()
}

// inspectSandboxWorkspaceAfterPinned walks the pinned workspace after execution.
// It restores temporary owner grants on any exit and reports failed inspections.
func inspectSandboxWorkspaceAfterPinned(ctx context.Context, root string, pin *os.File, screen *outputScreen) (l sandboxGitLayout) {
	l = sandboxGitLayout{root: root, afterWalk: true, collect: true, access: newOwnerAccess(pin), screen: screen}
	l.access.screen = screen
	defer func() {
		l.warnings = append(l.warnings, l.access.restore()...)
		l.access.close()
		l.access = nil
	}()
	base := filepath.Base(root)
	if pin != nil {
		l.workspacePath = pin.Name()
		base = filepath.Base(pin.Name())
	}
	l.noteAdministrativeRoot(base)
	l.walkAfter(ctx, root)
	// A directory may allow named lookups even though it cannot be listed.
	for i := 0; i < len(l.unreadableDirs) && ctx.Err() == nil; i++ {
		u := l.unreadableDirs[i]
		fi, err := os.Lstat(u)
		if err != nil || !fi.IsDir() {
			continue
		}
		if !l.lookupDirs[filepath.Clean(u)] {
			l.lookupGitNames(u)
		}
		child := filepath.Join(u, ".git")
		if fi, err := os.Lstat(child); err == nil && fi.IsDir() {
			l.walkAfter(ctx, child)
		}
	}
	if ctx.Err() == nil && l.scanObservedDirs(ctx) == nil {
		l.finish(ctx)
	}
	return l
}

// walkAfter is the after-walk from start, the root or a .git looked up in an
// unlisted directory. Directory entries guide descent and visit records the
// identities resolved by Git's names.
func (l *sandboxGitLayout) walkAfter(ctx context.Context, start string) {
	_ = filepath.WalkDir(start, func(name string, entry fs.DirEntry, err error) error {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		if err != nil {
			switch {
			case entry != nil: // a directory it could not list; what it did list still follows
				if !errors.Is(err, fs.ErrNotExist) {
					l.unreadableDirs = append(l.unreadableDirs, name)
					l.unlist(name, err)
				}
			case name == l.root: // the workspace itself could not be read
				l.unlist(name, err)
			} // otherwise a looked-up .git that is absent, inside a directory already unlisted
			return nil
		}
		empty := entry.IsDir() && l.openUp(name)
		l.visit(name, entry.Type(), empty)
		return nil
	})
}

// gitLookupPath is the path the post-run check stats to look up git's own
// name in dir, as git does: filepath.Join. A test sets it to resolve a name
// as a casefold directory would, so the lookup finds another spelling.
var gitLookupPath = func(dir, name string) string { return filepath.Join(dir, name) }

// noteAdministrativeRoot recognizes a workspace opened directly at a Git directory.
func (l *sandboxGitLayout) noteAdministrativeRoot(base string) {
	if base != ".git" {
		return
	}
	id, err := lookupID(l.root)
	if err != nil {
		l.inspectionError(l.root, l.root, err)
		return
	}
	if id.Mode == fs.ModeDir {
		p := filepath.Clean(l.root)
		l.dotGitDirs = append(l.dotGitDirs, p)
		l.ids = map[string]gitEntryID{p: id}
	}
}

// visit records a directory's named lookups or the exact entries listed by its parent.
func (l *sandboxGitLayout) visit(p string, kind fs.FileMode, empty bool) {
	parent := filepath.Dir(p)
	if p != l.root && !l.lookupDirs[parent] {
		switch filepath.Base(p) {
		case "HEAD":
			l.noteLayout(parent)
		case ".git":
			l.observeDotGit(parent)
		}
	}
	if !kind.IsDir() {
		return
	}
	if p != l.root && l.lookupDirs[parent] {
		l.listedDirectory(parent, p)
	}
	if !empty && l.usesLookup(p) {
		l.lookupGitNames(p)
	}
}

// lookupGitNames inspects the same names git resolves in dir.
func (l *sandboxGitLayout) lookupGitNames(dir string) {
	l.observeDotGit(dir)
	l.noteLayout(dir)
}

// observeDotGit records the directory, gitfile or link resolved by the .git lookup.
func (l *sandboxGitLayout) observeDotGit(dir string) {
	p, id, ok := l.observe(dir, ".git")
	if !ok {
		return
	}
	switch id.Mode {
	case fs.ModeDir:
		l.dotGitDirs = append(l.dotGitDirs, p)
	case 0:
		l.gitFiles = append(l.gitFiles, p)
	case fs.ModeSymlink:
		l.dotGitLinks = append(l.dotGitLinks, p)
	default:
		delete(l.ids, p)
	}
}

// scanObservedDirs collects the administrative structure for each resolved .git.
func (l *sandboxGitLayout) scanObservedDirs(ctx context.Context) error {
	for _, dir := range l.dotGitDirs {
		if err := scanGitDir(ctx, dir, l); err != nil {
			return err
		}
	}
	return nil
}

// usesLookup also recognizes a directory reached later through Git's spelling.
func (l *sandboxGitLayout) usesLookup(dir string) bool {
	p := filepath.Clean(dir)
	if fold, seen := l.lookupDirs[p]; seen {
		return fold
	}
	if l.lookupDirs == nil {
		l.lookupDirs = make(map[string]bool)
	}
	fold := l.mayFoldNames(dir)
	l.lookupDirs[p] = fold
	return fold
}

// directoryKey identifies the parent and its entry through one parent descriptor.
func (l *sandboxGitLayout) directoryKey(dir, lookup string) (gitDirKey, error) {
	if dir == filepath.Clean(l.root) {
		dir = l.root // keep the trailing /. on a pinned root
	}
	fd, err := unix.Open(dir, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return gitDirKey{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	var parent, child unix.Stat_t
	if err := unix.Fstat(fd, &parent); err != nil {
		return gitDirKey{}, err
	}
	if err := unix.Fstatat(fd, filepath.Base(lookup), &child, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return gitDirKey{}, err
	}
	if child.Mode&unix.S_IFMT != unix.S_IFDIR {
		return gitDirKey{}, errSandboxWorkspaceChanged
	}
	return gitDirKey{
		parent: gitEntryID{Dev: uint64(parent.Dev), Ino: uint64(parent.Ino), Mode: fs.ModeDir},
		entry:  gitEntryID{Dev: uint64(child.Dev), Ino: uint64(child.Ino), Mode: fs.ModeDir},
	}, nil
}

// listedDirectory indexes the listing spelling, including listings reached via an alias.
func (l *sandboxGitLayout) listedDirectory(dir, p string) gitDirKey {
	if key, ok := l.listedDirs[p]; ok {
		return key
	}
	key, err := l.directoryKey(dir, p)
	if err != nil {
		l.inspectionError(p, dir, err)
		return gitDirKey{}
	}
	if l.listedDirs == nil {
		l.listedDirs = make(map[string]gitDirKey)
	}
	l.listedDirs[p] = key
	return key
}

// nameDirectory associates the identity resolved by a lookup with Git's spelling.
func (l *sandboxGitLayout) nameDirectory(dir, name, lookup string, id gitEntryID) {
	key, err := l.directoryKey(dir, lookup)
	if err == nil && key.entry != id {
		err = errSandboxWorkspaceChanged
	}
	if err != nil {
		l.inspectionError(filepath.Join(dir, name), dir, err)
		return
	}
	if l.namedDirs == nil {
		l.namedDirs = make(map[gitDirKey]string)
	}
	l.namedDirs[key] = name
}

// observe records a Git lookup under its own name. Descent uses that lookup too.
func (l *sandboxGitLayout) observe(dir, name string) (string, gitEntryID, bool) {
	p := filepath.Join(dir, name)
	lookup := p
	fold := l.usesLookup(dir)
	if fold {
		lookup = gitLookupPath(dir, name)
	}
	id, err := lookupID(lookup)
	if err != nil {
		l.inspectionError(p, dir, err)
		return p, gitEntryID{}, false
	}
	if l.ids == nil {
		l.ids = make(map[string]gitEntryID)
	}
	l.ids[p] = id
	if fold && id.Mode == fs.ModeDir {
		l.nameDirectory(dir, name, lookup, id)
		return lookup, id, true
	}
	return p, id, true
}

// canonical spells listed components by their matching Git lookups. Callers
// share names across a batch only after its listings and lookups are complete.
func (l *sandboxGitLayout) canonical(p string, names map[string]string) string {
	if len(l.namedDirs) == 0 || p == l.root {
		return p
	}
	if name, ok := names[p]; ok {
		return name
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	prefix := l.canonical(parent, names)
	name := l.namedDirs[l.listedDirs[p]]
	base := filepath.Base(p)
	if name == "" {
		name = base
	}
	resolved := p
	if prefix != parent || name != base {
		resolved = filepath.Join(prefix, name)
	}
	names[p] = resolved
	return resolved
}

// inspectionError keeps the before observation unknown or reports the unchecked directory.
func (l *sandboxGitLayout) inspectionError(p, dir string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if l.afterWalk {
		l.unlist(dir, err)
	} else if !errors.Is(err, unix.ENOTDIR) {
		if l.unknown == nil {
			l.unknown = make(map[string]string)
		}
		p = filepath.Clean(p)
		if _, seen := l.unknown[p]; !seen {
			l.unknown[p] = inspectionReason(err)
		}
	}
}

// openUp tries listing the directory and searching its entries before requesting
// owner access. An empty listing needs no child lookup.
func (l *sandboxGitLayout) openUp(dir string) (empty bool) {
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm()&0o500 == 0o500 {
		return false
	}
	rel, err := filepath.Rel(l.root, dir)
	if err != nil {
		return false
	}
	empty, err = l.access.openUpPath(filepath.ToSlash(rel))
	if errors.Is(err, errAccessSpecial) {
		l.unlist(dir, err)
	} else if errors.Is(err, errAccessMoved) || errors.Is(err, errAccessLimit) || errors.Is(err, errAccessNotOwned) {
		l.why(dir, err)
	}
	return empty
}

// unlist records dir as a directory the after-walk could not list or search,
// for the reason err (why).
func (l *sandboxGitLayout) unlist(dir string, err error) {
	if l.unlistedSet == nil {
		l.unlistedSet = make(map[string]bool)
	}
	if !l.unlistedSet[dir] {
		l.unlistedSet[dir] = true
		l.unlisted = append(l.unlisted, dir)
	}
	l.why(dir, err)
}

// why records err as the reason for dir unless one is already recorded,
// without the path a file-system error carries, so a note can spell the path
// itself.
func (l *sandboxGitLayout) why(dir string, err error) {
	if l.unlistedWhy == nil {
		l.unlistedWhy = make(map[string]string)
	}
	if _, ok := l.unlistedWhy[dir]; !ok {
		l.unlistedWhy[dir] = inspectionReason(err)
	}
}

// inspectionReason omits any path embedded by a filesystem operation.
func inspectionReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// noteEntry collects the original pre-spawn .git mount candidates.
func (l *sandboxGitLayout) noteEntry(name, base string, kind fs.FileMode) {
	if strings.EqualFold(base, ".git") {
		switch {
		case kind.IsDir():
			l.dotGitDirs = append(l.dotGitDirs, name)
		case kind.IsRegular():
			l.gitFiles = append(l.gitFiles, name)
		}
	}
}

// noteLayout records HEAD's identity even in an incomplete layout.
func (l *sandboxGitLayout) noteLayout(dir string) {
	lookup := func(name string) string {
		if l.lookupDirs[filepath.Clean(dir)] {
			return gitLookupPath(dir, name)
		}
		return filepath.Join(dir, name)
	}
	key := filepath.Join(dir, "HEAD")
	head := lookup("HEAD")
	id, err := lookupID(head)
	if err != nil {
		l.inspectionError(key, dir, err)
		return
	}
	if l.headIDs == nil {
		l.headIDs = make(map[string]gitEntryID)
	}
	l.headIDs[key] = id
	valid, err := readGitHEAD(head, id)
	if err != nil {
		l.inspectionError(key, dir, err)
		return
	}
	if !valid {
		return
	}
	layout, err := l.gitLayoutMarkers(dir, lookup)
	if err != nil {
		if !errors.Is(err, unix.ENOTDIR) {
			l.inspectionError(key, dir, err)
		}
	} else if layout {
		l.layoutHEADs = append(l.layoutHEADs, key)
	}
}

// lookupID identifies a resolved entry without following its final symlink.
func lookupID(p string) (gitEntryID, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(unix.AT_FDCWD, p, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return gitEntryID{}, err
	}
	id := gitEntryID{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Mode: statType(uint32(st.Mode))}
	if id.Mode == fs.ModeSymlink {
		target, err := gitReadlink(p)
		if err != nil {
			return gitEntryID{}, err
		}
		id.Target = target
	}
	return id, nil
}

// gitReadlink and openGitHEAD allow tests to inject lookup and read failures.
var gitReadlink = os.Readlink
var openGitHEAD = os.OpenFile

// readGitHEAD checks the observed entry against Git's HEAD syntax. A missing
// file returns ENOENT, invalid content returns false, and an inspection
// failure retains its error. Named pipes and other special files are skipped.
func readGitHEAD(p string, id gitEntryID) (bool, error) {
	if id.Mode == fs.ModeSymlink {
		return strings.HasPrefix(id.Target, "refs/"), nil
	}
	if !id.Mode.IsRegular() {
		return false, nil
	}
	f, err := openGitHEAD(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false, nil
	}
	if uint64(st.Dev) != id.Dev || uint64(st.Ino) != id.Ino {
		return false, errSandboxWorkspaceChanged
	}
	b, err := io.ReadAll(io.LimitReader(f, 255))
	if err != nil {
		return false, err
	}
	if i := bytes.IndexByte(b, 0); i >= 0 { // git compares C strings
		b = b[:i]
	}
	if rest, ok := bytes.CutPrefix(b, []byte("ref:")); ok {
		// git's isspace (sane_ctype): space, \t, \n and \r, not \v or \f.
		if bytes.HasPrefix(bytes.TrimLeft(rest, " \t\n\r"), []byte("refs/")) {
			return true, nil
		}
	}
	if len(b) < 40 {
		return false, nil
	}
	for _, c := range b[:40] {
		if strings.IndexByte("0123456789abcdefABCDEF", c) < 0 {
			return false, nil
		}
	}
	return true, nil
}

// finish sorts and de-duplicates every list, files each of layoutHEADs'
// directories under repoLayouts or innerLayouts by whether a .git directory
// holds it, and takes the identity (ids) of each entry the post-run check
// compares. A failed stat leaves the entry without one; the after-walk also
// records the entry's directory in unlisted unless the entry is gone. Its
// walks cost the entries times their depth (pathTree).
func (l *sandboxGitLayout) finish(ctx context.Context) {
	sortCompact := func(lists ...*[]string) {
		for _, s := range lists {
			if ctx.Err() != nil {
				return
			}
			slices.Sort(*s)
			*s = slices.Compact(*s)
		}
	}
	lists := []*[]string{&l.dotGitDirs, &l.gitDirs, &l.containers, &l.intermediates, &l.gitFiles, &l.metadata,
		&l.moduleReadOnly, &l.dotGitLinks, &l.layoutHEADs, &l.containerLinks, &l.grantReachedHEADs}
	names := make(map[string]string)
	if len(l.namedDirs) != 0 {
		for _, m := range []*map[string]gitEntryID{&l.ids, &l.headIDs} {
			keys := make(map[string]gitEntryID, len(*m))
			for p, id := range *m {
				if ctx.Err() != nil {
					return
				}
				keys[l.canonical(p, names)] = id
			}
			*m = keys
		}
	}
	if l.collect {
		if l.ids == nil {
			l.ids = make(map[string]gitEntryID)
		}
		recordDir := func(dir string) {
			key := l.canonical(dir, names)
			if _, ok := l.ids[key]; ok {
				return
			}
			p := dir
			if dir == filepath.Clean(l.root) {
				p = l.root
			}
			id, err := lookupID(p)
			if err != nil {
				l.inspectionError(dir, filepath.Dir(dir), err)
				return
			}
			if id.Mode != fs.ModeDir {
				l.inspectionError(dir, filepath.Dir(dir), errSandboxWorkspaceChanged)
				return
			}
			l.ids[key] = id
		}
		for _, h := range l.layoutHEADs {
			if ctx.Err() != nil {
				return
			}
			recordDir(filepath.Dir(h))
		}
		for _, p := range l.gitDirs {
			if ctx.Err() != nil {
				return
			}
			recordDir(p)
		}
	}
	if len(l.namedDirs) != 0 {
		for _, list := range lists {
			for i, p := range *list {
				if ctx.Err() != nil {
					return
				}
				(*list)[i] = l.canonical(p, names)
			}
		}
	}
	sortCompact(lists...)
	if !l.collect {
		return
	}
	var tree pathTree
	dirs := make([]int, len(l.layoutHEADs))
	for i, h := range l.layoutHEADs {
		if ctx.Err() != nil {
			return
		}
		dirs[i] = tree.node(filepath.Dir(h))
	}
	dotGit := tree.marks(l.dotGitDirs)
	for _, d := range dirs {
		if ctx.Err() != nil {
			return
		}
		if tree.up(d, dotGit) < 0 {
			l.repoLayouts = append(l.repoLayouts, tree.path[d])
		} else {
			l.innerLayouts = append(l.innerLayouts, tree.path[d])
		}
	}
	sortCompact(&l.repoLayouts, &l.innerLayouts)
	if len(l.unknown) > 0 && len(l.namedDirs) > 0 {
		keys := make(map[string]string, len(l.unknown))
		for p, why := range l.unknown {
			if ctx.Err() != nil {
				return
			}
			keys[l.canonical(p, names)] = why
		}
		l.unknown = keys
	}
	l.lookupDirs, l.namedDirs, l.listedDirs, l.unlistedSet, l.unreadableDirs = nil, nil, nil, nil, nil
	sortCompact(&l.unlisted)
	l.lookupPolicies = nil
}

// pathTree numbers clean absolute paths and all their ancestors once, so a
// walk up from a path follows parent indices instead of rebuilding and hashing
// each ancestor's string. Adding a path hashes it, and each ancestor it
// creates, once; a walk costs the path's depth. Mark slices (marks) must be
// taken after the last node is added. Paths must be absolute, as the walks'
// are: a relative path's top has no parent (not ".").
type pathTree struct {
	index  map[string]int
	parent []int    // the parent's index; -1 above the top
	path   []string // each node's path
}

// node returns p's index, adding p and every ancestor not yet present.
func (t *pathTree) node(p string) int {
	if t.index == nil {
		t.index = make(map[string]int)
	}
	var missing []string
	parent := -1
	for q := p; ; {
		if i, ok := t.index[q]; ok {
			parent = i
			break
		}
		missing = append(missing, q)
		j := strings.LastIndexByte(q, '/')
		if j < 0 || q == "/" {
			break
		}
		q = q[:max(j, 1)] // the parent of "/a" is "/"
	}
	for k := len(missing) - 1; k >= 0; k-- {
		t.index[missing[k]] = len(t.parent)
		t.parent = append(t.parent, parent)
		t.path = append(t.path, missing[k])
		parent = len(t.parent) - 1
	}
	return parent
}

// marks returns a mark per node, set for each of paths that is a node. A path
// that is not one is no node's ancestor, so no walk could reach its mark.
func (t *pathTree) marks(paths []string) []bool {
	marked := make([]bool, len(t.parent))
	for _, p := range paths {
		if i, ok := t.index[p]; ok {
			marked[i] = true
		}
	}
	return marked
}

// up returns the nearest of node i and its ancestors that marked holds, or -1
// (also for i == -1).
func (t *pathTree) up(i int, marked []bool) int {
	for ; i >= 0; i = t.parent[i] {
		if marked[i] {
			return i
		}
	}
	return -1
}

// scanGitDir records git dir d's metadata, its linked-worktree git dirs and
// its submodule git dirs (recursively), bounded by the git-dir structure
// rather than the workspace. Each directory under <d>/modules is classified
// by descendModules. Absent entries are skipped; any other error fails
// closed, except in the after-walk (unreadable).
//
// Residual R9: a .git that is a symlink, and a metadata entry or a file inside
// hooks or info that is a symlink or hard link to a workspace file, are
// user-chosen in-tree layouts; their targets are ordinary workspace files,
// outside this protection like core.hooksPath into the workspace. The same
// holds for a <d>/modules or <d>/worktrees that is a symlink (or not a
// directory) and for a symlinked child of either container: the scan skips
// them, so on bwrap such a container or git dir is as unprotected as one
// absent at spawn (R1). A symlinked metadata entry itself (hooks, info,
// config, ...; not a file inside hooks or info) is the exception on bwrap: it
// fails the spawn (bwrapRejectSymlinkedGitMetadata), because its mount would
// cover the target and leave the link replaceable.
func scanGitDir(ctx context.Context, d string, out *sandboxGitLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"hooks", "info", "config", "config.worktree", "commondir", "gitdir"} {
		if out.collect {
			if p, _, ok := out.observe(d, name); ok {
				out.metadata = append(out.metadata, p)
			}
			continue
		}
		p := filepath.Join(d, name)
		if _, err := os.Lstat(p); err == nil {
			out.metadata = append(out.metadata, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return out.scanErr(p, err)
		}
	}
	container := func(name string) string {
		if !out.collect {
			return filepath.Join(d, name)
		}
		p, id, ok := out.observe(d, name)
		if ok {
			switch id.Mode {
			case fs.ModeSymlink:
				out.containerLinks = append(out.containerLinks, p)
			case fs.ModeDir:
				return p
			default:
				delete(out.ids, p)
			}
		}
		return ""
	}
	worktrees := container("worktrees")
	entries, ok, err := out.listDir(worktrees)
	if err != nil {
		return err
	}
	if ok {
		out.containers = append(out.containers, worktrees)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			c := filepath.Join(worktrees, e.Name())
			if out.collect && out.usesLookup(worktrees) {
				key := out.listedDirectory(worktrees, c)
				if n := out.namedDirs[key]; n != "" {
					c = gitLookupPath(worktrees, n)
				}
			}
			out.gitDirs = append(out.gitDirs, c)
			if err := scanGitDir(ctx, c, out); err != nil {
				return err
			}
		}
	}
	modules := container("modules")
	entries, ok, err = out.listDir(modules)
	if err != nil || !ok {
		return err
	}
	out.containers = append(out.containers, modules)
	return out.descendModules(ctx, modules, entries)
}

// descendModules classifies each child directory c of dir, a modules
// container or an intermediate under one, in order:
//  1. c/HEAD is a non-directory: a working git dir, scanned (scanGitDir);
//  2. c is named hooks or info (ASCII case-folded): moduleReadOnly, which
//     both backends make read-only whole, not descended;
//  3. c holds any other non-directory entry (hostLitter aside): a broken git
//     dir, scanned;
//  4. otherwise: an intermediate of a slash-named submodule, descended.
//
// A git dir a command stripped of HEAD and every file (writable on both
// backends) thus becomes an intermediate at the next spawn whose hooks and
// info stay read-only, and slash-named siblings are always found. A
// submodule git dir itself named hooks or info (.git/modules/hooks,
// libs/Hooks) whose HEAD a command deleted is caught by clause 2 and made
// read-only whole, so its own hooks cannot be planted at the next spawn.
func (l *sandboxGitLayout) descendModules(ctx context.Context, dir string, entries []fs.DirEntry) error {
	fold := l.collect && l.usesLookup(dir)
	if fold {
		for _, name := range []string{"hooks", "info"} {
			lookup := gitLookupPath(dir, name)
			id, err := lookupID(lookup)
			if err != nil {
				l.inspectionError(filepath.Join(dir, name), dir, err)
			} else if id.Mode == fs.ModeDir {
				l.nameDirectory(dir, name, lookup, id)
			}
		}
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() {
			continue
		}
		c := filepath.Join(dir, e.Name())
		name := e.Name()
		readOnly := name == "hooks" || name == "info"
		if fold {
			key := l.listedDirectory(dir, c)
			n := l.namedDirs[key]
			readOnly = n == "hooks" || n == "info"
			if n != "" {
				c = gitLookupPath(dir, n)
			}
		} else if !l.collect {
			readOnly = len(name) == len("hooks") && strings.EqualFold(name, "hooks") ||
				len(name) == len("info") && strings.EqualFold(name, "info")
		}
		kids, ok, err := l.listDir(c)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		hasHEAD := false
		if l.collect && l.usesLookup(c) {
			id, err := lookupID(gitLookupPath(c, "HEAD"))
			hasHEAD = err == nil && id.Mode != fs.ModeDir
			if err != nil {
				l.inspectionError(c, c, err)
			}
		} else {
			hasHEAD = slices.ContainsFunc(kids, func(k fs.DirEntry) bool { return k.Name() == "HEAD" && !k.IsDir() })
		}
		switch {
		case hasHEAD:
			l.gitDirs = append(l.gitDirs, c)
			err = scanGitDir(ctx, c, l)
		case readOnly:
			l.moduleReadOnly = append(l.moduleReadOnly, c)
		case slices.ContainsFunc(kids, func(k fs.DirEntry) bool { return !k.IsDir() && !hostLitter(k.Name()) }):
			l.gitDirs = append(l.gitDirs, c)
			err = scanGitDir(ctx, c, l)
		default:
			l.intermediates = append(l.intermediates, c)
			err = l.descendModules(ctx, c, kids)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// hostLitter reports a file macOS leaves in a directory the user browsed or
// copied: Finder's .DS_Store and AppleDouble ._* files. descendModules clause
// 3 ignores them, or a stray one in an intermediate would make it a broken git
// dir and hide its sibling submodules from both backends. A sandboxed command
// cannot create a file in an intermediate (Seatbelt denies its direct
// children; bwrap mounts it read-only); one it leaves in a git dir stripped of
// HEAD and every other file only makes that dir an intermediate, as stripping
// alone does.
func hostLitter(name string) bool {
	return name == ".DS_Store" || strings.HasPrefix(name, "._")
}

// listDir reads p when it is a directory (not a symlink to one); ok is false
// when p is absent or not a directory. In the after-walk, a directory it
// cannot read is still a directory: ok, with no entries.
func (l *sandboxGitLayout) listDir(p string) (entries []fs.DirEntry, ok bool, err error) {
	if p == "" {
		return nil, false, nil
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, l.unreadable(p, filepath.Dir(p), err)
	}
	if !info.IsDir() {
		return nil, false, nil
	}
	entries, err = os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		if err := l.unreadable(p, p, err); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	return entries, true, nil
}

// unreadable fails the before-walk on p (scanErr). The after-walk instead
// records dir, the directory it could not list or search, and carries on.
func (l *sandboxGitLayout) unreadable(p, dir string, err error) error {
	if !l.afterWalk {
		if l.collect {
			l.inspectionError(p, dir, err)
			return nil
		}
		return l.scanErr(p, err)
	}
	l.unlist(dir, err)
	return nil
}

// scanErr fails the inspection naming p relative to the workspace root.
func (l *sandboxGitLayout) scanErr(p string, err error) error {
	rel := p
	if r, relErr := filepath.Rel(l.root, p); l.root != "" && relErr == nil {
		rel = filepath.ToSlash(r)
	}
	return fmt.Errorf("tools: sandbox inspect git metadata %q: %w", rel, err)
}

// statType is st_mode's file type in fs.FileMode type bits, as os.Lstat
// spells it (gitEntryID.Mode).
func statType(m uint32) fs.FileMode {
	switch m & unix.S_IFMT {
	case unix.S_IFDIR:
		return fs.ModeDir
	case unix.S_IFLNK:
		return fs.ModeSymlink
	case unix.S_IFIFO:
		return fs.ModeNamedPipe
	case unix.S_IFSOCK:
		return fs.ModeSocket
	case unix.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		return fs.ModeDevice
	}
	return 0 // a regular file
}
