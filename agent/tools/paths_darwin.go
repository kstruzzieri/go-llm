//go:build darwin

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin SDK sys/fcntl.h: O_SEARCH = O_EXEC (0x40000000) | O_DIRECTORY.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/fcntl.h
const workspaceSearchFlags = 0x40000000 | unix.O_DIRECTORY

// F_GETPATH obtains the opened dentry's spelling without enumerating its parent.
// Only the basename is used: a pinned ancestor may have been renamed.
// https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/fcntl.2.html
func workspaceDescriptorName(parent, f *os.File, part string) (string, error) {
	path, err := workspaceDescriptorPath(f)
	if err != nil {
		return "", err
	}
	parentPath, err := workspaceDescriptorPath(parent)
	if err != nil || filepath.Dir(path) != parentPath {
		return "", errScopeDenied
	}
	name := filepath.Base(path)
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", err
	}
	// Darwin caches a vnode name shared by hardlinks. Without enumeration,
	// only an exact name can disambiguate such a file; aliases fail closed.
	if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink > 1 && name != part {
		return "", errScopeDenied
	}
	return name, nil
}

func workspaceDescriptorPath(f *os.File) (string, error) {
	var path [unix.PathMax]byte
	// FcntlInt uses libSystem but carries pointer arguments as integers. Pin
	// the buffer so its address stays valid across that wrapper's Go frames.
	var pin runtime.Pinner
	pin.Pin(&path[0])
	defer pin.Unpin()
	_, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&path[0]))))
	runtime.KeepAlive(f)
	if err != nil {
		return "", err
	}
	return unix.ByteSliceToString(path[:]), nil
}
