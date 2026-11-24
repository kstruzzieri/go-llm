//go:build linux

package tools

import (
	"errors"
	"fmt"
	"path"
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
	counted map[[2]uint64]bool // the directories opened up so far
}

// accessGrant is one mode ownerAccess changed: the kept descriptor, the
// directory's path from the root, and its mode before.
type accessGrant struct {
	fd   int
	rel  string
	mode uint32
}

// newOwnerAccess opens root for one walk or hold; close releases it.
func newOwnerAccess(root string) *ownerAccess {
	fd, err := unix.Open(root, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		fd = -1
	}
	return &ownerAccess{root: fd, rootErr: err, counted: make(map[[2]uint64]bool)}
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
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return unix.ENOTDIR
	case int(st.Uid) != ownerAccessEuid():
		return errAccessNotOwned
	case st.Mode&need == need:
		return errAccessHas
	case !a.reaches(rel, &st):
		return errAccessMoved
	case !a.counted[key] && len(a.counted) >= ownerAccessLimit:
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
	a.counted[key] = true
	a.grants = append(a.grants, accessGrant{fd: keep, rel: rel, mode: mode})
	return nil
}

// restore puts back, newest first, the exact mode (permission, setuid,
// setgid and sticky bits) of every directory grant opened up, through its
// kept descriptor, and checks it with fstat. It returns a warning for each
// mode it could not put back: the kernel drops setgid when the user is not
// in the directory's group, for one.
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
			warnings = append(warnings, fmt.Sprintf("WARNING: could not restore the mode of %s to %04o (%v); set it back by hand",
				noteName(g.rel), g.mode, err))
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
// When the kernel refuses it (EACCES or EPERM, which a mode the command set
// can cause) and grant gives need, it runs op once more; otherwise it returns
// op's error, marked when grant found the directory moved or replaced
// (movedErr).
func (a *ownerAccess) retry(dir int, dirRel string, need uint32, op func() error) error {
	err := op()
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return err
	}
	if gerr := a.grant(dir, dirRel, need); gerr != nil {
		if errors.Is(gerr, errAccessMoved) {
			return movedErr{err}
		}
		return err
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
			next, err = unix.Openat(fd, c, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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

// openUpPath opens up the directory at rel from the root so it can be listed
// and searched (u+rx), opening up any directory above it whose lookup needs
// it. It is the after-walk's entry point.
func (a *ownerAccess) openUpPath(rel string) error {
	if a.root < 0 {
		return a.rootErr
	}
	fd, err := a.open(a.root, ".", rel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := a.grant(fd, rel, 0o500); err != nil && !errors.Is(err, errAccessHas) {
		return err
	}
	return nil
}

// movedErr is err, met in a directory the root no longer reaches where it
// was found.
type movedErr struct{ err error }

// Error is err's text.
func (e movedErr) Error() string { return e.err.Error() }

// Unwrap gives both err and errAccessMoved to errors.Is.
func (e movedErr) Unwrap() []error { return []error{e.err, errAccessMoved} }

// procFd is the /proc path that reaches the file open at fd.
func procFd(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }
