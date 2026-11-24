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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// sandboxGitLayout is the git metadata inside a workspace at spawn time
// (#721). Every path is absolute and canonical-parent spelled (the walk's own
// spelling under the canonical root); every slice is sorted and de-duplicated.
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
	dotGitLinks []string // each symbolic link named .git (ASCII case-folded)
	// layoutHEADs is the HEAD of each directory git would treat as a
	// repository (noteLayout), anywhere in the workspace, git dirs included;
	// it supplies the HEADs a held item must also set aside.
	layoutHEADs []string
	// repoLayouts is each directory of layoutHEADs that is neither a directory
	// named .git nor inside one: a bare repository or a linked-worktree admin
	// directory, for example. One inside another layout is still listed.
	repoLayouts []string
	// innerLayouts is each directory of layoutHEADs at or inside a directory
	// named .git, whatever else it also is: the .git itself, a git dir, a
	// hooks directory, or a command-made .git/evil. entryKinds decides which
	// kind such a path counts as.
	innerLayouts []string
	// ids is the identity of each entry in dotGitDirs, gitFiles, dotGitLinks,
	// gitDirs, containers, metadata, repoLayouts and innerLayouts, by path. An
	// entry that could not be stat'ed has none.
	ids map[string]gitEntryID
	// unlisted is each directory the after-walk could not list or search
	// (inspectSandboxWorkspaceAfter); the before-walk fails instead.
	unlisted []string
	// unlistedWhy is the first reason the after-walk recorded for a
	// directory, by path, with no path in its text.
	unlistedWhy map[string]string
	// warnings are the after-walk's own warnings: a mode it opened up and
	// could not put back.
	warnings []string
	// access opens up, during the after-walk only, a directory the user owns
	// whose mode keeps the walk out (ownerAccess).
	access *ownerAccess

	root      string // the workspace root; errors name paths relative to it
	afterWalk bool   // record unreadable directories in unlisted rather than fail
}

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

func (s *sandboxLinkShortfall) Error() string {
	return fmt.Sprintf("tools: sandbox workspace entry %q is linked outside the workspace", s.first)
}

// inspectSandboxWorkspaceWithWalk runs a fresh walk for each attempt.
func inspectSandboxWorkspaceWithWalk(root string, walk func(string, fs.WalkDirFunc) error) (sandboxGitLayout, error) {
	var previous *sandboxLinkShortfall
	var err error
	for range sandboxInspectPasses {
		var layout sandboxGitLayout
		layout, err = inspectSandboxWorkspaceOnce(root, walk)
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
func inspectSandboxWorkspaceOnce(root string, walk func(string, fs.WalkDirFunc) error) (sandboxGitLayout, error) {
	type inode struct {
		dev uint64
		ino uint64
	}
	type linkRecord struct {
		total uint64
		seen  uint64
		first string
	}

	layout := sandboxGitLayout{root: root}
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
		layout.noteEntry(name, kind)
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
		if err := scanGitDir(d, &layout); err != nil {
			return sandboxGitLayout{}, err
		}
	}
	layout.finish()
	return layout, nil
}

// inspectSandboxWorkspaceAfter walks the workspace after a sandboxed command
// for the post-run check (#802): the same records as the before-walk, but best
// effort, so it never fails. A directory the user owns whose mode keeps the
// walk out (a command can chmod its own) is opened up before it is read
// (openUp) and given its exact mode back when the walk ends, even on a
// panic; a mode it cannot put back is a warning. A directory it still cannot
// list or search goes into unlisted, with the reason, the entries it did list
// are still walked, and in each one it looks up .git (walking it when it is a
// directory) and the repository-layout names HEAD, objects, refs and
// commondir by name, as git would. It neither checks hard links nor retries.
func inspectSandboxWorkspaceAfter(root string) (l sandboxGitLayout) {
	l = sandboxGitLayout{root: root, afterWalk: true, access: newOwnerAccess(root)}
	defer func() {
		l.warnings = append(l.warnings, l.access.restore()...)
		l.access.close()
		l.access = nil
	}()
	l.walkAfter(root)
	for i := 0; i < len(l.unlisted); i++ { // a lookup can add a deeper unlisted directory
		u := l.unlisted[i]
		l.walkAfter(filepath.Join(u, ".git"))
		l.noteLayout(u)
	}
	for _, d := range l.dotGitDirs {
		_ = scanGitDir(d, &l) // the after-walk's scan records failures and never returns one
	}
	l.finish()
	return l
}

// walkAfter is the after-walk from start, the root or a .git looked up in an
// unlisted directory. Entry types come from the listing; ids are taken once
// the walk is done (finish).
func (l *sandboxGitLayout) walkAfter(start string) {
	_ = filepath.WalkDir(start, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			switch {
			case entry != nil: // a directory it could not list; what it did list still follows
				if !errors.Is(err, fs.ErrNotExist) {
					l.unlist(name, err)
				}
			case name == l.root: // the workspace itself could not be read
				l.unlist(name, err)
			} // otherwise a looked-up .git that is absent, inside a directory already unlisted
			return nil
		}
		if entry.IsDir() {
			l.openUp(name)
		}
		l.noteEntry(name, entry.Type())
		return nil
	})
}

// openUp opens up dir, a directory the walk is about to read, when its owner
// can neither list it nor search it and access can (ownerAccess). When access
// declines because dir moved or is past the limit, that is the reason the
// walk gives if it then cannot read dir.
func (l *sandboxGitLayout) openUp(dir string) {
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm()&0o500 == 0o500 {
		return
	}
	rel, err := filepath.Rel(l.root, dir)
	if err != nil {
		return
	}
	if err := l.access.openUpPath(filepath.ToSlash(rel)); errors.Is(err, errAccessMoved) || errors.Is(err, errAccessLimit) {
		l.why(dir, err)
	}
}

// unlist records dir as a directory the after-walk could not list or search,
// for the reason err (why).
func (l *sandboxGitLayout) unlist(dir string, err error) {
	l.unlisted = append(l.unlisted, dir)
	l.why(dir, err)
}

// why records err as the reason for dir unless one is already recorded,
// without the path a file-system error carries, so a note can spell the path
// itself.
func (l *sandboxGitLayout) why(dir string, err error) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if l.unlistedWhy == nil {
		l.unlistedWhy = make(map[string]string)
	}
	if _, ok := l.unlistedWhy[dir]; !ok {
		l.unlistedWhy[dir] = err.Error()
	}
}

// noteEntry records a walked entry: a name folding to .git by its type, and a
// name folding to HEAD as the cue to test its directory as a repository
// layout (noteLayout, which looks up the exact names, so a case-insensitive
// filesystem's spelling counts as git's own lookup would).
func (l *sandboxGitLayout) noteEntry(name string, kind fs.FileMode) {
	base := filepath.Base(name)
	if strings.EqualFold(base, ".git") {
		switch {
		case kind.IsDir():
			l.dotGitDirs = append(l.dotGitDirs, name)
		case kind.IsRegular():
			l.gitFiles = append(l.gitFiles, name)
		case kind&fs.ModeSymlink != 0:
			l.dotGitLinks = append(l.dotGitLinks, name)
		}
	}
	if name != l.root && strings.EqualFold(base, "HEAD") {
		l.noteLayout(filepath.Dir(name))
	}
}

// noteLayout records dir's HEAD in layoutHEADs when git would treat dir as a
// repository: a valid HEAD (validHEAD) plus either objects and refs, or
// commondir. That is git's is_git_directory, except that commondir counts
// without checking where it points. A lookup that fails counts as absent, as
// in git, so neither walk fails or records anything here.
func (l *sandboxGitLayout) noteLayout(dir string) {
	has := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, name))
		return err == nil
	}
	if head := filepath.Join(dir, "HEAD"); validHEAD(head) && (has("objects") && has("refs") || has("commondir")) {
		l.layoutHEADs = append(l.layoutHEADs, head)
	}
}

// validHEAD reports whether git's validate_headref accepts p: a symbolic link
// whose target starts "refs/", or a regular file whose first 255 bytes start
// "ref:", whitespace and "refs/", or with 40 hex digits (a SHA-1 object ID, or
// the start of a SHA-256 one). Anything that cannot be read is not valid. The
// file is opened non-blocking and without following a link, and must still be
// a regular file once open, so a swapped-in FIFO cannot stall the walk.
func validHEAD(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		return err == nil && strings.HasPrefix(target, "refs/")
	}
	if !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, 255))
	if err != nil {
		return false
	}
	if i := bytes.IndexByte(b, 0); i >= 0 { // git compares C strings
		b = b[:i]
	}
	if rest, ok := bytes.CutPrefix(b, []byte("ref:")); ok {
		// git's isspace (sane_ctype): space, \t, \n and \r, not \v or \f.
		if bytes.HasPrefix(bytes.TrimLeft(rest, " \t\n\r"), []byte("refs/")) {
			return true
		}
	}
	if len(b) < 40 {
		return false
	}
	for _, c := range b[:40] {
		if strings.IndexByte("0123456789abcdefABCDEF", c) < 0 {
			return false
		}
	}
	return true
}

// finish sorts and de-duplicates every list, files each of layoutHEADs'
// directories under repoLayouts or innerLayouts by whether a .git directory
// holds it, and takes the identity (ids) of each entry the post-run check
// compares. A failed stat leaves the entry without one; the after-walk also
// records the entry's directory in unlisted unless the entry is gone. Its
// walks cost the entries times their depth (pathTree).
func (l *sandboxGitLayout) finish() {
	sortCompact := func(lists ...*[]string) {
		for _, s := range lists {
			slices.Sort(*s)
			*s = slices.Compact(*s)
		}
	}
	sortCompact(&l.dotGitDirs, &l.gitDirs, &l.containers, &l.intermediates, &l.gitFiles, &l.metadata,
		&l.moduleReadOnly, &l.dotGitLinks, &l.layoutHEADs)
	var tree pathTree
	dirs := make([]int, len(l.layoutHEADs))
	for i, h := range l.layoutHEADs {
		dirs[i] = tree.node(filepath.Dir(h))
	}
	dotGit := tree.marks(l.dotGitDirs)
	for _, d := range dirs {
		if tree.up(d, dotGit) < 0 {
			l.repoLayouts = append(l.repoLayouts, tree.path[d])
		} else {
			l.innerLayouts = append(l.innerLayouts, tree.path[d])
		}
	}
	sortCompact(&l.repoLayouts, &l.innerLayouts)
	l.ids = make(map[string]gitEntryID)
	for _, list := range [][]string{l.dotGitDirs, l.gitFiles, l.dotGitLinks, l.gitDirs, l.containers, l.metadata,
		l.repoLayouts, l.innerLayouts} {
		for _, p := range list {
			if _, done := l.ids[p]; done {
				continue // in an earlier list too: a .git, git dir, metadata entry or container that is also a layout
			}
			fi, err := os.Lstat(p)
			if err != nil {
				if l.afterWalk && !errors.Is(err, fs.ErrNotExist) {
					l.unlist(filepath.Dir(p), err)
				}
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			id := gitEntryID{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Mode: fi.Mode().Type()}
			if id.Mode&fs.ModeSymlink != 0 {
				id.Target, _ = os.Readlink(p) // unreadable: no target, which matches no real one
			}
			l.ids[p] = id
		}
	}
	sortCompact(&l.unlisted)
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
func scanGitDir(d string, out *sandboxGitLayout) error {
	for _, name := range []string{"hooks", "info", "config", "config.worktree", "commondir", "gitdir"} {
		p := filepath.Join(d, name)
		if _, err := os.Lstat(p); err == nil {
			out.metadata = append(out.metadata, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			if err := out.unreadable(p, d, err); err != nil {
				return err
			}
		}
	}
	worktrees := filepath.Join(d, "worktrees")
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
			out.gitDirs = append(out.gitDirs, c)
			if err := scanGitDir(c, out); err != nil {
				return err
			}
		}
	}
	modules := filepath.Join(d, "modules")
	entries, ok, err = out.listDir(modules)
	if err != nil || !ok {
		return err
	}
	out.containers = append(out.containers, modules)
	return out.descendModules(modules, entries)
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
func (l *sandboxGitLayout) descendModules(dir string, entries []fs.DirEntry) error {
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c := filepath.Join(dir, e.Name())
		kids, ok, err := l.listDir(c)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		name := e.Name()
		switch {
		case slices.ContainsFunc(kids, func(k fs.DirEntry) bool { return k.Name() == "HEAD" && !k.IsDir() }):
			l.gitDirs = append(l.gitDirs, c)
			err = scanGitDir(c, l)
		// Equal byte length keeps the fold ASCII: the non-ASCII runes that fold
		// to an ASCII letter (Kelvin K, long s) are multi-byte.
		case len(name) == len("hooks") && strings.EqualFold(name, "hooks"),
			len(name) == len("info") && strings.EqualFold(name, "info"):
			l.moduleReadOnly = append(l.moduleReadOnly, c)
		case slices.ContainsFunc(kids, func(k fs.DirEntry) bool { return !k.IsDir() && !hostLitter(k.Name()) }):
			l.gitDirs = append(l.gitDirs, c)
			err = scanGitDir(c, l)
		default:
			l.intermediates = append(l.intermediates, c)
			err = l.descendModules(c, kids)
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
