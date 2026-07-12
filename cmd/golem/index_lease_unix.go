//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var errIndexWriterLeaseHeld = errors.New("golem: workspace index writer lease is held")

type indexWriterLease struct {
	file *os.File
	once sync.Once
	err  error
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
	if err := f.Chmod(0o600); err != nil {
		closeErr := f.Close()
		return nil, errors.Join(fmt.Errorf("golem: chmod index writer lease: %w", err), wrapLeaseCloseError(closeErr))
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := wrapLeaseCloseError(f.Close())
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(errIndexWriterLeaseHeld, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("golem: lock index writer lease: %w", err), closeErr)
	}
	return &indexWriterLease{file: f}, nil
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
		if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
			l.err = fmt.Errorf("golem: unlock index writer lease: %w", err)
		}
		if err := l.file.Close(); err != nil {
			l.err = errors.Join(l.err, fmt.Errorf("golem: close index writer lease: %w", err))
		}
	})
	return l.err
}
