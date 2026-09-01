//go:build linux

package tools

import "golang.org/x/sys/unix"

// promoteRename installs the completed temp with renameat2(RENAME_NOREPLACE):
// atomic, descriptor-relative, and never replaces a destination that
// appeared after validation. Filesystems that reject it fail closed with no
// link or copy fallback.
func promoteRename(parentFd int, tmpName, dstName string) error {
	return unix.Renameat2(parentFd, tmpName, parentFd, dstName, unix.RENAME_NOREPLACE)
}
