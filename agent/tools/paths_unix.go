//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		err = errFileChanged
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

// openRead pins each component before consulting policy on the final canonical
// logical path. No subsequent access reopens an ambient absolute pathname.
func (w *Workspace) openRead(p string, directory bool) (*os.File, string, error) {
	return w.openReadMode(p, directory, false)
}

// pinScope retains a search-only handle; delegated known-file reads must not
// require permission to enumerate the delegated directory.
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
	policyRel := rel
	defer func() {
		if err := w.checkScope(filepath.Join(w.root, policyRel), false); err != nil {
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
		name, nameErr := workspaceCanonicalName(parent, part, &st)
		if nameErr != nil {
			if !errors.Is(nameErr, fs.ErrPermission) {
				return nil, "", nameErr
			}
			// Search permission permits a known-file open, but the caller's
			// spelling is not policy evidence on a case-insensitive filesystem.
			name = part
		}
		parentLogical := logical
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
		if nameErr != nil {
			name, err = workspaceDescriptorName(parent, f, part)
			var named, requested unix.Stat_t
			// Do not substitute an unrelated hardlink or a renamed component.
			// The descriptor name must identify this spelling in this parent.
			if err != nil || name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name ||
				unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil ||
				named.Dev != opened.Dev || named.Ino != opened.Ino || named.Mode&unix.S_IFMT != opened.Mode&unix.S_IFMT ||
				unix.Fstatat(int(parent.Fd()), part, &requested, unix.AT_SYMLINK_NOFOLLOW) != nil ||
				requested.Dev != opened.Dev || requested.Ino != opened.Ino || requested.Mode&unix.S_IFMT != opened.Mode&unix.S_IFMT {
				_ = f.Close()
				return nil, "", w.denyScope()
			}
			logical = filepath.Join(parentLogical, name)
			policyRel = filepath.Join(logical, filepath.Join(parts[i+1:]...))
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
		entries = append(entries, workspaceEntry{name: name, stat: st})
	}
	return entries, nil
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
	entries, err := readWorkspaceEntries(f)
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
				return workspaceOpenError(err)
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
