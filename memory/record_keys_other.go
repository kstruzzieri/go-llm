//go:build !unix

package memory

import "os"

// As in signing's hardened loaders, non-Unix hosts rely on profile ACLs.
func recordKeyDirectoryPrivate(os.FileInfo) bool { return true }
