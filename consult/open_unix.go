//go:build unix

package consult

import (
	"os"
	"syscall"
)

// openConfigFile opens the consultants file without blocking. A FIFO swapped
// in after the Lstat must reach the regular-file check rather than wait for a
// writer, or a hostile path stalls startup forever. O_NONBLOCK has no effect
// on regular file reads.
func openConfigFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
