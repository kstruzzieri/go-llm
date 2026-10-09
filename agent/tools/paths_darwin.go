//go:build darwin

package tools

import "golang.org/x/sys/unix"

// Darwin SDK sys/fcntl.h: O_SEARCH = O_EXEC (0x40000000) | O_DIRECTORY.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/fcntl.h
const workspaceSearchFlags = 0x40000000 | unix.O_DIRECTORY

// workspaceNoFollowAny refuses a symlink in any component of a path, so one
// openat can verify a run of components (#613). It replaces O_NOFOLLOW, which
// XNU rejects (EINVAL) in combination with it.
const workspaceNoFollowAny = unix.O_NOFOLLOW_ANY
