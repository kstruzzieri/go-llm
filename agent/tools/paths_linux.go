//go:build linux

package tools

import (
	"os"

	"golang.org/x/sys/unix"
)

const workspaceSearchFlags = unix.O_PATH | unix.O_DIRECTORY

// workspaceNoFollowAny is zero: Linux verifies one component at a time.
const workspaceNoFollowAny = 0

// provenSpelling has no constant-time proof on Linux, so canonicalName lists
// the parent (#558). A case-flip lookup was rejected: a rename racing the
// negative lookup could hand the guard a spelling never stored.
func provenSpelling(*os.File, string, *unix.Stat_t) (string, bool, error) { return "", false, nil }
