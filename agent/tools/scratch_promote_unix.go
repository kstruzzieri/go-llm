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
// the destination must be absent. The content lands in a reserved
// same-directory 0600 temp (written, chmodded, read back, synced) and is
// installed with an atomic no-replace rename. Every ordinary failure removes
// the temp; only the rename may leave state behind, and it either fully
// installs or fully fails.
func installPromotedCreate(root string, change scratchChange) error {
	rootFile, err := os.OpenFile(root, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open canonical root: %w", err)
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
				return fmt.Errorf("open parent component %q: %w", comp, err)
			}
			parent = os.NewFile(uintptr(fd), comp)
		}
		defer func() { _ = parent.Close() }()
	}

	// The pinned parent must still be the snapshot-time canonical directory:
	// identity (dev/ino) and complete fs.FileMode.
	fi, err := parent.Stat()
	if err != nil {
		return fmt.Errorf("stat pinned parent: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("pinned parent has no stat identity")
	}
	if uint64(st.Dev) != change.parent.dev || st.Ino != change.parent.ino {
		return fmt.Errorf("parent %q was replaced since the snapshot; promotion aborted", change.parent.path)
	}
	if fi.Mode() != change.parent.mode {
		return fmt.Errorf("parent %q mode changed since the snapshot (%v -> %v); promotion aborted",
			change.parent.path, change.parent.mode, fi.Mode())
	}

	base := path.Base(change.path)
	var destSt unix.Stat_t
	switch err := unix.Fstatat(int(parent.Fd()), base, &destSt, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return fmt.Errorf("destination %q already exists; promotion never overwrites", change.path)
	case !errors.Is(err, unix.ENOENT):
		return fmt.Errorf("probe destination %q: %w", change.path, err)
	}

	var rnd [8]byte
	if _, err := io.ReadFull(cryptorand.Reader, rnd[:]); err != nil {
		return fmt.Errorf("promotion temp entropy: %w", err)
	}
	tmpName := scratchPromoteTempPrefix + hex.EncodeToString(rnd[:])
	tfd, err := unix.Openat(int(parent.Fd()), tmpName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create promotion temp: %w", err)
	}
	tmp := os.NewFile(uintptr(tfd), tmpName)
	removeTemp := func() {
		_ = tmp.Close()
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
	}
	if _, err := tmp.Write(change.data); err != nil {
		removeTemp()
		return fmt.Errorf("write promotion temp: %w", err)
	}
	if err := tmp.Chmod(change.mode.Perm()); err != nil {
		removeTemp()
		return fmt.Errorf("chmod promotion temp: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		removeTemp()
		return fmt.Errorf("verify promotion temp: %w", err)
	}
	readBack, err := io.ReadAll(tmp)
	if err != nil || !bytes.Equal(readBack, change.data) {
		removeTemp()
		return fmt.Errorf("promotion temp bytes do not match the approved content (read err: %v)", err)
	}
	if err := tmp.Sync(); err != nil {
		removeTemp()
		return fmt.Errorf("sync promotion temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("close promotion temp: %w", err)
	}
	if err := promoteRename(int(parent.Fd()), tmpName, base); err != nil {
		_ = unix.Unlinkat(int(parent.Fd()), tmpName, 0)
		return fmt.Errorf("install %q (no-replace rename, no fallback): %w", change.path, err)
	}
	// Durability of the directory entry is best-effort: the rename was the
	// last namespace operation allowed to fail through this closure.
	_ = parent.Sync()
	return nil
}
