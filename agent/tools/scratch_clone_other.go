//go:build !darwin && !linux

package tools

import (
	"io/fs"
	"os"
	"time"
)

// cloneFile has no CoW primitive on this platform; the sentinel routes every
// file through the exact plain-copy path.
func cloneFile(src *os.File, dst string) error {
	return errScratchCloneUnsupported
}

// cloneFallbackErrnos is empty: only the sentinel permits fallback here.
var cloneFallbackErrnos []error

// statIdentity has no portable identity on this platform; manifest
// comparison degrades to path/type/mode/size/mtime.
func statIdentity(fi fs.FileInfo) (dev, ino, nlink uint64, ctime time.Time) {
	return 0, 0, 0, time.Time{}
}
