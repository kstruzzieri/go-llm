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
	errIndexWriterLeaseHeld = errors.New("workspace index writer lease is held")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx              = kernel32.NewProc("LockFileEx")
	unlockFileEx            = kernel32.NewProc("UnlockFileEx")
)

type indexWriterLease struct {
	file       *os.File
	overlapped syscall.Overlapped
	once       sync.Once
	err        error
}

func acquireIndexWriterLease(dbPath string) (*indexWriterLease, error) {
	path := writerLeasePath(dbPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create index lease directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open index writer lease: %w", err)
	}
	lease := &indexWriterLease{file: f}
	r1, _, callErr := lockFileEx.Call(
		f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0,
		^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&lease.overlapped)),
	)
	if r1 == 0 {
		_ = f.Close()
		if errors.Is(callErr, errorLockViolation) {
			return nil, errIndexWriterLeaseHeld
		}
		return nil, fmt.Errorf("lock index writer lease: %w", callErr)
	}
	return lease, nil
}

func (l *indexWriterLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		r1, _, callErr := unlockFileEx.Call(
			l.file.Fd(), 0, ^uintptr(0), ^uintptr(0), uintptr(unsafe.Pointer(&l.overlapped)),
		)
		if r1 == 0 {
			l.err = callErr
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
