//go:build unix

package mcpclient

import (
	"errors"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const pinNonblock = syscall.O_NONBLOCK

func pinOwnerModeOK(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.Mode().Perm()&0077 == 0
}

func chmodPinFile(f *os.File) error { return f.Chmod(0600) }
func syncPinDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type pinLease struct {
	file *os.File
	once sync.Once
	err  error
}

func lockPinFile(f *os.File) (*pinLease, error) {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errPinContention
		}
		return nil, err
	}
	return &pinLease{file: f}, nil
}

func (l *pinLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close()) })
	return l.err
}
