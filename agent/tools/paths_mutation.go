package tools

import (
	"errors"
	"fmt"
	"io/fs"
)

// ErrPreconditionMismatch reports that an inspected file no longer matches the
// expected existence, content hash, or checked mode, including when another
// regular file replaced it (an editor's rename-over). Inspection/cleanup errors
// retain their own identity; callers must not discard joined errors.
var ErrPreconditionMismatch = errors.New("file precondition mismatch")

// FilePrecondition describes expected content for one conditional mutation.
// Existing files require a lowercase SHA-256 ContentHash. CheckMode adds a full
// mode check; it never replaces the content check. Absent files require zero
// values for Hash, Mode and CheckMode. Mode must be zero when CheckMode is false.
type FilePrecondition struct {
	Exists    bool
	Hash      string
	Mode      fs.FileMode
	CheckMode bool
}

func (p FilePrecondition) validate(remove bool) error {
	valid := p.Exists || !remove
	if !p.Exists {
		valid = valid && p.Hash == "" && p.Mode == 0 && !p.CheckMode
	} else {
		valid = valid && len(p.Hash) == 64 && (p.CheckMode || p.Mode == 0)
		for _, c := range p.Hash {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				valid = false
				break
			}
		}
	}
	if !valid {
		return fmt.Errorf("invalid file precondition: %w", fs.ErrInvalid)
	}
	return nil
}

// WriteFileAtomic installs content with an atomic rename. On Linux/Darwin it
// acts through the admitted parent descriptor even if that directory is moved.
// A target admitted absent uses no-replace: a concurrent create is refused,
// including for this unconditional method. Existing targets retain permissions;
// new files use 0600. Sync is best-effort, not a crash-durability guarantee.
// Other platforms retain checked-path best-effort behavior without these race
// guarantees. Overwrites are not compare-and-swap of the inspected leaf inode.
func (w *Workspace) WriteFileAtomic(p string, content []byte) error {
	return w.mutateFile(p, content, nil, false)
}

// WriteFileAtomicIfMatch checks expected and writes through the same admitted
// parent on Linux/Darwin. Existing content requires read permission and read
// policy authorization. A late change to an already-checked inode's bytes or a
// final basename substitution is not a kernel content/identity CAS. Other
// platforms check state before a separate best-effort checked-path mutation.
func (w *Workspace) WriteFileAtomicIfMatch(p string, content []byte, expected FilePrecondition) error {
	if err := expected.validate(false); err != nil {
		return err
	}
	return w.mutateFile(p, content, &expected, false)
}

// RemoveFile removes a checked regular leaf. Linux/Darwin anchor deletion to the
// admitted parent and never remove a directory or follow a final symlink; a
// last-instant replacement entry may still be unlinked. Other platforms retain
// the legacy checked-path behavior. No content-read permission is required.
func (w *Workspace) RemoveFile(p string) error { return w.mutateFile(p, nil, nil, true) }

// RemoveFileIfMatch verifies expected content and optional complete mode before
// deletion, with the same permission/platform/late-leaf limits as
// WriteFileAtomicIfMatch. expected.Exists must be true.
func (w *Workspace) RemoveFileIfMatch(p string, expected FilePrecondition) error {
	if err := expected.validate(true); err != nil {
		return err
	}
	return w.mutateFile(p, nil, &expected, true)
}

type mutationPhase string

const (
	mutationBeforeOpen    mutationPhase = "before-open"
	mutationBeforeCheck   mutationPhase = "before-check"
	mutationAfterCheck    mutationPhase = "after-check"
	mutationBeforeTemp    mutationPhase = "before-temp"
	mutationBeforeWrite   mutationPhase = "before-write"
	mutationBeforeChmod   mutationPhase = "before-chmod"
	mutationBeforeClose   mutationPhase = "before-close"
	mutationAfterTemp     mutationPhase = "after-temp"
	mutationBeforeRename  mutationPhase = "before-rename"
	mutationBeforeRemove  mutationPhase = "before-remove"
	mutationBeforeCleanup mutationPhase = "before-cleanup"
)

// temp is a basename within the held parent, never an ambient cleanup path.
func (w *Workspace) mutationStep(phase mutationPhase, temp string) error {
	if w.beforeMutation != nil {
		return w.beforeMutation(phase, temp)
	}
	return nil
}
