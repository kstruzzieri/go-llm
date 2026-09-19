//go:build unix

package signing

import (
	"os"
	"syscall"
)

// ownerAndModeOK mirrors profiles.ownerAndModeOK: no group/other bits and
// owned by the current effective user. It applies to both key directories
// and key files.
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
