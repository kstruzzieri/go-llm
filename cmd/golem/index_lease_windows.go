//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	errIndexWriterLeaseHeld = errors.New("golem: workspace index writer lease is held")
)

// flockLease is an exclusive OS-level file lock. The kernel releases it when
// the owning process exits, so a crash never leaves the path stuck locked.
type flockLease struct {
	file       *os.File
	overlapped syscall.Overlapped
	unlock     *syscall.LazyProc
	once       sync.Once
	err        error
}

func acquireIndexWriterLease(dbPath string) (*flockLease, error) {
	path := writerLeasePath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("golem: create index lease directory: %w", err)
	}
	return acquireFlockLease(path)
}

// acquireFlockLease takes an exclusive non-blocking lock on path, creating the
// file if needed. Contention is reported as errIndexWriterLeaseHeld. The lock
// file deliberately persists after Close: unlinking a path another process may
// already hold open would let two owners lock different inodes at once.
func acquireFlockLease(path string) (*flockLease, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("golem: open lease %s: %w", path, err)
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	lock := kernel32.NewProc("LockFileEx")
	lease := &flockLease{file: f, unlock: kernel32.NewProc("UnlockFileEx")}
	r1, _, callErr := lock.Call(
		f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0,
		^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&lease.overlapped)),
	)
	if r1 == 0 {
		closeErr := wrapLeaseCloseError(f.Close())
		if errors.Is(callErr, errorLockViolation) {
			return nil, errors.Join(errIndexWriterLeaseHeld, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("golem: lock lease %s: %w", path, callErr), closeErr)
	}
	return lease, nil
}

func wrapLeaseCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("golem: close index writer lease after acquire failure: %w", err)
}

func (l *flockLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		r1, _, callErr := l.unlock.Call(
			l.file.Fd(), 0, ^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&l.overlapped)),
		)
		if r1 == 0 {
			l.err = fmt.Errorf("golem: unlock index writer lease: %w", callErr)
		}
		if err := l.file.Close(); err != nil {
			l.err = errors.Join(l.err, fmt.Errorf("golem: close index writer lease: %w", err))
		}
	})
	return l.err
}
