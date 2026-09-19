//go:build unix && !linux

package consult

import "syscall"

func groupExited(pgid int) bool {
	return syscall.Kill(-pgid, 0) == syscall.ESRCH
}
