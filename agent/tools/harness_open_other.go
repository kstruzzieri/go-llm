//go:build !unix

package tools

import "os"

// harnessOpenFlags is a plain read-only open where O_NONBLOCK does not exist;
// openHarnessFile's type checks still refuse anything but a regular file.
const harnessOpenFlags = os.O_RDONLY
