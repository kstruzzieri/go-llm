//go:build linux

package tools

import "golang.org/x/sys/unix"

const workspaceSearchFlags = unix.O_PATH | unix.O_DIRECTORY
