//go:build linux || darwin

package tools

import (
	"bytes"
	"encoding/binary"
)

// direntFixed is the fixed part of a Linux getdents64 record: d_ino (8),
// d_off (8), d_reclen (2), d_type (1). The name starts right after it.
const direntFixed = 19

// parseDirents emits the name and raw d_type of each getdents64 record in
// buf, leaving out "." and "..". A record with inode 0 is emitted like any
// other, as os.File.Readdirnames does on Linux. Any record that does not
// parse (its fixed part or its length runs past buf, its length leaves no
// room for a name, or its name is empty or has no NUL) is errBadDirent; the
// caller then discards what was emitted. It is a pure function of bytes, so
// it builds and is tested on Darwin too although only the Linux read calls it.
func parseDirents(buf []byte, emit func(name string, typ uint8)) error {
	for len(buf) > 0 {
		if len(buf) < direntFixed {
			return errBadDirent
		}
		reclen := int(binary.NativeEndian.Uint16(buf[16:]))
		if reclen < direntFixed+1 || reclen > len(buf) {
			return errBadDirent
		}
		nameLen := bytes.IndexByte(buf[direntFixed:reclen], 0)
		if nameLen <= 0 {
			return errBadDirent
		}
		name := string(buf[direntFixed : direntFixed+nameLen])
		if name != "." && name != ".." {
			emit(name, buf[18])
		}
		buf = buf[reclen:]
	}
	return nil
}
