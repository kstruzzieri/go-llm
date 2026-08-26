package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
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
	errEmptyRoot     = errors.New("empty workspace root")
	errNUL           = errors.New("path contains NUL byte")
	errAbsPath       = errors.New("absolute paths are not allowed")
	errEscape        = errors.New("path escapes the workspace root")
	errSymlink       = errors.New("symlinks are not followed")
	errNotRegular    = errors.New("not a regular file")
	errNotDir        = errors.New("not a directory")
	errFileChanged   = errors.New("file identity changed between stat and open")
	errParentMissing = errors.New("parent directory does not exist")
	// errScopeDenied marks guard vetoes for sanitized tool output. Its text is
	// the stable model-visible denial message.
	errScopeDenied = errors.New("path denied by workspace policy")
)

type scopeDeniedError struct{ cause error }

func (e scopeDeniedError) Error() string        { return e.cause.Error() }
func (e scopeDeniedError) Unwrap() error        { return e.cause }
func (e scopeDeniedError) Is(target error) bool { return target == errScopeDenied }

// ScopeGuard optionally vetoes an access by workspace-relative slash path. write
// is true for mutations (write/remove), false for reads/listing. A non-nil error
// denies the access. Enforced below any approver — an approved call still fails
// if the guard denies it. Point lookups pass only the final cleaned relative
// path — never its ancestors — so a guard must deny descendants itself (deny
// "secrets" AND "secrets/..."). Directory walks consult it per entry and skip
// denied directories.
type ScopeGuard func(rel string, write bool) error

// Workspace is the single audited chokepoint for all filesystem access within the
// agent workspace. The canonical root is resolved once at construction; no path
// component is ever resolved through a symlink afterwards (symlink policy: never
// follow).
type Workspace struct {
	root  string     // canonical absolute root; volume roots retain their separator
	guard ScopeGuard // nil => allow everything (default)
}

// CanonicalWorkspaceRoot resolves a workspace root to its absolute, symlink-free
// path using the spelling stored by the filesystem. The final step collapses
// case and Unicode aliases on filesystems that support them without folding
// distinct names on case-sensitive filesystems.
func CanonicalWorkspaceRoot(root string) (string, error) {
	if root == "" {
		return "", errEmptyRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("tools: abs root: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("tools: resolve root: %w", err)
	}
	canon, err := canonicalExistingPath(abs)
	if err != nil {
		return "", fmt.Errorf("tools: canonicalize root: %w", err)
	}
	return canon, nil
}

// NewWorkspace canonicalizes root so the root itself may legitimately be a
// symlinked or filesystem-aliased path while no later access follows a symlink.
func NewWorkspace(root string) (*Workspace, error) {
	canon, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	return &Workspace{root: canon}, nil
}

func canonicalExistingPath(path string) (string, error) {
	volume := filepath.VolumeName(path)
	current := volume + string(os.PathSeparator)
	rest := strings.TrimPrefix(path, volume)
	rest = strings.TrimLeft(rest, string(os.PathSeparator))
	if rest == "" {
		return filepath.Clean(current), nil
	}
	for _, part := range strings.Split(rest, string(os.PathSeparator)) {
		candidate := filepath.Join(current, part)
		target, err := os.Stat(candidate)
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		actual := ""
		for _, entry := range entries {
			if entry.Name() == part {
				actual = part
				break
			}
		}
		if actual == "" {
			for _, entry := range entries {
				info, err := entry.Info()
				if err == nil && os.SameFile(info, target) {
					actual = entry.Name()
					break
				}
			}
		}
		if actual == "" {
			return "", fmt.Errorf("filesystem entry for %q disappeared", candidate)
		}
		current = filepath.Join(current, actual)
	}
	return filepath.Clean(current), nil
}

func canonicalFuturePath(path string) (string, error) {
	current := path
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			canonical, err := canonicalExistingPath(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, suffix[i])
			}
			return filepath.Clean(canonical), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// SetScopeGuard installs (or clears with nil) the proof-mode scope guard.
func (w *Workspace) SetScopeGuard(g ScopeGuard) { w.guard = g }

// checkScope consults the guard for a cleaned absolute path. A veto preserves
// the host error while marking it for sanitized model-visible tool output.
func (w *Workspace) checkScope(abs string, write bool) error {
	if w.guard == nil {
		return nil
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return err
	}
	if err := w.guard(filepath.ToSlash(rel), write); err != nil {
		return scopeDeniedError{cause: err}
	}
	return nil
}

// underRoot reports whether a cleaned absolute candidate is the root or strictly
// under it. The separator-terminated prefix check prevents a sibling like
// "/x/rootx" from passing for root "/x/root".
func (w *Workspace) underRoot(candidate string) bool {
	if candidate == w.root {
		return true
	}
	prefix := w.root
	if !os.IsPathSeparator(prefix[len(prefix)-1]) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(candidate, prefix)
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

// rejectSymlinkAncestors verifies every parent component below the workspace
// root is a real directory. A final-component Lstat is not enough because
// os.Open("linkdir/file") would otherwise follow the intermediate symlink.
func (w *Workspace) rejectSymlinkAncestors(abs string) error {
	abs = filepath.Clean(abs)
	if !w.underRoot(abs) {
		return errEscape
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil {
		return err
	}
	dir := filepath.Dir(rel)
	if dir == "." || dir == "" {
		return nil
	}

	cur := w.root
	for _, part := range strings.Split(dir, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errSymlink
		}
		if !fi.IsDir() {
			return errNotDir
		}
	}
	return nil
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

// resolveFile contains a path to a concrete file: cleanRel for containment,
// Lstat checks for every parent component, then final Lstat to reject symlinks
// (never follow) and require a regular file. The returned FileInfo is the
// pre-open Lstat result; openRegularFile re-stats the open handle and compares
// with os.SameFile to close the final-component symlink-swap TOCTOU window.
func (w *Workspace) resolveFile(p string) (string, os.FileInfo, error) {
	abs, err := w.cleanRel(p)
	if err != nil {
		return "", nil, err
	}
	if err := w.checkScope(abs, false); err != nil {
		return "", nil, err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
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

// resolveDir contains a path to a concrete directory: cleanRel, Lstat checks for
// every parent component, then final Lstat to reject a symlinked directory
// target (never follow) and require a real dir. The returned FileInfo is the
// pre-open Lstat result; openDir re-stats the open handle and compares with
// os.SameFile to close the final-component symlink-swap TOCTOU window.
func (w *Workspace) resolveDir(p string) (string, os.FileInfo, error) {
	abs, err := w.cleanRel(p)
	if err != nil {
		return "", nil, err
	}
	if err := w.checkScope(abs, false); err != nil {
		return "", nil, err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
		return "", nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", nil, errSymlink
	}
	if !fi.IsDir() {
		return "", nil, errNotDir
	}
	return abs, fi, nil
}

// openDir is the single helper for directory listing. It resolves the path
// through resolveDir (containment + Lstat + symlink/kind checks), opens the
// directory, and verifies the open handle still refers to the same directory
// Lstat saw — closing the final-component symlink-swap TOCTOU window that a bare
// resolveDir+os.ReadDir would leave (os.ReadDir follows a final-component symlink).
// It returns errFileChanged if the identity no longer matches. Callers own the
// returned file and must close it; read entries via f.ReadDir.
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

// NewFileToolsForWorkspace builds read-only tools over an existing workspace.
// Use this when a caller has installed a ScopeGuard that must apply to reads,
// search, glob, and list.
func NewFileToolsForWorkspace(ws *Workspace) []agent.Tool {
	return []agent.Tool{
		NewReadFile(ws),
		NewSearch(ws),
		NewGlob(ws),
		NewList(ws),
	}
}

// NewFileTools builds the full read-only tool set bound to a single workspace
// root: read_file, search, glob, list. The consumer (e.g. cmd/golem) registers
// the returned slice with the agent loop. All four are Read / ApprovalNever.
func NewFileTools(root string) ([]agent.Tool, error) {
	ws, err := NewWorkspace(root)
	if err != nil {
		return nil, err
	}
	return NewFileToolsForWorkspace(ws), nil
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

// resolveWriteTarget contains a path to a write destination: cleanRel containment,
// symlink-free ancestry, and an existing parent directory. If the leaf exists it
// must be a regular file (never overwrite a symlink or directory). priorExists
// reports whether the leaf already exists. No filesystem mutation occurs here.
func (w *Workspace) resolveWriteTarget(p string) (abs string, priorExists bool, err error) {
	abs, err = w.cleanRel(p)
	if err != nil {
		return "", false, err
	}
	if err := w.checkScope(abs, true); err != nil {
		return "", false, err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
		return "", false, err
	}
	if fi, perr := os.Lstat(filepath.Dir(abs)); perr != nil || !fi.IsDir() {
		return "", false, errParentMissing
	}
	fi, lerr := os.Lstat(abs)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return abs, false, nil // new file
		}
		return "", false, lerr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", false, errSymlink
	}
	if !fi.Mode().IsRegular() {
		return "", false, errNotRegular
	}
	return abs, true, nil
}

// CanonicalPathForUndo returns the cleaned workspace-relative spelling used by
// the filesystem for a current or future write target.
func (w *Workspace) CanonicalPathForUndo(p string) (string, error) {
	abs, _, err := w.resolveWriteTarget(p)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalFuturePath(abs)
	if err != nil {
		return "", err
	}
	if !w.underRoot(canonical) {
		return "", errEscape
	}
	rel, err := filepath.Rel(w.root, canonical)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// WriteFileAtomic writes content to a workspace-relative path by creating a temp
// file in the SAME directory, syncing best-effort, and renaming over the target.
// It re-validates the target through resolveWriteTarget immediately before the
// rename so a symlink swapped in after planning is rejected. On POSIX, rename
// replaces a final symlink rather than following it, so containment holds even if
// the final component changes after the last check. This is NOT a crash-durability
// contract (undo is in-memory).
func (w *Workspace) WriteFileAtomic(p string, content []byte) error {
	abs, priorExists, err := w.resolveWriteTarget(p)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if priorExists {
		fi, err := os.Lstat(abs)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".golem-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	_ = tmp.Sync() // best-effort durability; not a crash guarantee
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Re-validate just before rename; reject a symlink/dir swapped in meanwhile.
	if _, _, err := w.resolveWriteTarget(p); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// RemoveFile deletes a single regular file inside the workspace. It refuses
// directories and symlinks and enforces containment. Used by /undo to revert a
// file that did not exist before a mutation.
func (w *Workspace) RemoveFile(p string) error {
	abs, err := w.cleanRel(p)
	if err != nil {
		return err
	}
	if err := w.checkScope(abs, true); err != nil {
		return err
	}
	if err := w.rejectSymlinkAncestors(abs); err != nil {
		return err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errSymlink
	}
	if !fi.Mode().IsRegular() {
		return errNotRegular
	}
	return os.Remove(abs)
}

// readAll reads a workspace-relative regular file in full, TOCTOU-hardened via
// openRegularFile. It is the write-side counterpart used by Plan/Invoke to snapshot
// current content; callers size-check the result against mutateMaxBytes.
func (w *Workspace) readAll(p string) ([]byte, error) {
	f, err := w.openRegularFile(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// ReadFileForUndo returns the full bytes of a workspace-relative regular file for
// undo verification. It applies the same containment + never-follow-symlink checks
// as every other access but does NOT apply the binary/size guards (undo compares
// against a recorded hash, not model-facing content). Returns an error if the file
// is absent.
func (w *Workspace) ReadFileForUndo(p string) ([]byte, error) {
	return w.readAll(p)
}
