//go:build linux || darwin

package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type mutationTarget struct {
	parent  *os.File
	release func()
	name    string
	logical string
	stat    unix.Stat_t
	mode    fs.FileMode
	exists  bool
}

func sameMutationEntry(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&unix.S_IFMT == b.Mode&unix.S_IFMT
}

func regularMutationEntry(st *unix.Stat_t) error {
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errSymlink
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errNotRegular
	}
	return nil
}

// openMutationTarget owns the parent until release. Canonical policy consults
// only the final logical name, including on resolution errors, as reads do.
func (w *Workspace) openMutationTarget(p string) (target *mutationTarget, resultErr error) {
	defer func() { resultErr = w.scopedPathError(resultErr) }()
	abs, err := w.cleanRel(p)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return nil, err
	}
	policyRel := rel
	var parent, root *os.File
	var releaseRoot func()
	release := func() {
		if parent != nil && parent != root {
			_ = parent.Close()
		}
		if releaseRoot != nil {
			releaseRoot()
		}
	}
	defer func() {
		if !errors.Is(resultErr, errScopeDenied) {
			if err := w.checkScope(filepath.Join(w.root, policyRel), true); err != nil {
				resultErr = err
			}
		}
		if resultErr != nil {
			release()
			target = nil
		}
	}()
	root, releaseRoot, err = w.workspaceRoot()
	if err != nil {
		return nil, err
	}
	parent = root
	parts := strings.Split(rel, string(os.PathSeparator))
	logical := "."
	for i, part := range parts {
		last := i == len(parts)-1
		var st unix.Stat_t
		err := unix.Fstatat(int(parent.Fd()), part, &st, unix.AT_SYMLINK_NOFOLLOW)
		if last && errors.Is(err, fs.ErrNotExist) {
			return &mutationTarget{parent: parent, release: release, name: part, logical: filepath.Join(logical, part)}, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errParentMissing // path-free; says a parent, not the target, is missing
		}
		if err != nil {
			return nil, err
		}
		name, err := workspaceCanonicalName(parent, part, &st)
		if errors.Is(err, fs.ErrPermission) {
			if w.guard != nil {
				return nil, w.denyScope()
			}
			name, err = part, nil // no policy relies on this unverified spelling
		}
		if err != nil {
			return nil, err
		}
		logical = filepath.Join(logical, name)
		policyRel = filepath.Join(logical, filepath.Join(parts[i+1:]...))
		if last {
			if err := regularMutationEntry(&st); err != nil {
				return nil, err
			}
			return &mutationTarget{parent: parent, release: release, name: name, logical: logical, stat: st, mode: fs.FileMode(st.Mode & 0777), exists: true}, nil
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, errSymlink
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return nil, errNotDir
		}
		if err := w.mutationStep(mutationBeforeOpen, ""); err != nil {
			return nil, err
		}
		fd, err := unix.Openat(int(parent.Fd()), name, workspaceSearchFlags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, workspaceOpenError(err)
		}
		next := os.NewFile(uintptr(fd), name)
		var opened unix.Stat_t
		if err = unix.Fstat(fd, &opened); err == nil && !sameMutationEntry(&st, &opened) {
			err = errFileChanged
		}
		if err != nil {
			_ = next.Close()
			return nil, err
		}
		if parent != root {
			_ = parent.Close()
		}
		parent = next
	}
	return nil, errNotRegular
}

func (w *Workspace) resolveWriteTarget(p string) (string, bool, error) {
	target, err := w.openMutationTarget(p)
	if err != nil {
		return "", false, err
	}
	defer target.release()
	return filepath.Join(w.root, target.logical), target.exists, nil
}

// CanonicalPathForUndo returns the admitted canonical logical name, enforcing
// write policy against that spelling. It does not track subsequent renames.
func (w *Workspace) CanonicalPathForUndo(p string) (string, error) {
	target, err := w.openMutationTarget(p)
	if err != nil {
		return "", err
	}
	defer target.release()
	return filepath.ToSlash(target.logical), nil
}

func (w *Workspace) checkMutation(target *mutationTarget, expected *FilePrecondition) error {
	if expected == nil {
		return nil
	}
	if expected.Exists {
		if err := w.checkScope(filepath.Join(w.root, target.logical), false); err != nil {
			return err
		}
	}
	if target.exists != expected.Exists {
		return ErrPreconditionMismatch
	}
	if !target.exists {
		return nil
	}
	entry := workspaceEntry{name: target.name, stat: target.stat, dir: target.parent}
	file, err := entry.openRegular()
	if errors.Is(err, fs.ErrNotExist) {
		return ErrPreconditionMismatch
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected.Hash || expected.CheckMode && info.Mode() != expected.Mode {
		return ErrPreconditionMismatch
	}
	target.mode = info.Mode().Perm()
	return nil
}

func (target *mutationTarget) recheck(conditional bool) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(target.parent.Fd()), target.name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if !target.exists && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if conditional && errors.Is(err, fs.ErrNotExist) {
		return ErrPreconditionMismatch
	}
	if err != nil {
		return err
	}
	if err := regularMutationEntry(&st); err != nil {
		return err
	}
	if !target.exists {
		if conditional {
			return ErrPreconditionMismatch
		}
		return fs.ErrExist
	}
	if !sameMutationEntry(&target.stat, &st) {
		return errFileChanged
	}
	if !conditional {
		target.mode = fs.FileMode(st.Mode & 0777)
	}
	return nil
}

func (w *Workspace) mutateFile(p string, content []byte, expected *FilePrecondition, remove bool) (resultErr error) {
	defer func() { resultErr = w.scopedPathError(resultErr) }()
	target, err := w.openMutationTarget(p)
	if err != nil {
		return err
	}
	defer target.release()
	if err := w.mutationStep(mutationBeforeCheck, ""); err != nil {
		return err
	}
	if err := w.checkMutation(target, expected); err != nil {
		return err
	}
	if remove && !target.exists {
		return fs.ErrNotExist
	}
	if err := w.mutationStep(mutationAfterCheck, ""); err != nil {
		return err
	}
	fd := int(target.parent.Fd())
	if remove {
		if err := target.recheck(expected != nil); err != nil {
			return err
		}
		if err := w.mutationStep(mutationBeforeRemove, ""); err != nil {
			return err
		}
		err := unix.Unlinkat(fd, target.name, 0) // never remove a late directory
		if expected != nil && errors.Is(err, fs.ErrNotExist) {
			return ErrPreconditionMismatch
		}
		return err
	}
	if err := w.mutationStep(mutationBeforeTemp, ""); err != nil {
		return err
	}
	var temp *os.File
	var name string
	for range 10 {
		name = ".golem-" + rand.Text() + ".tmp"
		tmpFD, err := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		temp = os.NewFile(uintptr(tmpFD), name)
		break
	}
	if temp == nil {
		return fmt.Errorf("temporary name collisions: %w", fs.ErrExist)
	}
	var identity unix.Stat_t
	identityErr := unix.Fstat(int(temp.Fd()), &identity)
	closed, installed := false, false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, temp.Close())
		}
		if installed {
			return
		}
		if err := w.mutationStep(mutationBeforeCleanup, name); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		if identityErr != nil {
			return
		} // ownership could not be established
		var st unix.Stat_t
		err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err == nil && !sameMutationEntry(&identity, &st) {
			err = errFileChanged
		}
		if err == nil {
			err = unix.Unlinkat(fd, name, 0)
		}
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("temporary cleanup: %w", err))
		}
	}()
	if identityErr != nil {
		return identityErr
	}
	if err := w.mutationStep(mutationBeforeWrite, name); err != nil {
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return err
	}
	if err := w.mutationStep(mutationBeforeChmod, name); err != nil {
		return err
	}
	mode := fs.FileMode(0600)
	if target.exists {
		// Conditional writes keep the mode captured with their verified hash.
		if expected == nil {
			if err := target.recheck(false); err != nil {
				return err
			}
		}
		mode = target.mode
	}
	// Linux O_PATH is lookup-only (fchmod/fsync return EBADF); use the writable
	// temp descriptor, not the pinned parent. No parent durability promise.
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	_ = temp.Sync() // best effort
	if err := w.mutationStep(mutationBeforeClose, name); err != nil {
		return err
	}
	err = temp.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := w.mutationStep(mutationAfterTemp, name); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameMutationEntry(&identity, &st) {
		return errFileChanged
	}
	if err := target.recheck(expected != nil); err != nil {
		return err
	}
	err = w.mutationStep(mutationBeforeRename, name)
	if err == nil {
		if target.exists {
			err = unix.Renameat(fd, name, fd, target.name)
		} else {
			install := promoteRename
			if w.noReplaceRename != nil {
				install = w.noReplaceRename
			}
			err = install(fd, name, fd, target.name)
		}
	}
	if err != nil {
		if expected != nil && !target.exists && errors.Is(err, fs.ErrExist) {
			return ErrPreconditionMismatch
		}
		return err // unsupported no-replace never falls back to plain rename
	}
	installed = true
	return nil
}
