//go:build unix

package tools

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// harnessOpenFlags opens a harness file without blocking, so a FIFO or device
// put in place of a checked regular file cannot hang Plan (#784, spec D2).
const harnessOpenFlags = os.O_RDONLY | unix.O_NONBLOCK

// isAbsentStatErr reports a stat error that means an import names no file
// anyone can read (#784, M8): absent, through a regular file, a name too long
// or a link loop.
func isAbsentStatErr(err error) bool {
	return harnessAbsent(err) || errors.Is(err, unix.ENAMETOOLONG) || errors.Is(err, unix.ELOOP)
}
