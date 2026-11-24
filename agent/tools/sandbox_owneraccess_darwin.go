//go:build darwin

package tools

import "errors"

// ownerAccess never opens a directory up on macOS: it changes a mode through
// /proc/self/fd, which Linux has and macOS does not, and the post-run check
// runs only with bwrap. The after-walk there stays best effort.
type ownerAccess struct{}

// newOwnerAccess returns the no-op ownerAccess.
func newOwnerAccess(string) *ownerAccess { return &ownerAccess{} }

// openUpPath opens nothing up.
func (*ownerAccess) openUpPath(string) error {
	return errors.New("directories are not opened up on this system")
}

// restore has nothing to put back.
func (*ownerAccess) restore() []string { return nil }

// close has nothing to release.
func (*ownerAccess) close() {}
