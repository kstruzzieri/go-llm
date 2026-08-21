//go:build windows

package profiles

import "golang.org/x/sys/windows"

// syncDir flushes a directory handle: os.File.Sync on a directory fails on
// Windows (no GENERIC_WRITE), so this opens with backup semantics and
// flushes explicitly. Mirrors config's syncDirectory pair — kept in
// lockstep.
func syncDir(dir string) error {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(
		path,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return err
	}
	if err := windows.FlushFileBuffers(h); err != nil {
		_ = windows.CloseHandle(h)
		return err
	}
	return windows.CloseHandle(h)
}
