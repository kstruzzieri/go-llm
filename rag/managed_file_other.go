//go:build !unix

package rag

import (
	"errors"
	"os"
)

func openManagedFile(path string) (*os.File, error) {
	// Non-Unix platforms cannot request a nonblocking open. Pre-stat rejects
	// special files early; readManagedRegularFile still verifies the opened
	// handle because replacement between Stat and Open cannot be prevented here.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return os.Open(path)
}
