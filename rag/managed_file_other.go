//go:build !unix

package rag

import (
	"errors"
	"os"
	"runtime"
)

func openManagedFileAt(root *os.Root, name string) (*os.File, error) {
	if runtime.GOOS == "js" || runtime.GOOS == "plan9" {
		return nil, errors.New("secure managed file open is unsupported on this platform")
	}
	return root.Open(name)
}
