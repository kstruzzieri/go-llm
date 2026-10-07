//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const supportsScopedDispatch = true

// workspaceRoot borrows an invocation capability or opens and validates a
// temporary parent root. Identity metadata alone does not prevent inode reuse.
func (w *Workspace) workspaceRoot() (*os.File, func(), error) {
	if w.pinnedRoot != nil {
		return w.pinnedRoot, func() {}, nil
	}
	fd, err := unix.Open(w.root, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, workspaceOpenError(err)
	}
	f := os.NewFile(uintptr(fd), w.root)
	fi, err := f.Stat()
	if err == nil && !os.SameFile(w.rootIdentity, fi) {
		err = ErrRootReplaced
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

func workspaceOpenError(err error) error {
	if errors.Is(err, unix.ELOOP) {
		return errSymlink
	}
	return err
}

// verifyReachable is the post-decision check (#613). After the guard allowed
// rel and the caller opened or enumerated it, rel must still resolve from the
// top-level workspace root, by name and never through a symlink, to the object
// the caller holds. It is a sequence of lookups, not an atomic snapshot. Linux
// looks up one name at a time; on Darwin one lookup covers a run of names and
// refuses a symlink in any of them (O_NOFOLLOW_ANY).
//
// The lookup first reaches this workspace's own root: the top-level root, then
// a scoped child's prefix down to its pinned scope directory. Every failure
// there is anchor-level and aborts walkers, search included, instead of
// skipping every remaining file: the root renamed away, replaced or turned
// into a symlink reports ErrRootReplaced, and any other error (lost
// permission, say) is wrapped with errAnchorUnreachable around its cause. A
// name below the own root that no longer reaches the held object reports
// errFileChanged.
func (w *Workspace) verifyReachable(rel string, opened *os.File) error {
	var held unix.Stat_t
	if err := unix.Fstat(int(opened.Fd()), &held); err != nil {
		return err
	}
	root, identity, prefix := w.readAnchor()
	dir, err := unix.Open(root, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return reachabilityError(err, true)
	}
	defer func() { _ = unix.Close(dir) }()
	var st unix.Stat_t
	if err := unix.Fstat(dir, &st); err != nil {
		return reachabilityError(err, true)
	} else if !sameIdentity(identity, &st) {
		return ErrRootReplaced
	}
	parts := splitClean(prefix)
	inRoot := len(parts) // parts[:inRoot] lead from the anchor to this workspace's own root
	parts = append(parts, splitClean(rel)...)
	if len(parts) == 0 {
		err = unix.Fstat(dir, &st)
	} else {
		dirs := parts[:len(parts)-1]
		// Up to and including the scope directory, a failure is anchor-level.
		if scope := min(inRoot, len(dirs)); scope > 0 {
			if err := descend(&dir, dirs[:scope]); err != nil {
				return reachabilityError(err, true)
			}
			if scope == inRoot {
				// The scope's name may now hold another directory.
				if err := unix.Fstat(dir, &st); err != nil {
					return reachabilityError(err, true)
				} else if !sameIdentity(w.rootIdentity, &st) {
					return ErrRootReplaced
				}
			}
			dirs = dirs[scope:]
		}
		if len(dirs) > 0 {
			if err := descend(&dir, dirs); err != nil {
				return reachabilityError(err, false)
			}
		}
		err = unix.Fstatat(dir, parts[len(parts)-1], &st, unix.AT_SYMLINK_NOFOLLOW)
	}
	// When rel names this workspace's own root, the leaf is that root.
	ownRoot := len(parts) == inRoot
	if err != nil {
		return reachabilityError(err, ownRoot)
	}
	if st.Dev != held.Dev || st.Ino != held.Ino {
		if ownRoot {
			return ErrRootReplaced
		}
		return errFileChanged
	}
	return nil
}

// descend replaces *dir with the directory that comps reach from it, never
// following a symlink in any component. Where the platform can refuse a symlink
// anywhere in a path (Darwin O_NOFOLLOW_ANY), one openat covers the run; a run
// too long for one path (ENAMETOOLONG) is opened one name at a time instead,
// still without following symlinks.
func descend(dir *int, comps []string) error {
	if workspaceNoFollowAny != 0 && len(comps) > 1 {
		next, err := unix.Openat(*dir, strings.Join(comps, string(os.PathSeparator)), workspaceSearchFlags|workspaceNoFollowAny|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(*dir)
			*dir = next
			return nil
		}
		if !errors.Is(err, unix.ENAMETOOLONG) {
			return err
		}
	}
	for _, c := range comps {
		next, err := unix.Openat(*dir, c, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		_ = unix.Close(*dir)
		*dir = next
	}
	return nil
}

// sameIdentity reports whether a construction-time identity (from os.Stat or
// File.Stat) names the object st describes. Any other FileInfo fails closed.
func sameIdentity(fi os.FileInfo, st *unix.Stat_t) bool {
	s, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(s.Dev) == uint64(st.Dev) && uint64(s.Ino) == uint64(st.Ino)
}

// reachabilityError classifies a failed lookup by where it failed. On the way
// to the workspace's own root (ownRoot), a lookup that no longer reaches its
// directory (nothing there, not a directory, or a symlink) is ErrRootReplaced,
// and any other error is wrapped with errAnchorUnreachable, keeping the cause's
// tool text. Below the own root, such a lookup is errFileChanged, whose
// model-visible text names no location, and other errors are returned
// unchanged.
func reachabilityError(err error, ownRoot bool) error {
	changed := errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP)
	switch {
	case ownRoot && changed:
		return ErrRootReplaced
	case ownRoot:
		return fmt.Errorf("%w: %w", errAnchorUnreachable, err)
	case changed:
		return errFileChanged
	}
	return err
}

// splitClean splits a relative path into its components, dropping "" and "."
// segments.
func splitClean(p string) []string {
	var parts []string
	for _, part := range strings.Split(p, string(os.PathSeparator)) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// openRead pins each component before consulting policy on the final canonical
// logical path, and opens content only through those pinned descriptors. After
// the guard allows, verifyReachable separately reopens the top-level anchor by
// path to confirm the guarded name still reaches the opened object.
func (w *Workspace) openRead(p string, directory bool) (*os.File, string, error) {
	return w.openReadMode(p, directory, false)
}

// pinScope retains a search-only handle for the scope root: the child borrows
// the least-privileged descriptor that still anchors relative opens.
func (w *Workspace) pinScope(p string) (*os.File, string, error) {
	return w.openReadMode(p, true, true)
}

func (w *Workspace) openReadMode(p string, directory, pin bool) (file *os.File, canonical string, resultErr error) {
	defer func() { resultErr = w.scopedPathError(resultErr) }()
	abs, err := w.cleanRel(p)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return nil, "", err
	}
	// Resolve policy spelling from pinned components as far as possible. On a
	// filesystem failure, retain the cleaned suffix and still consult the guard
	// exactly once, so denied names do not disclose existence/type/accessibility.
	// A name already denied as unverifiable never reaches the guard: the caller's
	// spelling is not policy evidence.
	policyRel := rel
	defer func() {
		if errors.Is(resultErr, errScopeDenied) {
			return
		}
		err := w.checkScope(filepath.Join(w.root, policyRel), false)
		if err == nil && file != nil {
			// #613: the decision binds only if the guarded path still reaches
			// the opened object after the guard returned.
			err = w.verifyReachable(canonical, file)
		}
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			file, canonical, resultErr = nil, "", err
		}
	}()
	root, release, err := w.workspaceRoot()
	if err != nil {
		return nil, "", err
	}
	defer release()
	parent := root
	defer func() {
		if parent != root {
			_ = parent.Close()
		}
	}()
	parts := strings.Split(rel, string(os.PathSeparator))
	logical := "."
	for i, part := range parts {
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), part, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return nil, "", err
		}
		name, err := workspaceCanonicalName(parent, part, &st)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				// The parent cannot be enumerated, so the on-disk spelling cannot
				// be proven. Fail closed on every platform rather than trust an
				// alias; descriptor-name metadata is not canonical evidence.
				return nil, "", w.denyScope()
			}
			return nil, "", err
		}
		logical = filepath.Join(logical, name)
		policyRel = filepath.Join(logical, filepath.Join(parts[i+1:]...))
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, "", errSymlink
		}
		last := i == len(parts)-1
		flags := workspaceSearchFlags
		if last && !directory {
			if st.Mode&unix.S_IFMT != unix.S_IFREG {
				return nil, "", errNotRegular
			}
			flags = unix.O_RDONLY | unix.O_NONBLOCK
		} else if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return nil, "", errNotDir
		}
		if w.beforeReadOpen != nil {
			w.beforeReadOpen()
		}
		fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, "", workspaceOpenError(err)
		}
		f := os.NewFile(uintptr(fd), name)
		var opened unix.Stat_t
		if err = unix.Fstat(fd, &opened); err == nil && (st.Dev != opened.Dev || st.Ino != opened.Ino || st.Mode&unix.S_IFMT != opened.Mode&unix.S_IFMT) {
			err = errFileChanged
		}
		if err != nil {
			_ = f.Close()
			return nil, "", err
		}
		if !last {
			if parent != root {
				_ = parent.Close()
			}
			parent = f
			continue
		}
		if directory && pin {
			return f, logical, nil
		}
		if directory {
			readable, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			_ = f.Close()
			if err != nil {
				return nil, "", err
			}
			return os.NewFile(uintptr(readable), name), logical, nil
		}
		if err = unix.SetNonblock(fd, false); err != nil {
			_ = f.Close()
			return nil, "", err
		}
		return f, logical, nil
	}
	return nil, "", errNotDir
}

func (w *Workspace) openRegularFile(p string) (*os.File, error) {
	f, _, err := w.openRead(p, false)
	return f, err
}
func (w *Workspace) openDir(p string) (*os.File, error) {
	f, _, err := w.openRead(p, true)
	return f, err
}
func (w *Workspace) openReadDir(p string) (*os.File, string, error) { return w.openRead(p, true) }

func workspaceCanonicalName(parent *os.File, part string, target *unix.Stat_t) (string, error) {
	if part == "." {
		return part, nil
	}
	fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), ".")
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if name == part {
			return part, nil
		}
	}
	actual := ""
	for _, name := range names {
		if strings.EqualFold(name, part) {
			if actual != "" {
				return "", errFileChanged
			}
			actual = name
		}
	}
	if actual != "" {
		return actual, nil
	}
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
		if st.Dev == target.Dev && st.Ino == target.Ino {
			if actual != "" {
				return "", errFileChanged
			}
			actual = name
		}
	}
	if actual == "" {
		return "", errFileChanged
	}
	return actual, nil
}

// Metadata belongs to the descriptor-relative name lookup. Never pass this
// private FileInfo to os.SameFile, which expects native os stat identities.
type workspaceEntry struct {
	name string
	stat unix.Stat_t
	dir  *os.File // enumerated directory; valid while the enumerator holds it open
}

// openRegular opens the enumerated entry from its pinned directory descriptor.
// Walkers use it instead of re-resolving the path by name, which would
// re-enumerate every ancestor per file (quadratic in directory size). Policy
// was consulted for this entry by the walk; identity is re-checked on the
// opened descriptor exactly as openReadMode does.
func (e workspaceEntry) openRegular() (*os.File, error) {
	if e.stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errNotRegular
	}
	fd, err := unix.Openat(int(e.dir.Fd()), e.name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, workspaceOpenError(err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if opened.Dev != e.stat.Dev || opened.Ino != e.stat.Ino || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, errFileChanged
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), e.name), nil
}

// openWalked opens a walked regular file from its pinned directory descriptor
// and verifies (#613) that rel still reaches it after the walk's guard
// decision. Every Unix walk entry holds its directory; any other entry fails
// closed with errWalkEntryUnheld, as verifyWalkedParent does, because a by-name
// open would consult the guard again and re-list every ancestor.
func (w *Workspace) openWalked(rel string, d fs.DirEntry) (*os.File, error) {
	e, ok := d.(workspaceEntry)
	if !ok {
		return nil, errWalkEntryUnheld
	}
	f, err := e.openRegular()
	if err != nil {
		return nil, err
	}
	if err := w.verifyReachable(rel, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (e workspaceEntry) Name() string               { return e.name }
func (e workspaceEntry) Size() int64                { return e.stat.Size }
func (e workspaceEntry) ModTime() time.Time         { return time.Unix(e.stat.Mtim.Sec, e.stat.Mtim.Nsec) }
func (e workspaceEntry) Sys() any                   { return nil }
func (e workspaceEntry) IsDir() bool                { return e.Mode().IsDir() }
func (e workspaceEntry) Type() fs.FileMode          { return e.Mode().Type() }
func (e workspaceEntry) Info() (fs.FileInfo, error) { return e, nil }
func (e workspaceEntry) Mode() fs.FileMode {
	mode := fs.FileMode(e.stat.Mode & 0777)
	switch e.stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if e.stat.Mode&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if e.stat.Mode&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if e.stat.Mode&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// Entry metadata needs search permission on the directory in addition to
// enumeration read permission; stat-only resolveDir does not require either.
func readWorkspaceEntries(f *os.File) ([]fs.DirEntry, error) {
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	entries := make([]fs.DirEntry, 0, len(names))
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		entries = append(entries, workspaceEntry{name: name, stat: st, dir: f})
	}
	return entries, nil
}

// readDirEntries enumerates a directory the caller opened at rel, then
// verifies (#613) that rel still reaches it, so names read from a directory
// that moved after the guard decided are never returned.
func (w *Workspace) readDirEntries(f *os.File, rel string) ([]fs.DirEntry, error) {
	if w.beforeReadDir != nil {
		w.beforeReadDir(rel)
	}
	entries, err := readWorkspaceEntries(f)
	if err != nil {
		return nil, err
	}
	if err := w.verifyReachable(rel, f); err != nil {
		return nil, err
	}
	return entries, nil
}

// verifyWalkedParent binds a listed name to its held directory after the
// walk's entry guard. Membership and metadata remain an enumeration snapshot.
// Every Unix walk entry holds its directory; any other entry fails closed
// rather than silently skipping the check.
func (w *Workspace) verifyWalkedParent(rel string, d fs.DirEntry) error {
	e, ok := d.(workspaceEntry)
	if !ok {
		return errWalkEntryUnheld
	}
	return w.verifyReachable(filepath.Dir(rel), e.dir)
}

func (w *Workspace) walk(ctx context.Context, fn func(string, fs.DirEntry) error) (resultErr error) {
	defer func() { resultErr = w.scopedPathError(resultErr) }()
	root, release, err := w.workspaceRoot()
	if err != nil {
		return err
	}
	defer release()
	return w.walkDir(ctx, root, "", fn)
}
func (w *Workspace) walkDir(ctx context.Context, parent *os.File, base string, fn func(string, fs.DirEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), ".")
	defer func() { _ = f.Close() }()
	entries, err := w.readDirEntries(f, base)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && ignoreDirs[entry.Name()] {
			continue
		}
		rel := filepath.Join(base, entry.Name())
		if w.checkScope(filepath.Join(w.root, rel), false) != nil {
			continue
		}
		err := fn(filepath.ToSlash(rel), entry)
		if err == fs.SkipDir && entry.IsDir() {
			continue
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			fd, err := unix.Openat(int(f.Fd()), entry.Name(), workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				// #613: enumerated as a directory, so nothing (ENOENT) or a
				// non-directory (ENOTDIR, incl. a symlink under O_DIRECTORY)
				// at this name now is a changed path, as the identity check is.
				return reachabilityError(workspaceOpenError(err), false)
			}
			child := os.NewFile(uintptr(fd), entry.Name())
			var st unix.Stat_t
			err = unix.Fstat(fd, &st)
			prior := entry.(workspaceEntry).stat
			if err == nil && (st.Dev != prior.Dev || st.Ino != prior.Ino) {
				err = errFileChanged
			}
			if err == nil {
				err = w.walkDir(ctx, child, rel, fn)
			}
			_ = child.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
