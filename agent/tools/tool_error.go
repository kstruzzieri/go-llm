package tools

import (
	"context"
	"errors"
	"io/fs"
)

// toolErrMessage maps an internal filesystem or scope failure to the fixed,
// path-free message the four read-only file tools (read_file, search, glob,
// list) place in ToolResult.Content. It is the single disclosure boundary for
// post-check open/read/ReadDir/WalkDir failures too, so a TOCTOU race cannot
// bypass a ScopeGuard and reveal the canonical root. It never concatenates
// the cause: the model already holds its own call arguments, so repeating the
// requested path would only add another disclosure surface.
func toolErrMessage(err error) string {
	switch {
	case errors.Is(err, errScopeDenied):
		return errScopeDenied.Error() // single source for the denial text
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "filesystem operation canceled"
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errParentMissing):
		return "path not found"
	case errors.Is(err, fs.ErrPermission):
		return "path is not accessible"
	case errors.Is(err, errNUL), errors.Is(err, errAbsPath), errors.Is(err, errEscape):
		return "path is outside the workspace"
	case errors.Is(err, errSymlink):
		return "symlinks are not followed"
	case errors.Is(err, errNotRegular), errors.Is(err, errNotDir):
		return "path has the wrong type"
	case errors.Is(err, errFileChanged):
		return "path changed during access"
	default:
		return "filesystem operation failed"
	}
}

// toolVisibleError removes host-only ScopeGuard details before a mutating tool
// exposes an error through Plan or ToolResult. Other diagnostics are unchanged.
func toolVisibleError(err error) error {
	if errors.Is(err, errScopeDenied) {
		return errScopeDenied
	}
	return err
}
