//go:build darwin || linux

package tools

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// gitLayoutMarkers validates the common directory before treating an observed
// HEAD as part of a repository. A broken pointer supersedes local markers,
// just as a valid pointer does. Uninspectable pointers leave the HEAD alone.
func (l *sandboxGitLayout) gitLayoutMarkers(dir string, lookup func(string) string) (bool, error) {
	rel, err := filepath.Rel(l.root, dir)
	if err != nil {
		return false, err
	}
	admin, err := l.openLayoutDir("", rel)
	throughGrant := false
	if errors.Is(err, unix.EACCES) && l.access.hasTemporaryGrants() {
		// Inspection may reach a new repository behind a directory the command
		// locked. Still check the candidate and its Git lookup paths against
		// original permissions, and keep any previously existing HEAD in place.
		admin, err = l.openLayoutDir(rel, "")
		throughGrant = err == nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(admin) }()

	target, present, err := readCommonDir(admin, filepath.Base(lookup("commondir")))
	if err != nil {
		return false, err
	}
	common := admin
	if present {
		if target == "" {
			return false, nil
		}
		prefix := ""
		if filepath.IsAbs(target) {
			host := l.workspacePath
			if host == "" {
				host = l.root
			}
			// Preserve '..' components until each directory has actually been opened.
			host = filepath.Clean(host)
			switch {
			case target == host:
				rel = "."
			case strings.HasPrefix(target, strings.TrimSuffix(host, "/")+"/"):
				rel = strings.TrimPrefix(target, strings.TrimSuffix(host, "/")+"/")
			default:
				return false, errLayoutOutside
			}
		} else if throughGrant {
			prefix, rel = rel, target
		} else {
			rel += "/" + target
		}
		common, err = l.openLayoutDir(prefix, rel)
		if err != nil {
			return false, err
		}
		defer func() { _ = unix.Close(common) }()
		commonPath := filepath.Join(l.root, prefix, rel)
		lookup = func(name string) string { return gitLookupPath(commonPath, name) }
	}

	if obj, set := os.LookupEnv("GIT_OBJECT_DIRECTORY"); set {
		// A relative override depends on the directory where the user starts Git.
		if obj != "" && !filepath.IsAbs(obj) {
			return false, errors.New("a relative Git object directory depends on the invoking directory")
		}
		if ok, err := l.objectDirectorySearch(obj); !ok || err != nil {
			return false, err
		}
	} else if ok, err := l.searchableLayoutMarker(common, filepath.Base(lookup("objects"))); !ok || err != nil {
		return false, err
	}
	ok, err := l.searchableLayoutMarker(common, filepath.Base(lookup("refs")))
	if ok && throughGrant {
		l.grantReachedHEADs = append(l.grantReachedHEADs, filepath.Join(dir, "HEAD"))
	}
	return ok, err
}

// objectDirectorySearch uses Git's access(2) test on the host-supplied
// override. Only a walk that temporarily granted owner access also needs to
// check the original search permission on each component. Keep raw prefixes:
// in a/b/../c, Git still has to search b. A symlink could hide a temporarily
// opened ancestor and leaves the result uncertain in that case.
func (l *sandboxGitLayout) objectDirectorySearch(p string) (bool, error) {
	if err := unix.Access(p, unix.X_OK); err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		return false, err
	}
	if !l.access.hasTemporaryGrants() {
		return true, nil
	}
	for prefix := strings.TrimRight(p, "/"); prefix != ""; {
		var st unix.Stat_t
		if err := unix.Fstatat(unix.AT_FDCWD, prefix, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return false, err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return false, errors.New("the configured Git object directory crosses a symbolic link during a temporary permission grant")
		}
		if mode, changed := l.access.originalOwnerMode(&st); changed && mode&0o100 == 0 {
			return false, nil
		}
		prefix = strings.TrimRight(prefix[:strings.LastIndexByte(prefix, '/')], "/")
	}
	return true, nil
}

var (
	errLayoutOutside = errors.New("the common directory points outside the workspace")
	errLayoutSymlink = errors.New("a repository layout marker is a symbolic link")
)

// openLayoutDir resolves a workspace-relative directory without following links.
// scannerDir is an already observed candidate reached through the walk's permissions.
// The remaining Git path must be searchable with the original permissions.
// A '..' uses the pinned ancestor. It cannot escape through the filesystem or
// erase an invalid component such as a stale directory before '..'.
func (l *sandboxGitLayout) openLayoutDir(scannerDir, rel string) (int, error) {
	first, err := unix.Open(l.root, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	stack := []int{first}
	defer func() {
		for _, fd := range stack {
			_ = unix.Close(fd)
		}
	}()
	// Keep the pinned ancestors so a relative common directory can legitimately
	// point back up from the candidate without escaping the workspace.
	if scannerDir != "" && scannerDir != "." {
		for _, name := range strings.Split(scannerDir, "/") {
			if name == "" || name == "." || name == ".." {
				return -1, errLayoutOutside
			}
			fd, err := openLayoutChild(stack[len(stack)-1], name)
			if err != nil {
				return -1, err
			}
			stack = append(stack, fd)
		}
	}
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' })
	parts = slices.DeleteFunc(parts, func(name string) bool { return name == "." })
	for i := 0; i < len(parts); i++ {
		name := parts[i]
		at := stack[len(stack)-1]
		if name == ".." {
			if len(stack) == 1 {
				return -1, errLayoutOutside
			}
			_ = unix.Close(at)
			stack = stack[:len(stack)-1]
			continue
		}
		// Git's realpath resolves an existing, non-link component followed
		// by ".." without entering it. A missing component still fails.
		if err := l.requireLayoutSearch(at); err != nil {
			return -1, err
		}
		if i+1 < len(parts) && parts[i+1] == ".." {
			var st unix.Stat_t
			if err := unix.Fstatat(at, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return -1, err
			}
			if st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return -1, errLayoutSymlink
			}
			i++
			continue
		}
		fd, err := openLayoutChild(at, name)
		if err != nil {
			return -1, err
		}
		stack = append(stack, fd)
	}
	last := stack[len(stack)-1]
	if err := l.requireLayoutSearch(last); err != nil {
		return -1, err
	}
	stack = stack[:len(stack)-1]
	return last, nil
}

// openLayoutChild distinguishes a missing or non-directory entry from a
// symlink that cannot be inspected. Both the lookup and open use one parent.
func openLayoutChild(parent int, name string) (int, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return -1, errLayoutSymlink
	case unix.S_IFDIR:
	default:
		return -1, unix.ENOTDIR
	}
	fd, err := unix.Openat(parent, name, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var opened unix.Stat_t
	err = unix.Fstat(fd, &opened)
	if err == nil && (st.Dev != opened.Dev || st.Ino != opened.Ino) {
		err = errSandboxWorkspaceChanged
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// searchableLayoutMarker asks the kernel, including its ACL check, whether Git
// can search the directory. Only a temporary grant to the owner needs undoing
// in this decision: POSIX ACLs apply the owner entry first to the owner.
func (l *sandboxGitLayout) searchableLayoutMarker(parent int, name string) (bool, error) {
	fd, err := openLayoutChild(parent, name)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	return l.canSearchLayoutDir(fd)
}

// requireLayoutSearch preserves an unsearchable ancestor as an unknown layout.
func (l *sandboxGitLayout) requireLayoutSearch(fd int) error {
	ok, err := l.canSearchLayoutDir(fd)
	if err == nil && !ok {
		return unix.EACCES
	}
	return err
}

// canSearchLayoutDir applies the kernel's access check on the pinned directory,
// except when this walk temporarily changed the owning user's search permission.
func (l *sandboxGitLayout) canSearchLayoutDir(fd int) (bool, error) {
	if l.access != nil {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return false, err
		}
		if mode, changed := l.access.originalOwnerMode(&st); changed {
			return mode&0o100 != 0, nil
		}
	}
	err := unix.Faccessat(fd, ".", unix.X_OK, 0)
	if errors.Is(err, unix.EACCES) {
		return false, nil
	}
	return err == nil, err
}

// readCommonDir reads only a bounded, regular commondir file, through its
// pinned parent. Invalid content or type is a present but unusable pointer.
func readCommonDir(parent int, name string) (target string, present bool, err error) {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "", false, nil
		}
		return "", false, err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return "", true, errors.New("commondir is a symbolic link")
	case unix.S_IFREG:
	default:
		return "", true, nil
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", true, err
	}
	f := os.NewFile(uintptr(fd), "commondir")
	defer func() { _ = f.Close() }()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return "", true, err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || st.Dev != opened.Dev || st.Ino != opened.Ino {
		return "", true, errSandboxWorkspaceChanged
	}
	b, err := io.ReadAll(io.LimitReader(f, unix.PathMax+1))
	if err != nil {
		return "", true, err
	}
	if len(b) > unix.PathMax || bytes.IndexByte(b, 0) >= 0 {
		return "", true, errors.New("commondir contains an unsupported path")
	}
	return string(bytes.TrimRight(b, "\r\n")), true, nil
}
