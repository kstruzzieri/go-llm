//go:build !unix

package signing

import "os"

// ownerAndModeOK is a documented no-op off unix: Windows has no POSIX mode
// bits, so the boundary relies on OS user-profile ACLs, matching profiles/.
func ownerAndModeOK(os.FileInfo) bool { return true }
