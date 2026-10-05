//go:build !unix

package main

import "os"

func openAgentflowStateFile(path string) (*os.File, error) {
	return os.Open(path)
}
