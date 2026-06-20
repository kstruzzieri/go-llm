package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Shared caps and markers for the read-only file tools. Each tool also sets an
// explicit Effect.OutputCap above its self-cap, because the runtime default
// (agent/types.go defaultOutputCap = 64 KiB) would otherwise silently truncate
// larger results in agent/dispatch.go capOutput.
const (
	readFileMaxBytes = 256 * 1024 // read_file content ceiling
	searchMaxMatches = 200        // search: max match lines
	searchMaxBytes   = 64 * 1024  // search: max bytes of joined output
	listMaxEntries   = 1000       // glob/list: max emitted entries
	markerHeadroom   = 256        // bytes reserved for an in-band truncation marker

	// listOutputCap is the byte backstop for the entry-listing tools (glob, list).
	// Their primary limit is the count cap (listMaxEntries); this byte ceiling is
	// set generously above a typical 1000-entry render so the in-band truncation
	// marker is not itself cut by the runtime. Paths are unbounded in theory, so a
	// pathological tree of very long paths may still be runtime-capped.
	listOutputCap = 512 * 1024
)

// ignoreDirs are directory names skipped during tree walks (search, glob).
var ignoreDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	".superpowers": true,
}

var (
	errEmptyRoot   = errors.New("empty workspace root")
	errNUL         = errors.New("path contains NUL byte")
	errAbsPath     = errors.New("absolute paths are not allowed")
	errEscape      = errors.New("path escapes the workspace root")
	errSymlink     = errors.New("symlinks are not followed")
	errNotRegular  = errors.New("not a regular file")
	errNotDir      = errors.New("not a directory")
	errFileChanged = errors.New("file identity changed between stat and open")
)

// Workspace is the single audited chokepoint for read-only filesystem access.
// The canonical root is resolved once at construction; no path component is ever
// resolved through a symlink afterwards (v1 symlink policy: never follow).
type Workspace struct {
	root string // canonical absolute root, post-EvalSymlinks, no trailing separator
}

// NewWorkspace canonicalizes root via filepath.EvalSymlinks so the root itself
// may legitimately be a symlinked path while no later access follows a symlink.
func NewWorkspace(root string) (*Workspace, error) {
	if root == "" {
		return nil, errEmptyRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("tools: abs root: %w", err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("tools: resolve root: %w", err)
	}
	return &Workspace{root: canon}, nil
}

// underRoot reports whether a cleaned absolute candidate is the root or strictly
// under it. The separator-terminated prefix check prevents a sibling like
// "/x/rootx" from passing for root "/x/root".
func (w *Workspace) underRoot(candidate string) bool {
	if candidate == w.root {
		return true
	}
	return strings.HasPrefix(candidate, w.root+string(os.PathSeparator))
}

// cleanRel rejects NUL bytes and absolute inputs, joins (and cleans) the input
// against the canonical root, and verifies containment. It does NOT touch the
// filesystem — kind/symlink checks are the caller's job (resolveFile/resolveDir).
func (w *Workspace) cleanRel(p string) (string, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return "", errNUL
	}
	if filepath.IsAbs(p) {
		return "", errAbsPath
	}
	joined := filepath.Join(w.root, p) // Join cleans ".." segments
	if !w.underRoot(joined) {
		return "", errEscape
	}
	return joined, nil
}

// walk drives filepath.WalkDir from the canonical root, skipping ignore-set
// directories and never descending symlinks (WalkDir does not follow them).
// fn receives slash-normalized relative paths. ctx cancellation aborts the walk.
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
		return fn(filepath.ToSlash(rel), d)
	})
}

// resolveFile contains a path to a concrete file: cleanRel for containment, then
// Lstat to reject symlinks (never follow) and require a regular file. The
// returned FileInfo is the pre-open Lstat result; openRegularFile re-stats the
// open handle and compares with os.SameFile to close the symlink-swap TOCTOU window.
func (w *Workspace) resolveFile(p string) (string, os.FileInfo, error) {
	abs, err := w.cleanRel(p)
	if err != nil {
		return "", nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", nil, errSymlink
	}
	if !fi.Mode().IsRegular() {
		return "", nil, errNotRegular
	}
	return abs, fi, nil
}

// resolveDir contains a path to a concrete directory: cleanRel, then Lstat to
// reject a symlinked directory target (never follow) and require a real dir.
func (w *Workspace) resolveDir(p string) (string, error) {
	abs, err := w.cleanRel(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", errSymlink
	}
	if !fi.IsDir() {
		return "", errNotDir
	}
	return abs, nil
}

// openRegularFile is the single helper for content reads. It first resolves the
// path through resolveFile (containment + Lstat + symlink/kind checks), then opens
// the file and verifies the open handle still refers to the same file Lstat saw.
// Returns errFileChanged if the open handle no longer matches the file Lstat saw
// (e.g. a regular-file swap between Lstat and Open). Callers own the returned file
// and must close it.
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
