//go:build windows

package signing

import "golang.org/x/sys/windows"

func syncDirectory(dir string) error {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
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
	if err := windows.FlushFileBuffers(handle); err != nil {
		_ = windows.CloseHandle(handle)
		return err
	}
	return windows.CloseHandle(handle)
}
