//go:build linux

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

const workspaceSearchFlags = unix.O_PATH | unix.O_DIRECTORY

// procfs exposes the opened dentry's spelling, including on casefold mounts.
// If procfs is unavailable, callers fail closed instead of trusting an alias.
func workspaceDescriptorName(_ *os.File, f *os.File, _ string) (string, error) {
	path, err := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	runtime.KeepAlive(f)
	if err != nil {
		return "", err
	}
	return filepath.Base(path), nil
}
