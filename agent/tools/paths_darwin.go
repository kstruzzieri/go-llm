//go:build darwin

package tools

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Darwin SDK sys/fcntl.h: O_SEARCH = O_EXEC (0x40000000) | O_DIRECTORY.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/fcntl.h
const workspaceSearchFlags = 0x40000000 | unix.O_DIRECTORY

// workspaceNoFollowAny refuses a symlink in any component of a path, so one
// openat can verify a run of components (#613). It replaces O_NOFOLLOW, which
// XNU rejects (EINVAL) in combination with it.
const workspaceNoFollowAny = unix.O_NOFOLLOW_ANY

// Darwin SDK sys/attr.h; x/sys does not export these directory attributes.
const (
	attrDirMountStatus = 0x00000004
	dirMountPoint      = 0x00000001
)

// storedNameFS reports whether the filesystem answers name lookups with the
// object's stored spelling (#558). Only APFS was examined; HFS+ has directory
// hardlinks, and other volumes keep the listing scan.
func storedNameFS(st *unix.Statfs_t) bool {
	return unix.ByteSliceToString(st.Fstypename[:]) == "apfs"
}

// requireEnumerable opens parent for reading and reads one batch of names, as
// the listing scan does, so a proof that skips the full listing still fails
// on a directory the caller may not list (#556): open-time permission and the
// read-time authorization XNU applies to directory reads. Reaching the end is
// fine; any error is returned unchanged.
func requireEnumerable(parent *os.File) error {
	fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), ".")
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// provenSpelling returns part's on-disk spelling without listing parent
// (#558). APFS reports, in one getattrlistat(2), the current stored name of
// the object a lookup of part reaches together with that object's device,
// file ID and (for regular files) link count. Binding the identity to target,
// the caller's fstatat of part, makes the name belong to the object the
// caller then opens and re-verifies. Only non-mount-point directories and
// single-link regular files on the parent's logical APFS device qualify:
// another link's name is never evidence (#556), and a mount point's name
// comes from the mounted volume. Mount status is checked in the same reply
// because APFS volume-group members can share a logical device ID.
// Anything else, and any query failure, returns ok=false so the caller lists
// parent. A parent that cannot be listed fails as the scan does; a name that
// belongs to another object is errFileChanged.
func provenSpelling(parent *os.File, part string, target *unix.Stat_t) (string, bool, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(parent.Fd()), &fs); err != nil || !storedNameFS(&fs) {
		return "", false, nil
	}
	var pst unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &pst); err != nil || pst.Dev != target.Dev {
		return "", false, nil
	}
	var regular bool
	switch target.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		regular = true
	default:
		return "", false, nil
	}
	if err := requireEnumerable(parent); err != nil {
		return "", false, err
	}
	e, err := storedName(int(parent.Fd()), part, regular)
	if err != nil || e.mountPoint || (regular && e.links != 1) {
		return "", false, nil
	}
	if e.dev != target.Dev || e.ino != target.Ino {
		return "", false, errFileChanged
	}
	return e.name, true, nil
}

// storedEntry is one decoded getattrlist reply.
type storedEntry struct {
	name       string
	dev        int32
	ino        uint64
	links      uint32
	mountPoint bool
}

var errMalformedAttrs = errors.New("tools: malformed getattrlist reply")

// storedName queries name relative to dirfd without following a final
// symlink.
func storedName(dirfd int, name string, regular bool) (storedEntry, error) {
	p, err := unix.BytePtrFromString(name)
	if err != nil {
		return storedEntry{}, err
	}
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_NAME | unix.ATTR_CMN_DEVID | unix.ATTR_CMN_FILEID}
	if regular {
		al.Fileattr = unix.ATTR_FILE_LINKCOUNT
	} else {
		al.Dirattr = attrDirMountStatus
	}
	var buf [28 + 1024]byte
	if e := getattrlistat(dirfd, p, &al, &buf[0], len(buf), unix.FSOPT_NOFOLLOW); e != 0 {
		return storedEntry{}, e
	}
	return parseStoredEntry(buf[:], regular)
}

// parseStoredEntry decodes storedName's reply. Attributes arrive in bitmap
// order, common before directory/file, each 4-byte aligned: u_int32 length; the name's
// attrreference_t {int32 offset from the reference itself, u_int32 length
// including the NUL}; dev_t; u_int64 file ID; a u_int32 mount status for
// directories or link count for regular files; then the name bytes. Every
// bound is checked: a malformed reply is an error, never a guess.
func parseStoredEntry(buf []byte, regular bool) (storedEntry, error) {
	const fixed = 28
	if len(buf) < fixed {
		return storedEntry{}, errMalformedAttrs
	}
	ne := binary.NativeEndian
	total := int(ne.Uint32(buf[0:4]))
	start := 4 + int(int32(ne.Uint32(buf[4:8])))
	n := int(ne.Uint32(buf[8:12]))
	// start >= fixed, n >= 2 and start+n <= total also bound total below.
	if total > len(buf) || start < fixed || n < 2 || start+n > total || buf[start+n-1] != 0 {
		return storedEntry{}, errMalformedAttrs
	}
	e := storedEntry{name: string(buf[start : start+n-1]), dev: int32(ne.Uint32(buf[12:16])), ino: ne.Uint64(buf[16:24])}
	if regular {
		e.links = ne.Uint32(buf[24:28])
	} else {
		e.mountPoint = ne.Uint32(buf[24:28])&dirMountPoint != 0
	}
	if e.name == "." || e.name == ".." || strings.ContainsAny(e.name, "/\x00") {
		return storedEntry{}, errMalformedAttrs
	}
	return e, nil
}
