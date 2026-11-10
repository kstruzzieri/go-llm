//go:build unix

package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

// harnessOpenFlags opens a harness file without blocking, so a FIFO or device
// put in place of a checked regular file cannot hang Plan (#784, spec D2).
const harnessOpenFlags = os.O_RDONLY | unix.O_NONBLOCK
