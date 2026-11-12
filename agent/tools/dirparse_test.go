//go:build linux || darwin

package tools

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// dirEntry is one (name, raw type) pair a directory read emitted.
type dirEntry struct {
	name string
	typ  uint8
}

// collect returns an emit function that appends to *got.
func collect(got *[]dirEntry) func(string, uint8) {
	return func(name string, typ uint8) { *got = append(*got, dirEntry{name, typ}) }
}

// names returns the names of got, in order.
func names(got []dirEntry) []string {
	out := make([]string, len(got))
	for i, e := range got {
		out[i] = e.name
	}
	return out
}

// direntBytes builds one Linux getdents64 record: inode, offset 0, record
// length, type, the name, a NUL and pad further zero bytes.
func direntBytes(ino uint64, typ uint8, name string, pad int) []byte {
	reclen := 19 + len(name) + 1 + pad
	b := make([]byte, reclen)
	binary.NativeEndian.PutUint64(b[0:], ino)
	binary.NativeEndian.PutUint16(b[16:], uint16(reclen))
	b[18] = typ
	copy(b[19:], name)
	return b
}

// withReclen returns rec with its record length field set to n.
func withReclen(rec []byte, n uint16) []byte {
	binary.NativeEndian.PutUint16(rec[16:], n)
	return rec
}

func TestParseDirents(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	valid := direntBytes(7, unix.DT_REG, "a", 0)
	long := strings.Repeat("n", 255)
	noNul := direntBytes(1, unix.DT_REG, "abc", 0)
	noNul[len(noNul)-1] = 'x'
	rows := []struct {
		label string
		buf   []byte
		want  []dirEntry
		err   error
	}{
		{"two records", slices.Concat(direntBytes(1, unix.DT_REG, "a.txt", 3), direntBytes(2, unix.DT_DIR, "sub", 5)),
			[]dirEntry{{"a.txt", unix.DT_REG}, {"sub", unix.DT_DIR}}, nil},
		{"dots", slices.Concat(direntBytes(1, unix.DT_DIR, ".", 0), direntBytes(2, unix.DT_DIR, "..", 0), direntBytes(3, unix.DT_REG, "x", 0)),
			[]dirEntry{{"x", unix.DT_REG}}, nil},
		{"inode zero", direntBytes(0, unix.DT_REG, "ghost", 0), []dirEntry{{"ghost", unix.DT_REG}}, nil},
		{"unknown type", direntBytes(1, unix.DT_UNKNOWN, "u", 0), []dirEntry{{"u", 0}}, nil},
		{"padding", direntBytes(1, unix.DT_REG, "p", 7), []dirEntry{{"p", unix.DT_REG}}, nil},
		{"long name", direntBytes(1, unix.DT_REG, long, 0), []dirEntry{{long, unix.DT_REG}}, nil},
		{"bytes", direntBytes(1, unix.DT_REG, "a b\n\xff\xfe", 0), []dirEntry{{"a b\n\xff\xfe", unix.DT_REG}}, nil},
		{"short header", slices.Concat(valid, direntBytes(8, unix.DT_REG, "b", 4)[:18]), []dirEntry{{"a", unix.DT_REG}}, errBadDirent},
		{"reclen zero", withReclen(direntBytes(1, unix.DT_REG, "z", 0), 0), nil, errBadDirent},
		{"reclen short", withReclen(direntBytes(1, unix.DT_REG, "z", 0), 19), nil, errBadDirent},
		{"reclen past end", withReclen(direntBytes(1, unix.DT_REG, "z", 0), 21+8), nil, errBadDirent},
		{"no nul", noNul, nil, errBadDirent},
		{"empty name", direntBytes(1, unix.DT_REG, "", 3), nil, errBadDirent},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			var got []dirEntry
			err := parseDirents(r.buf, collect(&got))
			if !errors.Is(err, r.err) {
				t.Fatalf("%s: err = %v, want %v", r.label, err, r.err)
			}
			if !slices.Equal(got, r.want) {
				t.Fatalf("%s: emitted %d entries, want %d (or a name or type differs)", r.label, len(got), len(r.want))
			}
		})
	}
}
