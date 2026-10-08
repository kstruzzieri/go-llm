package main

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a PTY pair the way posix_openpt, grantpt, unlockpt and
// ptsname do on macOS (x/sys wraps none of them), at an ordinary 80x24 size.
func openPTY(t *testing.T) *ptyPair {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	fail := func(format string, args ...any) {
		t.Helper()
		_ = unix.Close(mfd)
		t.Fatalf(format, args...)
	}
	if err := unix.IoctlSetInt(mfd, unix.TIOCPTYGRANT, 0); err != nil {
		fail("TIOCPTYGRANT: %v", err)
	}
	if err := unix.IoctlSetInt(mfd, unix.TIOCPTYUNLK, 0); err != nil {
		fail("TIOCPTYUNLK: %v", err)
	}
	var name [128]byte // TIOCPTYGNAME fills a 128-byte buffer
	//nolint:staticcheck // SA1019: x/sys has no libSystem wrapper for ptsname or a pointer ioctl.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		fail("TIOCPTYGNAME: %v", errno)
	}
	slavePath := string(name[:bytes.IndexByte(name[:], 0)])
	sfd, err := unix.Open(slavePath, unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		fail("open %s: %v", slavePath, err)
	}
	if err := unix.IoctlSetWinsize(sfd, unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		_ = unix.Close(sfd)
		fail("TIOCSWINSZ: %v", err)
	}
	p := &ptyPair{
		master: os.NewFile(uintptr(mfd), "/dev/ptmx"),
		slave:  os.NewFile(uintptr(sfd), slavePath),
	}
	t.Cleanup(p.close)
	return p
}
