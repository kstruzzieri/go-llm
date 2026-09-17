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

// procfs exposes the dcache spelling of the opened dentry. On a case-sensitive
// filesystem the successful lookup already proved the requested spelling. On
// case-insensitive Linux mounts (ext4/f2fs casefold, VFAT, CIFS) the dcache
// keeps the spelling that was looked up, not the on-disk one, so this is not
// canonical evidence there: alias verification of a search-only directory is
// only proven on Darwin (F_GETPATH). If procfs is unavailable, callers fail
// closed instead of trusting an alias.
func workspaceDescriptorName(_ *os.File, f *os.File, _ string) (string, error) {
	path, err := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	runtime.KeepAlive(f)
	if err != nil {
		return "", err
	}
	return filepath.Base(path), nil
}
