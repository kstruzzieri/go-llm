package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// errScratchCloneUnsupported is the sentinel a cloneFile implementation (or
// an injected test seam) returns when platform CoW is unavailable; it always
// permits the exact plain-copy fallback. Every other error falls back only
// when it is in the platform's enumerated unsupported/cross-device set —
// permission, I/O, corruption, and disk-full failures are fatal.
var errScratchCloneUnsupported = errors.New("tools: scratch clone unsupported on this platform")

// errSnapshotDrift reports that the canonical source tree observably changed
// while a snapshot pass ran. snapshotCanonical retries the whole pass once,
// then fails closed. The manifest comparison catches common races; it is
// explicitly not a point-in-time coherence proof.
var errSnapshotDrift = errors.New("tools: canonical tree changed during snapshot")

// snapshotEntry records one source entry's approval-relevant identity for
// drift detection and for the diff layer (link counts, times, identity).
type snapshotEntry struct {
	path string      // slash-separated, root-relative ("." for the root)
	typ  fs.FileMode // type bits only (0 for a regular file)
	mode fs.FileMode // permission bits as found on the source
	// fullMode is the complete source mode including special bits
	// (setuid/setgid/sticky). The clone applies mode (special bits
	// stripped, the snapshot contract), but drift detection and promotion's
	// parent pinning must compare the complete mode: a setgid parent is not
	// drift, and a special-bit flip must be visible.
	fullMode   fs.FileMode
	size       int64 // regular files only
	modTime    time.Time
	changeTime time.Time // zero where the platform offers none
	dev, ino   uint64    // zero where the platform offers none
	nlink      uint64
	linkTarget string // raw source target for symlinks
}

// snapshotManifest is the ordered record of one source pass.
type snapshotManifest struct {
	entries []snapshotEntry
}

// snapshotCanonical clones src into dst (which must not exist) with platform
// CoW where possible, verifies the source did not observably change during
// the pass, and returns the source manifest. One whole-pass retry is allowed
// on detected drift; a second drift fails closed. Failure removes dst.
func snapshotCanonical(ctx context.Context, src, dst string, cfg ScratchConfig, clone func(*os.File, string) error) (snapshotManifest, error) {
	man, err := snapshotOnce(ctx, src, dst, cfg, clone)
	if errors.Is(err, errSnapshotDrift) {
		if rmErr := forcedRemoveAll(dst); rmErr != nil {
			return snapshotManifest{}, fmt.Errorf("tools: remove drifted snapshot: %w", rmErr)
		}
		man, err = snapshotOnce(ctx, src, dst, cfg, clone)
	}
	if err != nil {
		_ = forcedRemoveAll(dst)
		return snapshotManifest{}, err
	}
	return man, nil
}

// snapshotOnce runs one clone pass plus its post-pass manifest verification.
func snapshotOnce(ctx context.Context, src, dst string, cfg ScratchConfig, clone func(*os.File, string) error) (snapshotManifest, error) {
	man, err := walkSource(ctx, src, cfg, func(rel string, fi fs.FileInfo) error {
		return cloneEntry(src, dst, rel, fi, clone)
	})
	if err != nil {
		return snapshotManifest{}, err
	}
	// Directory permissions are applied post-order (deepest first) so a
	// read-only source directory does not block populating its children,
	// and explicitly so the process umask cannot change the contract.
	dirs := make([]snapshotEntry, 0, 8)
	for _, e := range man.entries {
		if e.typ.IsDir() {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		if err := os.Chmod(filepath.Join(dst, filepath.FromSlash(d.path)), d.mode); err != nil {
			return snapshotManifest{}, fmt.Errorf("tools: apply snapshot directory mode: %w", err)
		}
	}
	// Post-pass verification: rewalk the source and require an identical
	// manifest (path set, identity, type, mode, size, available timestamps).
	verify, err := walkSource(ctx, src, cfg, nil)
	if err != nil {
		return snapshotManifest{}, err
	}
	if !manifestsEqual(man, verify) {
		return snapshotManifest{}, errSnapshotDrift
	}
	return man, nil
}

// walkSource traverses src without following symlinks, enforcing the entry
// and byte budgets and ctx on every entry, excluding every entry named .git
// at every depth, and recording a manifest. When visit is non-nil it is
// called per entry to materialize the clone.
func walkSource(ctx context.Context, src string, cfg ScratchConfig, visit func(rel string, fi fs.FileInfo) error) (snapshotManifest, error) {
	root, err := os.OpenRoot(src)
	if err != nil {
		return snapshotManifest{}, fmt.Errorf("tools: open snapshot source: %w", err)
	}
	defer func() { _ = root.Close() }()

	var man snapshotManifest
	var entries int
	var bytes int64
	walkErr := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Name() == ".git" && rel != "." {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > cfg.MaxWorkspaceFiles {
			return fmt.Errorf("tools: snapshot exceeds %d workspace entries", cfg.MaxWorkspaceFiles)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			bytes += fi.Size()
			if bytes > cfg.MaxWorkspaceBytes {
				return fmt.Errorf("tools: snapshot exceeds %d workspace bytes", cfg.MaxWorkspaceBytes)
			}
		}
		entry := manifestEntry(src, rel, fi)
		man.entries = append(man.entries, entry)
		if visit != nil {
			if err := visit(rel, fi); err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		return snapshotManifest{}, walkErr
	}
	return man, nil
}

// manifestEntry records one source entry. Identity fields degrade to zero on
// platforms without them; the comparison is then correspondingly weaker.
func manifestEntry(src, rel string, fi fs.FileInfo) snapshotEntry {
	dev, ino, nlink, ctime := statIdentity(fi)
	e := snapshotEntry{
		path:       rel,
		typ:        fi.Mode().Type(),
		mode:       fi.Mode().Perm(),
		fullMode:   fi.Mode(),
		modTime:    fi.ModTime(),
		changeTime: ctime,
		dev:        dev,
		ino:        ino,
		nlink:      nlink,
	}
	if fi.Mode().IsRegular() {
		e.size = fi.Size()
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if target, err := os.Readlink(filepath.Join(src, filepath.FromSlash(rel))); err == nil {
			e.linkTarget = target
		}
	}
	return e
}

func manifestsEqual(a, b snapshotManifest) bool {
	if len(a.entries) != len(b.entries) {
		return false
	}
	for i := range a.entries {
		x, y := a.entries[i], b.entries[i]
		if x.path != y.path || x.typ != y.typ || x.fullMode != y.fullMode || x.size != y.size ||
			x.dev != y.dev || x.ino != y.ino || x.nlink != y.nlink ||
			!x.modTime.Equal(y.modTime) || !x.changeTime.Equal(y.changeTime) ||
			x.linkTarget != y.linkTarget {
			return false
		}
	}
	return true
}

// cloneEntry materializes one source entry inside dst. Directories are
// created writable and receive their real mode post-order; regular files are
// cloned from a stable open handle with identity checks before and after;
// symlinks follow the classification policy; sockets, FIFOs, and devices are
// not workspace content and are skipped.
func cloneEntry(src, dst, rel string, fi fs.FileInfo, clone func(*os.File, string) error) error {
	target := filepath.Join(dst, filepath.FromSlash(rel))
	switch {
	case fi.IsDir():
		if rel == "." {
			return os.Mkdir(dst, 0o700)
		}
		return os.Mkdir(target, 0o700)
	case fi.Mode()&fs.ModeSymlink != 0:
		linkTarget, err := os.Readlink(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		rewritten, err := rewriteSymlinkTarget(src, rel, linkTarget)
		if err != nil {
			return err
		}
		return os.Symlink(rewritten, target)
	case fi.Mode().IsRegular():
		return cloneRegular(src, rel, fi, target, clone)
	default:
		return nil // sockets, FIFOs, devices: not workspace content
	}
}

// cloneRegular clones one regular file from an opened handle, so a path swap
// between the walk and the copy cannot substitute content, then applies the
// permission bits explicitly (special bits stripped, umask-proof) and
// re-checks the source for drift.
func cloneRegular(src, rel string, walkInfo fs.FileInfo, target string, clone func(*os.File, string) error) error {
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(walkInfo, opened) {
		return fmt.Errorf("tools: snapshot source %q replaced during traversal: %w", rel, errSnapshotDrift)
	}
	if err := clone(f, target); err != nil {
		if !isCloneFallbackError(err) {
			return fmt.Errorf("tools: clone %q: %w", rel, err)
		}
		if err := copyFromHandle(f, target); err != nil {
			return fmt.Errorf("tools: copy %q after clone fallback: %w", rel, err)
		}
	}
	runtime.KeepAlive(f)
	if err := os.Chmod(target, walkInfo.Mode().Perm()); err != nil {
		return err
	}
	// Re-stat the stable handle: content grown or truncated during the copy
	// is drift, caught here rather than only at the whole-pass verification.
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if after.Size() != walkInfo.Size() || !after.ModTime().Equal(walkInfo.ModTime()) {
		return fmt.Errorf("tools: snapshot source %q changed while cloning: %w", rel, errSnapshotDrift)
	}
	return nil
}

// isCloneFallbackError reports whether err permits the exact plain-copy
// fallback: the sentinel, or one of the platform's enumerated
// unsupported/cross-device errnos. Everything else is fatal.
func isCloneFallbackError(err error) bool {
	if errors.Is(err, errScratchCloneUnsupported) {
		return true
	}
	for _, e := range cloneFallbackErrnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// copyFromHandle streams the opened source into target. Any partial
// destination (from a failed clone attempt or a failed copy) is removed, and
// close errors are checked so a short write cannot pass silently.
func copyFromHandle(f *os.File, target string) error {
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, f); err != nil {
		_ = out.Close()
		_ = os.Remove(target)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}

// rewriteSymlinkTarget applies the snapshot symlink policy (D2 + amendment
// A1): targets that stay lexically inside the source root keep working
// inside the clone (relative verbatim; absolute rewritten relative), targets
// outside the root are absolutized so they cannot accidentally point into
// the scratch hierarchy (resolved when resolvable, lexical when dangling),
// and dangling links never fail the snapshot.
func rewriteSymlinkTarget(srcRoot, rel, target string) (string, error) {
	linkDir := filepath.Dir(filepath.Join(srcRoot, filepath.FromSlash(rel)))
	var absTarget string
	if filepath.IsAbs(target) {
		absTarget = filepath.Clean(target)
	} else {
		absTarget = filepath.Clean(filepath.Join(linkDir, target))
	}
	inside := absTarget == srcRoot || strings.HasPrefix(absTarget, srcRoot+string(filepath.Separator))
	if inside {
		// Always normalize to the relative form anchored at the link's own
		// directory — never verbatim. A verbatim relative target that
		// lexically leaves and re-enters the root ("../<rootname>/x") would
		// classify differently on the second clone pass (its prefix no
		// longer matches the new root), dangling in the work tree and
		// producing a phantom diff entry on every capture. Rel() is a fixed
		// point, so both passes agree.
		relTarget, err := filepath.Rel(linkDir, absTarget)
		if err != nil {
			return "", fmt.Errorf("tools: rewrite internal symlink %q: %w", rel, err)
		}
		return relTarget, nil
	}
	if resolved, err := filepath.EvalSymlinks(absTarget); err == nil {
		return resolved, nil
	}
	return absTarget, nil
}
