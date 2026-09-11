//go:build unix

package recipe

import (
	"os"
	"syscall"
)

func openRecipeFile(path string) (*os.File, error) {
	// A FIFO swapped in after stat must reach the regular-file check without
	// waiting for a writer. O_NONBLOCK has no effect on regular file reads.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
