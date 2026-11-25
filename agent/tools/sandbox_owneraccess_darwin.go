//go:build darwin

package tools

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ownerAccess never opens a directory up on macOS: it changes a mode through
// /proc/self/fd, which Linux has and macOS does not, and the post-run check
// runs only with bwrap. The after-walk there stays best effort.
type ownerAccess struct {
	screen *outputScreen // unused on macOS; set as on Linux
}

// newOwnerAccess returns the no-op ownerAccess.
func newOwnerAccess(*os.File) *ownerAccess { return &ownerAccess{} }

// openUpPath opens nothing up.
func (*ownerAccess) openUpPath(string) (bool, error) {
	return false, errors.New("directories are not opened up on this system")
}

// restore has nothing to put back.
func (*ownerAccess) restore() []string { return nil }

// close has nothing to release.
func (*ownerAccess) close() {}

// hasTemporaryGrants reports no temporary permission grants on Darwin.
func (*ownerAccess) hasTemporaryGrants() bool { return false }

// originalOwnerMode reports no temporary permission grants on Darwin.
func (*ownerAccess) originalOwnerMode(*unix.Stat_t) (uint32, bool) { return 0, false }
