//go:build !linux && !darwin

package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
)

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
