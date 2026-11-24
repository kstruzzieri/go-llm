//go:build darwin || linux

package tools

// What the post-run check (#802) shares about opening up a directory the
// user owns whose mode keeps the check out: a command can chmod its own
// directories. ownerAccess does it on Linux (sandbox_owneraccess_linux.go);
// elsewhere it never opens anything up (sandbox_owneraccess_darwin.go), so
// the after-walk there stays best effort.

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ownerAccess declines with these errors.
var (
	errAccessNotOwned = errors.New("owned by another user")
	errAccessMoved    = errors.New("it was moved or replaced during the run")
	errAccessLimit    = errors.New("too many locked directories to open up")
	errAccessHas      = errors.New("its owner can already do that")
)

// Test seams, set only from tests:
//   - ownerAccessEuid is the user whose directories may be opened up;
//   - ownerAccessLimit is how many directories one walk or hold may open up;
//   - ownerAccessBeforeChange, when set, runs as a directory is about to be
//     opened up, with its root-relative path;
//   - ownerAccessChmod changes a mode.
var (
	ownerAccessEuid         = os.Geteuid
	ownerAccessLimit        = 4096
	ownerAccessBeforeChange func(rel string)
	ownerAccessChmod        = unix.Chmod
)
