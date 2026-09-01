//go:build linux

package tools

import (
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cloneFile reflinks the opened source file to dst (FICLONE) via the stable
// descriptor, so a source path swap cannot substitute content. The partial
// destination is removed on ioctl failure so the caller's exclusive-create
// fallback can run.
func cloneFile(src *os.File, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(src.Fd())); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		runtime.KeepAlive(src)
		return err
	}
	runtime.KeepAlive(src)
	return out.Close()
}

// cloneFallbackErrnos is the enumerated set of FICLONE failures that permit
// the exact plain-copy fallback: filesystems without reflink support
// (EOPNOTSUPP, and ENOTTY where the filesystem rejects the ioctl class),
// cross-device destinations (EXDEV), and EINVAL for filesystems that reject
// reflink on otherwise valid arguments. Everything else (permission, I/O,
// corruption, disk-full) is fatal.
var cloneFallbackErrnos = []error{unix.EOPNOTSUPP, unix.ENOTTY, unix.EXDEV, unix.EINVAL}

// statIdentity extracts platform file identity for manifest comparison.
func statIdentity(fi fs.FileInfo) (dev, ino, nlink uint64, ctime time.Time) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, time.Time{}
	}
	return uint64(st.Dev), st.Ino, uint64(st.Nlink),
		time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
