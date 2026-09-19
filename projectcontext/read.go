package projectcontext

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
)

const streamReadBufferSize = 32 * 1024

// readCapped hashes a regular file at path while retaining at most maxBytes,
// never following a symlink. It returns found=false (nil error) when the path
// does not exist, is a symlink, or is not a regular file. The open handle is
// re-stat'd and compared with os.SameFile to reject a final-component swap
// between Lstat and Open (TOCTOU).
func readCapped(ctx context.Context, path string, maxBytes int) (content string, hash [32]byte, size int64, truncated, found bool, err error) {
	lfi, lerr := os.Lstat(path)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return "", hash, 0, false, false, nil
		}
		return "", hash, 0, false, false, lerr
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return "", hash, 0, false, false, nil // never follow symlinks
	}
	if !lfi.Mode().IsRegular() {
		return "", hash, 0, false, false, nil
	}
	f, oerr := os.Open(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return "", hash, 0, false, false, nil
		}
		return "", hash, 0, false, false, oerr
	}
	defer func() { _ = f.Close() }()
	sfi, serr := f.Stat()
	if serr != nil {
		return "", hash, 0, false, false, serr
	}
	if !os.SameFile(lfi, sfi) {
		return "", hash, 0, false, false, nil // identity changed between Lstat and Open
	}
	content, hash, size, truncated, err = readStream(ctx, f, maxBytes)
	if err != nil {
		return "", [32]byte{}, 0, false, false, err
	}
	return content, hash, size, truncated, true, nil
}

func readStream(ctx context.Context, r io.Reader, maxBytes int) (string, [32]byte, int64, bool, error) {
	hasher := sha256.New()
	retained := make([]byte, 0, min(maxBytes, streamReadBufferSize))
	buf := make([]byte, streamReadBufferSize)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", [32]byte{}, 0, false, err
		}
		n, readErr := r.Read(buf)
		if err := ctx.Err(); err != nil {
			return "", [32]byte{}, 0, false, err
		}
		if n > 0 {
			_, _ = hasher.Write(buf[:n])
			size += int64(n)
			remaining := maxBytes - len(retained)
			if remaining > 0 {
				retained = append(retained, buf[:min(n, remaining)]...)
			}
		}
		if readErr == nil {
			continue
		}
		if readErr != io.EOF {
			return "", [32]byte{}, 0, false, readErr
		}
		if err := ctx.Err(); err != nil {
			return "", [32]byte{}, 0, false, err
		}
		var hash [32]byte
		copy(hash[:], hasher.Sum(nil))
		return string(retained), hash, size, size > int64(maxBytes), nil
	}
}
