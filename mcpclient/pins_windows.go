//go:build windows

package mcpclient

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

const pinNonblock = 0

// Windows follows the project convention of relying on user-profile ACLs.
func pinOwnerModeOK(os.FileInfo) bool { return true }
func chmodPinFile(*os.File) error     { return nil }
func syncPinDirectory(root *os.Root) error {
	path, err := windows.UTF16PtrFromString(root.Name())
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), root.Name())
	before, err := root.Stat(".")
	if err != nil {
		return errors.Join(err, f.Close())
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return errors.Join(errors.New("mcpclient: pin sync directory changed"), err, f.Close())
	}
	return errors.Join(windows.FlushFileBuffers(h), f.Close())
}

type pinLease struct {
	file       *os.File
	overlapped windows.Overlapped
	once       sync.Once
	err        error
}

func lockPinFile(f *os.File) (*pinLease, error) {
	lease := &pinLease{file: f}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xffffffff, 0xffffffff, &lease.overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, errPinContention
	}
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (l *pinLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		l.err = errors.Join(windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 0xffffffff, 0xffffffff, &l.overlapped), l.file.Close())
	})
	return l.err
}
