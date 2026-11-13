//go:build darwin || linux

package tools

// Shared pre-spawn workspace walk for the pathname/bind-mount sandbox
// backends (#442 Seatbelt, #441 bwrap). Both authorize the workspace by
// pathname (SBPL subpath, bind mount), so an inode reachable through an
// outside name must be rejected before either lifetime spawns. The same walk
// finds the workspace's git metadata, which both backends refuse to let a
// child write (#721).

import (
	"errors"
	"fmt"
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

	root string // the workspace root; errors name paths relative to it
}

// inspectSandboxWorkspace walks the workspace once before a sandboxed spawn.
// It rejects an inode with any directory entry outside root: pathname-based
// sandboxes authorize names, so an allowed workspace hard link would
// otherwise let a child mutate the same inode through an outside name.
// Internal-only hard links remain valid. It also returns the workspace's git
// layout: every entry named .git, then each .git directory's submodule and
// worktree structure (scanGitDir). Any error fails the spawn, so the layout
// is complete or there is none. This is the final host check; an
// unsandboxed same-UID process can still race it by the documented threat
// model.
func inspectSandboxWorkspace(root string) (sandboxGitLayout, error) {
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
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		rel, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return fmt.Errorf("tools: sandbox inspect workspace links: %w", relErr)
		}
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: %w", rel, walkErr)
		}
		if strings.EqualFold(entry.Name(), ".git") {
			switch {
			case entry.IsDir():
				layout.dotGitDirs = append(layout.dotGitDirs, name)
			case entry.Type().IsRegular():
				layout.gitFiles = append(layout.gitFiles, name)
			}
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("tools: sandbox inspect workspace entry %q: %w", rel, err)
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
			return fmt.Errorf("tools: sandbox workspace link count changed while inspecting %q", rel)
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
			return sandboxGitLayout{}, fmt.Errorf("tools: sandbox workspace entry %q is linked outside the workspace", record.first)
		}
		if record.seen > record.total {
			return sandboxGitLayout{}, fmt.Errorf("tools: sandbox workspace link count changed while inspecting %q", record.first)
		}
	}
	for _, d := range layout.dotGitDirs {
		if err := scanGitDir(d, &layout); err != nil {
			return sandboxGitLayout{}, err
		}
	}
	for _, s := range []*[]string{&layout.dotGitDirs, &layout.gitDirs, &layout.containers,
		&layout.intermediates, &layout.gitFiles, &layout.metadata, &layout.moduleReadOnly} {
		slices.Sort(*s)
		*s = slices.Compact(*s)
	}
	return layout, nil
}

// scanGitDir records git dir d's metadata, its linked-worktree git dirs and
// its submodule git dirs (recursively), bounded by the git-dir structure
// rather than the workspace. Each directory under <d>/modules is classified
// by descendModules. Absent entries are skipped; any other error fails
// closed.
//
// Residual R9: a .git that is a symlink, and a hooks directory that is a
// symlink into the tree, are user-chosen in-tree layouts; their targets are
// ordinary workspace files, outside this protection like core.hooksPath into
// the tree. The same holds for a <d>/modules or <d>/worktrees that is a
// symlink (or not a directory) and for a symlinked child of either
// container: the scan skips them, so on bwrap such a container or git dir is
// as unprotected as one absent at spawn (R1). A symlinked metadata entry
// (hooks, info, config, ...) is the exception on bwrap: it fails the spawn
// (bwrapRejectSymlinkedGitMetadata), because its mount would cover the
// target and leave the link replaceable.
func scanGitDir(d string, out *sandboxGitLayout) error {
	for _, name := range []string{"hooks", "info", "config", "config.worktree", "commondir", "gitdir"} {
		p := filepath.Join(d, name)
		if _, err := os.Lstat(p); err == nil {
			out.metadata = append(out.metadata, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return out.scanErr(p, err)
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
// when p is absent or not a directory.
func (l *sandboxGitLayout) listDir(p string) (entries []fs.DirEntry, ok bool, err error) {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, l.scanErr(p, err)
	}
	if !info.IsDir() {
		return nil, false, nil
	}
	entries, err = os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, l.scanErr(p, err)
	}
	return entries, true, nil
}

// scanErr fails the inspection naming p relative to the workspace root.
func (l *sandboxGitLayout) scanErr(p string, err error) error {
	rel := p
	if r, relErr := filepath.Rel(l.root, p); l.root != "" && relErr == nil {
		rel = filepath.ToSlash(r)
	}
	return fmt.Errorf("tools: sandbox inspect git metadata %q: %w", rel, err)
}
