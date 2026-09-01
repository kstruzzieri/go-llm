//go:build darwin

package tools

import (
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cloneFile clones the opened source file to dst with clonefile(2) via the
// stable descriptor, so a source path swap cannot substitute content. APFS
// creates dst atomically; on failure no partial destination exists.
func cloneFile(src *os.File, dst string) error {
	err := unix.Fclonefileat(int(src.Fd()), unix.AT_FDCWD, dst, 0)
	runtime.KeepAlive(src)
	return err
}

// cloneFallbackErrnos is the enumerated set of clonefile failures that
// permit the exact plain-copy fallback: filesystems without CoW support and
// cross-device destinations. Everything else (permission, I/O, corruption,
// disk-full) is fatal.
var cloneFallbackErrnos = []error{unix.ENOTSUP, unix.EXDEV}

// statIdentity extracts platform file identity for manifest comparison.
func statIdentity(fi fs.FileInfo) (dev, ino, nlink uint64, ctime time.Time) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, time.Time{}
	}
	return uint64(st.Dev), st.Ino, uint64(st.Nlink),
		time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
