//go:build unix

package memory

import (
	"os"
	"syscall"
)

func recordKeyDirectoryPrivate(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.Mode().Perm()&0o077 == 0
}
