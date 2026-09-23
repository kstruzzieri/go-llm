//go:build !linux && !darwin

package tools

import (
	"os"
	"path/filepath"
)

// The portable backend retains checked-path best-effort behavior. It does not
// provide the Unix same-parent or atomic absent-target no-replace guarantees.
func (w *Workspace) mutateFile(p string, content []byte, expected *FilePrecondition, remove bool) error {
	if expected != nil {
		_, exists, err := w.resolveWriteTarget(p)
		if err != nil {
			return err
		}
		if exists != expected.Exists {
			return ErrPreconditionMismatch
		}
		if exists {
			hash, mode, err := w.HashFileWithMode(p)
			if err != nil {
				return err
			}
			if hash != expected.Hash || expected.CheckMode && mode != expected.Mode {
				return ErrPreconditionMismatch
			}
		}
	}
	if remove {
		return w.legacyRemoveFile(p)
	}
	return w.legacyWriteFileAtomic(p, content)
}

// resolveWriteTarget contains a path to a write destination: cleanRel containment,
// symlink-free ancestry, and an existing parent directory. If the leaf exists it
// must be a regular file (never overwrite a symlink or directory). priorExists
// reports whether the leaf already exists. No filesystem mutation occurs here.
func (w *Workspace) resolveWriteTarget(p string) (abs string, priorExists bool, err error) {
	abs, err = w.cleanRel(p)
	if err != nil {
		return "", false, err
	}
	if err := w.checkScope(abs, true); err != nil {
		return "", false, err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
		return "", false, err
	}
	if fi, perr := os.Lstat(filepath.Dir(abs)); perr != nil || !fi.IsDir() {
		return "", false, errParentMissing
	}
	fi, lerr := os.Lstat(abs)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return abs, false, nil // new file
		}
		return "", false, lerr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", false, errSymlink
	}
	if !fi.Mode().IsRegular() {
		return "", false, errNotRegular
	}
	return abs, true, nil
}

// CanonicalPathForUndo returns the cleaned workspace-relative spelling used by
// the filesystem for a current or future write target.
func (w *Workspace) CanonicalPathForUndo(p string) (string, error) {
	abs, _, err := w.resolveWriteTarget(p)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalFuturePath(w.root, abs)
	if err != nil {
		return "", err
	}
	if !w.underRoot(canonical) {
		return "", errEscape
	}
	rel, err := filepath.Rel(w.root, canonical)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// legacyWriteFileAtomic preserves the portable checked-path implementation.
func (w *Workspace) legacyWriteFileAtomic(p string, content []byte) error {
	abs, priorExists, err := w.resolveWriteTarget(p)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if priorExists {
		fi, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".golem-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	_ = tmp.Sync() // best-effort durability; not a crash guarantee
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Re-validate just before rename; reject a symlink/dir swapped in meanwhile.
	if _, _, err := w.resolveWriteTarget(p); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// legacyRemoveFile preserves the portable checked-path implementation.
func (w *Workspace) legacyRemoveFile(p string) error {
	abs, err := w.cleanRel(p)
	if err != nil {
		return err
	}
	if err := w.checkScope(abs, true); err != nil {
		return err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	if !fi.Mode().IsRegular() {
		return errNotRegular
	}
	return os.Remove(abs)
}
