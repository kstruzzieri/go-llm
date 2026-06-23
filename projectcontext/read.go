package projectcontext

import (
	"io"
	"os"
)

// readCapped reads at most maxBytes from a regular file at path, never following
// a symlink. It returns found=false (nil error) when the path does not exist, is
// a symlink, or is not a regular file — a non-existent or unsafe project-context
// file is simply absent, not an error. truncated reports whether the file was
// longer than maxBytes. The open handle is re-stat'd and compared with
// os.SameFile to reject a final-component swap between Lstat and Open (TOCTOU).
func readCapped(path string, maxBytes int) (content string, truncated, found bool, err error) {
	lfi, lerr := os.Lstat(path)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return "", false, false, nil
		}
		return "", false, false, lerr
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return "", false, false, nil // never follow symlinks
	}
	if !lfi.Mode().IsRegular() {
		return "", false, false, nil
	}
	f, oerr := os.Open(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return "", false, false, nil
		}
		return "", false, false, oerr
	}
	defer func() { _ = f.Close() }()
	sfi, serr := f.Stat()
	if serr != nil {
		return "", false, false, serr
	}
	if !os.SameFile(lfi, sfi) {
		return "", false, false, nil // identity changed between Lstat and Open
	}
	// Read one extra byte to detect truncation without loading the whole file.
	buf, rerr := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if rerr != nil {
		return "", false, false, rerr
	}
	if len(buf) > maxBytes {
		return string(buf[:maxBytes]), true, true, nil
	}
	return string(buf), false, true, nil
}
