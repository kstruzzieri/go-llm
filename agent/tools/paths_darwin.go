//go:build darwin

package tools

import "golang.org/x/sys/unix"

// Darwin SDK sys/fcntl.h: O_SEARCH = O_EXEC (0x40000000) | O_DIRECTORY.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/fcntl.h
const workspaceSearchFlags = 0x40000000 | unix.O_DIRECTORY
