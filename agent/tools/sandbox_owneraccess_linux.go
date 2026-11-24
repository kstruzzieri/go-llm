//go:build linux

package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ownerAccess opens up, for one walk or one hold, the directories whose mode
// keeps it out, and puts their exact modes back. It changes only a directory
// the effective user owns (ownerAccessEuid), that is still the one reached
// at its path from the root (reaches), and only by the bits needed: u+rx to
// list and look up, u+wx to rename. At most ownerAccessLimit directories are
// opened up. Every change goes through /proc/self/fd on a duplicate of the
// pinned O_PATH descriptor (fchmod refuses one), so it reaches exactly that
// inode, and so does the restore. Callers defer restore, which puts modes
// back newest first: nested directories are opened outside in and restored
// inside out.
type ownerAccess struct {
	root    int // the workspace root, O_PATH; -1 when rootErr
	rootErr error
	grants  []accessGrant
	counted map[[2]uint64]uint32 // the directories opened up so far
	screen  *outputScreen        // the command's credential screen, for each path a warning shows
}

// accessGrant is one mode ownerAccess changed: the kept descriptor, the
// directory's path from the root, and its mode before.
type accessGrant struct {
	fd   int
	rel  string
	mode uint32
}

// newOwnerAccess duplicates the workspace pin for one walk or hold.
func newOwnerAccess(pin *os.File) *ownerAccess {
	if pin == nil {
		return &ownerAccess{root: -1, rootErr: unix.EBADF}
	}
	fd, err := unix.FcntlInt(pin.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		fd = -1
	}
	return &ownerAccess{root: fd, rootErr: err, counted: make(map[[2]uint64]uint32)}
}

// close releases the root descriptor. Call restore first.
func (a *ownerAccess) close() {
	if a.root >= 0 {
		_ = unix.Close(a.root)
	}
}

// grant gives the owner need on the directory open at dir, found at rel from
// the root, until restore. It declines a directory another user owns
// (errAccessNotOwned), one whose owner already has need (errAccessHas), one
// the root no longer reaches at rel (errAccessMoved), and a new one past
// ownerAccessLimit (errAccessLimit).
func (a *ownerAccess) grant(dir int, rel string, need uint32) error {
	if ownerAccessBeforeChange != nil {
		ownerAccessBeforeChange(rel)
	}
	var st unix.Stat_t
	if err := unix.Fstat(dir, &st); err != nil {
		return err
	}
	key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
	_, opened := a.counted[key]
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return unix.ENOTDIR
	case int(st.Uid) != ownerAccessEuid():
		return errAccessNotOwned
	case st.Mode&need == need:
		return errAccessHas
	case st.Mode&unix.S_ISUID != 0 || st.Mode&unix.S_ISGID != 0 && !ownerAccessInGroup(st.Gid):
		return errAccessSpecial
	case !a.reaches(rel, &st):
		return errAccessMoved
	case !opened && len(a.counted) >= ownerAccessLimit:
		return errAccessLimit
	}
	keep, err := unix.FcntlInt(uintptr(dir), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return err
	}
	mode := st.Mode & 0o7777
	if err := ownerAccessChmod(procFd(keep), mode|need); err != nil {
		_ = unix.Close(keep)
		return err
	}
	if !opened {
		a.counted[key] = mode
	}
	a.grants = append(a.grants, accessGrant{fd: keep, rel: rel, mode: mode})
	return nil
}

// ownerAccessInGroup checks whether setgid can be restored without relying on
// capabilities in the filesystem's user namespace. Tests can model a foreign group.
var ownerAccessInGroup = func(gid uint32) bool {
	// The invalid gid queries the filesystem gid without changing it.
	if fsgid, err := unix.SetfsgidRetGid(-1); err == nil && int(gid) == fsgid {
		return true
	}
	groups, err := os.Getgroups()
	return err == nil && slices.Contains(groups, int(gid))
}

// restore puts back, newest first, the exact mode (permission, setuid,
// setgid and sticky bits) of every directory grant opened up, through its
// kept descriptor, and checks it with fstat. It returns a warning for each
// mode it could not put back.
func (a *ownerAccess) restore() (warnings []string) {
	for i := len(a.grants) - 1; i >= 0; i-- {
		g := a.grants[i]
		err := ownerAccessChmod(procFd(g.fd), g.mode)
		var st unix.Stat_t
		if err == nil {
			err = unix.Fstat(g.fd, &st)
		}
		if err == nil && st.Mode&0o7777 != g.mode {
			err = fmt.Errorf("it is %04o", st.Mode&0o7777)
		}
		if err != nil {
			name, ok := screenedName(a.screen, g.rel)
			if !ok {
				warnings = append(warnings, "WARNING: could not restore the directory mode at "+withheldPath)
			} else {
				warnings = append(warnings, fmt.Sprintf("WARNING: could not restore the mode of %s to %04o (%v); set it back by hand",
					name, g.mode, err))
			}
		}
		_ = unix.Close(g.fd)
	}
	a.grants = a.grants[:0]
	return warnings
}

// reaches reports whether rel, opened again from the root one component at a
// time without following a symbolic link (descend), is the directory st
// describes.
func (a *ownerAccess) reaches(rel string, st *unix.Stat_t) bool {
	if a.root < 0 {
		return false
	}
	fd, err := unix.FcntlInt(uintptr(a.root), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	if rel != "." && descend(&fd, strings.Split(rel, "/")) != nil {
		return false
	}
	var now unix.Stat_t
	return unix.Fstat(fd, &now) == nil && now.Dev == st.Dev && now.Ino == st.Ino
}

// retry runs op, which works in the directory open at dir, found at dirRel.
// When the kernel refuses access (EACCES) and grant gives need, it runs op once more; otherwise it returns
// op's error, marked when grant found the directory moved or replaced
// (movedErr) or owned by another user (notOwnedErr), and otherwise followed by
// the reason grant declined (the limit, a descriptor it could not duplicate).
func (a *ownerAccess) retry(dir int, dirRel string, need uint32, op func() error) error {
	err := op()
	if !errors.Is(err, unix.EACCES) {
		return err
	}
	if gerr := a.grant(dir, dirRel, need); gerr != nil {
		switch {
		case errors.Is(gerr, errAccessSpecial):
			return gerr
		case errors.Is(gerr, errAccessMoved):
			return movedErr{err}
		case errors.Is(gerr, errAccessNotOwned):
			return notOwnedErr{err}
		}
		return fmt.Errorf("%w; %w", err, gerr) // why it could not be opened up, beside why the operation failed
	}
	return op()
}

// open opens rel under the directory open at dir, found at dirRel, as a new
// O_PATH descriptor ("." for a duplicate), one component at a time without
// following a symbolic link. A directory that refuses a lookup is opened up
// for it (retry, u+rx).
func (a *ownerAccess) open(dir int, dirRel, rel string) (int, error) {
	fd, err := unix.FcntlInt(uintptr(dir), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if rel == "." {
		return fd, nil
	}
	at := dirRel
	for _, c := range strings.Split(rel, "/") {
		next := -1
		err := a.retry(fd, at, 0o500, func() (err error) {
			next, err = unix.Openat(fd, holdComponent(fd, c), workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			return err
		})
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd, at = next, path.Join(at, c)
	}
	return fd, nil
}

// openUpPath tries the walk's read and child lookup in the pinned directory.
// An empty listing needs no search. Only a denied operation needs a grant.
func (a *ownerAccess) openUpPath(rel string) (bool, error) {
	if a.root < 0 {
		return false, a.rootErr
	}
	fd, err := a.open(a.root, ".", rel)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(fd) }()
	dir, err := os.Open(procFd(fd))
	if err == nil {
		defer func() { _ = dir.Close() }()
		var names []string
		names, err = dir.Readdirnames(1)
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err == nil {
			var st unix.Stat_t
			err = unix.Fstatat(fd, names[0], &st, unix.AT_SYMLINK_NOFOLLOW)
		}
	}
	if errors.Is(err, unix.EACCES) {
		err = a.grant(fd, rel, 0o500)
	}
	return false, err
}

// movedErr is err, met in a directory the root no longer reaches where it
// was found.
type movedErr struct{ err error }

// Error is err's text.
func (e movedErr) Error() string { return e.err.Error() }

// Unwrap gives both err and errAccessMoved to errors.Is.
func (e movedErr) Unwrap() []error { return []error{e.err, errAccessMoved} }

// notOwnedErr is err, met in a directory another user owns, which is never
// opened up. Its text is that reason rather than the kernel's, which only
// says the operation was refused.
type notOwnedErr struct{ err error }

// Error is errAccessNotOwned's text.
func (e notOwnedErr) Error() string { return errAccessNotOwned.Error() }

// Unwrap gives both err and errAccessNotOwned to errors.Is.
func (e notOwnedErr) Unwrap() []error { return []error{e.err, errAccessNotOwned} }

// procFd is the /proc path that reaches the file open at fd.
func procFd(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

// originalMode returns the permission bits from before the first temporary grant.
func (a *ownerAccess) originalMode(st *unix.Stat_t) uint32 {
	if a != nil {
		if mode, ok := a.counted[[2]uint64{uint64(st.Dev), uint64(st.Ino)}]; ok {
			return mode
		}
	}
	return st.Mode
}
