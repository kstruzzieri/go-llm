//go:build !unix

package rag

import "os"

func openManagedFile(path string) (*os.File, error) { return os.Open(path) }
