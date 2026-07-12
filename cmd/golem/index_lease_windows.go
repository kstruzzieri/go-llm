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

type indexWriterLease struct {
	file       *os.File
	overlapped syscall.Overlapped
	unlock     *syscall.LazyProc
	once       sync.Once
	err        error
}

func acquireIndexWriterLease(dbPath string) (*indexWriterLease, error) {
	path := writerLeasePath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("golem: create index lease directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("golem: open index writer lease: %w", err)
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	lock := kernel32.NewProc("LockFileEx")
	lease := &indexWriterLease{file: f, unlock: kernel32.NewProc("UnlockFileEx")}
	r1, _, callErr := lock.Call(
		f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0,
		^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&lease.overlapped)),
	)
	if r1 == 0 {
		closeErr := wrapLeaseCloseError(f.Close())
		if errors.Is(callErr, errorLockViolation) {
			return nil, errors.Join(errIndexWriterLeaseHeld, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("golem: lock index writer lease: %w", callErr), closeErr)
	}
	return lease, nil
}

func wrapLeaseCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("golem: close index writer lease after acquire failure: %w", err)
}

func (l *indexWriterLease) Close() error {
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
