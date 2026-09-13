//go:build unix

package consult

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// A non-writable leaf can still be replaced through a writable ancestor.
// Trust only root and our effective uid along the entire path. Sticky shared
// directories are safe here because their owner AND each child are trusted.
func validateCommandParents(path string, info os.FileInfo) error {
	for {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && uint64(stat.Uid) != uint64(os.Geteuid())) {
			return errors.New("command and its parents must be owned by root or the current user")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
		var err error
		info, err = os.Lstat(path)
		if err != nil {
			return fmt.Errorf("command parent: %w", err)
		}
		if !info.IsDir() {
			return errors.New("command parents must be non-symlink directories")
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return errors.New("command parents must not be group- or world-writable without the sticky bit")
		}
	}
}
