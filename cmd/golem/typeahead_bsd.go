//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// fread is FREAD from <sys/fcntl.h>: the queue selector BSD libc's
// tcflush(fd, TCIFLUSH) passes to TIOCFLUSH. Zero would flush output too and
// could drop the tail of what golem printed.
const fread = 0x1

// discardTypedAhead empties f's terminal input queue; see the Linux
// implementation.
func discardTypedAhead(f *os.File) error {
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCFLUSH, fread)
}
