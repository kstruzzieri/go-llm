package tools

import (
	"errors"
	"io/fs"
	"time"
)

// screenLimits bounds the pre-run read of credential files: bytes read per
// file, the longest record a key is taken from, the number of keys, the total
// bytes of keys held, and the walk's time budget. Hitting any of them withholds
// the command's output.
type screenLimits struct {
	fileBytes, keyBytes, keys, totalBytes int
	budget                                time.Duration
}

// defaultScreenLimits applies to every field a Workspace leaves zero.
var defaultScreenLimits = screenLimits{
	fileBytes:  262144,
	keyBytes:   16384,
	keys:       4096,
	totalBytes: 1048576,
	budget:     5 * time.Second,
}

// orDefaults returns l with each zero field replaced by its default.
func (l screenLimits) orDefaults() screenLimits {
	d := defaultScreenLimits
	if l.fileBytes == 0 {
		l.fileBytes = d.fileBytes
	}
	if l.keyBytes == 0 {
		l.keyBytes = d.keyBytes
	}
	if l.keys == 0 {
		l.keys = d.keys
	}
	if l.totalBytes == 0 {
		l.totalBytes = d.totalBytes
	}
	if l.budget == 0 {
		l.budget = d.budget
	}
	return l
}

// screenSeams are test-only failure seams for the screen's walk and reads. Each
// is asked, with the workspace-relative slash path, where the real call is
// about to happen: openErr before each file and subdirectory is opened
// (.git/config included), listErr before a directory's names are read (the
// root is ""), statErr before each listed entry's stat, readErr after each
// credential file's read succeeds. A non-nil answer is handled exactly as that
// error from the real call. A seam may change the tree and return nil.
type screenSeams struct {
	openErr, listErr, statErr, readErr func(rel string) error
}

func (s *screenSeams) open(rel string) error {
	if s == nil || s.openErr == nil {
		return nil
	}
	return s.openErr(rel)
}

func (s *screenSeams) list(rel string) error {
	if s == nil || s.listErr == nil {
		return nil
	}
	return s.listErr(rel)
}

func (s *screenSeams) stat(rel string) error {
	if s == nil || s.statErr == nil {
		return nil
	}
	return s.statErr(rel)
}

func (s *screenSeams) read(rel string) error {
	if s == nil || s.readErr == nil {
		return nil
	}
	return s.readErr(rel)
}

// skipKind is what an entry the screen could not read does to the output.
type skipKind int

const (
	// skipFatal: the workspace is not what was approved; the command is not run.
	skipFatal skipKind = iota
	// skipIgnore: nothing to read (not a regular file).
	skipIgnore
	// skipAnnounceChanged: the entry changed during the read; it is counted in a note.
	skipAnnounceChanged
	// skipWithhold: anything else; both streams are withheld.
	skipWithhold
)

// classifySkip says what err, from opening, reading or listing one entry, does
// to the output. A permission failure always withholds: neither owner nor mode
// proves an entry cannot be read some other way.
func classifySkip(err error) skipKind {
	switch {
	// ErrRootReplaced wraps errFileChanged, so it is tested first.
	case errors.Is(err, ErrRootReplaced), errors.Is(err, errAnchorUnreachable), errors.Is(err, errWalkEntryUnheld):
		return skipFatal
	case errors.Is(err, errNotRegular):
		return skipIgnore
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errFileChanged), errors.Is(err, errSymlink):
		return skipAnnounceChanged
	}
	return skipWithhold
}
