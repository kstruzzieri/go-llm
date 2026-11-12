package tools

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// dirSys is the walk's own directory read (#789): getdents64 on the held
// descriptor, parsed for each record's name and raw d_type. It is not
// os.File.ReadDir, which on a filesystem that reports no types drops entries
// whose stat fails. The field is a seam for tests.
type dirSys struct {
	getdents func(fd int, buf []byte) (int, error)
}

// sysDir is the real directory read.
var sysDir = dirSys{getdents: unix.ReadDirent}

// dirBufPool holds the read's 65536-byte buffers, as os.dirBufPool does; only
// buf[:n] is ever parsed, so a reused buffer's old bytes are never read.
var dirBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 65536)
		return &buf
	},
}

// read emits the name and raw d_type of every entry of f except "." and "..".
// f must be freshly opened: the read starts at its current position. A nil
// return means the whole directory was read; on any error the caller
// discards what was emitted.
func (s dirSys) read(f *os.File, emit func(name string, typ uint8)) error {
	defer runtime.KeepAlive(f)
	fd := int(f.Fd())
	bp := dirBufPool.Get().(*[]byte)
	defer dirBufPool.Put(bp)
	buf := *bp
	for {
		n, err := s.getdents(fd, buf)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if n < 0 || n > len(buf) {
			return errBadDirent
		}
		if err := parseDirents(buf[:n], emit); err != nil {
			return err
		}
	}
}
