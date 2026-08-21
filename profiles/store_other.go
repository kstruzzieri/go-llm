//go:build !unix

package profiles

import "os"

// ownerAndModeOK is a documented same-user best-effort no-op off unix:
// per-file mode and ownership semantics differ (e.g. Windows ACLs), so the
// boundary downgrades to trusting the OS user profile isolation.
func ownerAndModeOK(os.FileInfo) bool { return true }
