//go:build unix

package consult

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func validateCommandPermissions(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("command must not be group- or world-writable")
	}
	return validatePathTrust(path, info)
}

// Both executable paths and temp parents must resist replacement by other
// users. Sticky shared directories protect entries only when their owner and
// each child along the path are trusted too.
func validatePathTrust(path string, info os.FileInfo) error {
	for {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && uint64(stat.Uid) != uint64(os.Geteuid())) {
			return errors.New("path and its parents must be owned by root or the current user")
		}
		if info.IsDir() && info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return errors.New("path directories must not be group- or world-writable without the sticky bit")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
		var err error
		info, err = os.Lstat(path)
		if err != nil {
			return fmt.Errorf("path parent: %w", err)
		}
		if !info.IsDir() {
			return errors.New("path parents must be non-symlink directories")
		}
	}
}
