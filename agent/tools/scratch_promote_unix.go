//go:build linux || darwin

package tools

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// scratchPromotionSupported gates promotion-enabled factory construction:
// this platform has a behaviorally tested atomic no-replace install.
const scratchPromotionSupported = true

// fsModePermMask is the permission-bit mask used by post-commit checks.
const fsModePermMask = fs.FileMode(0o777)

// installPromotedCreate applies one captured create beneath root with a
// descriptor-anchored walk: the canonical root is opened once, every parent
// component is opened with O_NOFOLLOW relative to its predecessor, the final
// parent must carry the recorded snapshot identity and complete mode, and
// the destination must be absent. The content lands inside a reserved 0700
// staging directory, receives its approved mode there, is read back and
// synced, then is installed with an atomic no-replace rename. A crash can
// therefore leave either a protected staging file or a destination already at
// its approved mode. installed reports whether the rename landed, so later
// cleanup or durability failures remain journaled and surface as indeterminate.
func installPromotedCreate(root string, change scratchChange) (installed bool, err error) {
	rootFile, err := os.OpenFile(root, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("open canonical root: %w", err)
	}
	defer func() { _ = rootFile.Close() }()

	parent := rootFile
	parentRel := path.Dir(change.path)
	if parentRel != "." {
		for _, comp := range strings.Split(parentRel, "/") {
			fd, err := unix.Openat(int(parent.Fd()), comp,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if parent != rootFile {
				_ = parent.Close()
			}
			if err != nil {
				return false, fmt.Errorf("open parent component %q: %w", comp, err)
			}
			parent = os.NewFile(uintptr(fd), comp)
		}
		defer func() { _ = parent.Close() }()
	}

	// The pinned parent must still be the snapshot-time canonical directory:
	// identity (dev/ino) and complete fs.FileMode.
	fi, err := parent.Stat()
	if err != nil {
		return false, fmt.Errorf("stat pinned parent: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("pinned parent has no stat identity")
	}
	if uint64(st.Dev) != change.parent.dev || st.Ino != change.parent.ino {
		return false, fmt.Errorf("parent %q was replaced since the snapshot; promotion aborted", change.parent.path)
	}
	if fi.Mode() != change.parent.mode {
		return false, fmt.Errorf("parent %q mode changed since the snapshot (%v -> %v); promotion aborted",
			change.parent.path, change.parent.mode, fi.Mode())
	}

	base := path.Base(change.path)
	var destSt unix.Stat_t
	switch err := unix.Fstatat(int(parent.Fd()), base, &destSt, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return false, fmt.Errorf("destination %q already exists; promotion never overwrites", change.path)
	case !errors.Is(err, unix.ENOENT):
		return false, fmt.Errorf("probe destination %q: %w", change.path, err)
	}

	var rnd [8]byte
	if _, err := io.ReadFull(cryptorand.Reader, rnd[:]); err != nil {
		return false, fmt.Errorf("promotion temp entropy: %w", err)
	}
	stageName := scratchPromoteTempPrefix + hex.EncodeToString(rnd[:])
	if err := unix.Mkdirat(int(parent.Fd()), stageName, 0o700); err != nil {
		return false, fmt.Errorf("create promotion staging directory: %w", err)
	}
	removeStage := func() error {
		return unix.Unlinkat(int(parent.Fd()), stageName, unix.AT_REMOVEDIR)
	}
	if err := unix.Fchmodat(int(parent.Fd()), stageName, 0o700, 0); err != nil {
		_ = removeStage()
		return false, fmt.Errorf("secure promotion staging directory: %w", err)
	}
	stageFD, err := unix.Openat(int(parent.Fd()), stageName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = removeStage()
		return false, fmt.Errorf("open promotion staging directory: %w", err)
	}
	stage := os.NewFile(uintptr(stageFD), stageName)
	const tmpName = "artifact"
	tfd, err := unix.Openat(int(stage.Fd()), tmpName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		_ = stage.Close()
		_ = removeStage()
		return false, fmt.Errorf("create promotion temp: %w", err)
	}
	tmp := os.NewFile(uintptr(tfd), tmpName)
	removeTemp := func() {
		_ = tmp.Close()
		_ = unix.Unlinkat(int(stage.Fd()), tmpName, 0)
		_ = stage.Close()
		_ = removeStage()
	}
	if _, err := tmp.Write(change.data); err != nil {
		removeTemp()
		return false, fmt.Errorf("write promotion temp: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		removeTemp()
		return false, fmt.Errorf("verify promotion temp: %w", err)
	}
	readBack, err := io.ReadAll(tmp)
	if err != nil || !bytes.Equal(readBack, change.data) {
		removeTemp()
		return false, fmt.Errorf("promotion temp bytes do not match the approved content (read err: %v)", err)
	}
	if err := tmp.Chmod(change.mode.Perm()); err != nil {
		removeTemp()
		return false, fmt.Errorf("chmod promotion temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		removeTemp()
		return false, fmt.Errorf("sync promotion mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		removeTemp()
		return false, fmt.Errorf("close promotion temp: %w", err)
	}
	if err := promoteRename(int(stage.Fd()), tmpName, int(parent.Fd()), base); err != nil {
		removeTemp()
		return false, fmt.Errorf("install %q (no-replace rename, no fallback): %w", change.path, err)
	}
	closeErr := stage.Close()
	removeErr := removeStage()
	if closeErr != nil || removeErr != nil {
		return true, fmt.Errorf("clean promotion staging directory after installing %q: %w", change.path, errors.Join(closeErr, removeErr))
	}
	if err := parent.Sync(); err != nil {
		return true, fmt.Errorf("sync parent after installing %q: %w", change.path, err)
	}
	return true, nil
}
