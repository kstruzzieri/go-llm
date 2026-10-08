//go:build windows

package tools

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows Stat defers its file ID lookup until SameFile, which could then
// observe a replacement at the saved path. A handle captures the identity now.
func workspaceRootIdentity(path string) (os.FileInfo, error) {
	// The caller supplies a canonical absolute path. Preserve long drive and
	// UNC paths when passing it directly to the Windows API.
	name := path
	switch {
	case strings.HasPrefix(name, `\\?\`), strings.HasPrefix(name, `\\.\`):
	case strings.HasPrefix(name, `\\`):
		name = `\\?\UNC\` + name[2:]
	default:
		name = `\\?\` + name
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// Metadata-only access needs no directory read permission; sharing also
	// permits existing readers/watchers and never pins the root against rename.
	handle, err := windows.CreateFile(ptr, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	identity, err := file.Stat()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return identity, err
}
