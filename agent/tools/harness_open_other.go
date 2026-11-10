//go:build !unix && !windows

package tools

import "os"

// harnessOpenFlags is a plain read-only open where O_NONBLOCK does not exist;
// openHarnessFile's type checks still refuse anything but a regular file.
const harnessOpenFlags = os.O_RDONLY

// isAbsentStatErr reports a stat error that means an import names no file
// (#784, M8): only an absent one here.
func isAbsentStatErr(err error) bool { return harnessAbsent(err) }
