//go:build unix

package profiles

import (
	"os"
	"syscall"
)

// ownerAndModeOK enforces the unix private-boundary invariant on the
// profiles directory: no group/world bits and owned by the current euid.
func ownerAndModeOK(fi os.FileInfo) bool {
	if fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid()
}
