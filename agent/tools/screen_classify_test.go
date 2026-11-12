package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

func TestClassifySkip(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	rows := []struct {
		label string
		err   error
		want  skipKind
	}{
		// ErrRootReplaced wraps errFileChanged, so it must be tested first.
		{"root replaced", ErrRootReplaced, skipFatal},
		{"anchor unreachable", fmt.Errorf("%w: %w", errAnchorUnreachable, syscall.EACCES), skipFatal},
		{"entry unheld", errWalkEntryUnheld, skipFatal},
		{"not regular", errNotRegular, skipIgnore},
		{"not exist", fs.ErrNotExist, skipAnnounceChanged},
		{"file changed", errFileChanged, skipAnnounceChanged},
		{"symlink", errSymlink, skipAnnounceChanged},
		{"permission", fs.ErrPermission, skipWithhold},
		{"eacces", syscall.EACCES, skipWithhold},
		{"eperm", syscall.EPERM, skipWithhold},
		{"eio", syscall.EIO, skipWithhold},
		{"bare", errors.New("boom"), skipWithhold},
		{"unexamined", errWalkEntryUnexamined, skipWithhold},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			if got := classifySkip(r.err); got != r.want {
				t.Fatalf("%s: classifySkip = %d, want %d", r.label, got, r.want)
			}
		})
	}
	for label, lenient := range map[string]error{
		"not regular":  errNotRegular,
		"file changed": errFileChanged,
		"not exist":    fs.ErrNotExist,
		"symlink":      errSymlink,
	} {
		if errors.Is(errWalkEntryUnexamined, lenient) {
			t.Fatalf("%s: the sentinel matches a lenient error", label)
		}
	}
}
