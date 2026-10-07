//go:build !linux && !darwin

package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const supportsScopedDispatch = false

func (w *Workspace) walk(ctx context.Context, fn func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(w.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() && ignoreDirs[d.Name()] {
			return fs.SkipDir
		}
		rel, rerr := filepath.Rel(w.root, abs)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil // skip the root entry itself
		}
		slash := filepath.ToSlash(rel)
		if w.guard != nil {
			if gerr := w.guard(slash, false); gerr != nil {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		return fn(slash, d)
	})
}

func (w *Workspace) openDir(p string) (*os.File, error) {
	abs, lfi, err := w.resolveDir(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	sfi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(lfi, sfi) {
		_ = f.Close()
		return nil, errFileChanged
	}
	return f, nil
}

func (w *Workspace) openRegularFile(p string) (*os.File, error) {
	abs, lfi, err := w.resolveFile(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	sfi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(lfi, sfi) {
		_ = f.Close()
		return nil, errFileChanged
	}
	return f, nil
}

// openWalked opens by checked path after the guard decided; this backend has
// no retained descriptors and adds no #613 recheck.
func (w *Workspace) openWalked(rel string, _ fs.DirEntry) (*os.File, error) {
	return w.openRegularFile(rel)
}

func (w *Workspace) openReadDir(p string) (*os.File, string, error) {
	f, err := w.openDir(p)
	if err != nil {
		return nil, "", err
	}
	abs, err := w.cleanRel(p)
	if err != nil {
		_ = f.Close()
		return nil, "", err
	}
	rel, err := filepath.Rel(w.root, abs)
	return f, rel, err
}
func readWorkspaceEntries(f *os.File) ([]fs.DirEntry, error) { return f.ReadDir(-1) }

// readDirEntries enumerates only. The checked-path backend opened this
// directory by its checked path after the guard decided, so it adds no
// post-enumeration check (#613); other platforms keep their existing limits.
func (w *Workspace) readDirEntries(f *os.File, _ string) ([]fs.DirEntry, error) {
	return readWorkspaceEntries(f)
}

// No-op for the same reason: this backend's reads open by checked path after
// the guard decides and keep their existing limits, adding no #613 recheck.
func (w *Workspace) verifyReachable(_ string, _ *os.File) error       { return nil }
func (w *Workspace) verifyWalkedParent(_ string, _ fs.DirEntry) error { return nil }

func (w *Workspace) pinScope(string) (*os.File, string, error) {
	return nil, "", errors.New("scoped dispatch is unsupported on this platform")
}
