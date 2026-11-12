package tools

import (
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// getdentsStep is one scripted answer of a getdents stub.
type getdentsStep struct {
	batch   []byte
	n       int  // used when batch is nil
	pastBuf bool // return len(buf)+1
	err     error
}

// TestDirReadSeams drives the Linux read through a scripted getdents.
func TestDirReadSeams(t *testing.T) {
	rec := func(name string) []byte { return direntBytes(1, unix.DT_REG, name, 2) }
	rows := []struct {
		label string
		steps []getdentsStep
		want  []string
		err   error
	}{
		{"eintr", []getdentsStep{{batch: rec("a")}, {err: syscall.EINTR}, {batch: rec("b")}}, []string{"a", "b"}, nil},
		{"short reads", []getdentsStep{{batch: rec("a")}, {batch: rec("b")}, {batch: rec("c")}}, []string{"a", "b", "c"}, nil},
		{"error after records", []getdentsStep{{batch: rec("a")}, {err: syscall.EIO}}, nil, syscall.EIO},
		{"bad record", []getdentsStep{{batch: slices.Concat(rec("a"), rec("b")[:18])}}, nil, errBadDirent},
		{"negative count", []getdentsStep{{n: -1}}, nil, errBadDirent},
		{"count past buffer", []getdentsStep{{pastBuf: true}}, nil, errBadDirent},
	}
	f, err := os.Open(t.TempDir())
	if err != nil {
		failNoPath(t, "seams", err)
	}
	defer func() { _ = f.Close() }()
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			calls := 0
			stub := func(_ int, buf []byte) (int, error) {
				calls++
				if calls > 3 && r.label == "bad record" || calls > 1 && r.label == "count past buffer" {
					t.Fatalf("%s: getdents called %d times", r.label, calls)
				}
				if calls > len(r.steps) {
					return 0, nil
				}
				s := r.steps[calls-1]
				if s.err != nil {
					return 0, s.err
				}
				if s.pastBuf {
					return len(buf) + 1, nil
				}
				if s.batch == nil {
					return s.n, nil
				}
				return copy(buf, s.batch), nil
			}
			var got []dirEntry
			err := dirSys{getdents: stub}.read(f, collect(&got))
			if !errors.Is(err, r.err) {
				t.Fatalf("%s: err = %v, want %v", r.label, err, r.err)
			}
			if r.err != nil {
				return
			}
			if !slices.Equal(names(got), r.want) {
				t.Fatalf("%s: got %d names, want %d (or one differs)", r.label, len(got), len(r.want))
			}
		})
	}
}
