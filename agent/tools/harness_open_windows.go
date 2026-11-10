//go:build windows

package tools

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// harnessOpenFlags is a plain read-only open: Windows has no O_NONBLOCK, and
// openHarnessFile's type checks still refuse anything but a regular file.
const harnessOpenFlags = os.O_RDONLY

// isAbsentStatErr reports a stat error that means an import names no file
// anyone can read (#784, M8, fix round 1b): absent, or a name Windows refuses
// outright, as ordinary Markdown such as "@team:" or "@https://x/y" yields
// (invalid name, bad path, a component too long, a file where a folder is
// expected).
func isAbsentStatErr(err error) bool {
	return harnessAbsent(err) ||
		errors.Is(err, windows.ERROR_INVALID_NAME) ||
		errors.Is(err, windows.ERROR_BAD_PATHNAME) ||
		errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE) ||
		errors.Is(err, windows.ERROR_DIRECTORY)
}
