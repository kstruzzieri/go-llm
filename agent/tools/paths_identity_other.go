//go:build !windows

package tools

import "os"

// Stat captures the native identity without requiring directory read access.
func workspaceRootIdentity(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
