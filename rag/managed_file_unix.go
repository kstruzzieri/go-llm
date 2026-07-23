//go:build unix

package rag

import (
	"os"
	"syscall"
)

func openManagedFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
