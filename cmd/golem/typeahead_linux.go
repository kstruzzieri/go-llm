package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// discardTypedAhead empties f's terminal input queue, as tcflush(fd, TCIFLUSH)
// does: bytes typed or pasted that nothing has read yet, an unfinished line
// included. Output is left alone, so nothing golem printed is dropped. It
// neither waits for output to drain nor changes termios, so it cannot block on
// a stopped terminal.
func discardTypedAhead(f *os.File) error {
	return unix.IoctlSetInt(int(f.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}
