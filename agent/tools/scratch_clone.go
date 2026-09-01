package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
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

type scratchFileIdentity struct {
	dev uint64
	ino uint64
}

// snapshotCanonical clones src into dst (which must not exist) with platform
// CoW where possible, verifies the source did not observably change during
// the pass, and returns the source manifest. One whole-pass retry is allowed
// on detected drift; a second drift fails closed. The session caller owns
// final-failure cleanup so setup cannot spend two independent cleanup budgets.
func snapshotCanonical(ctx context.Context, src, dst string, cfg ScratchConfig, clone func(*os.File, string) error) (snapshotManifest, error) {
	man, err := snapshotOnce(ctx, src, dst, cfg, clone)
	if errors.Is(err, errSnapshotDrift) {
		if rmErr := guardedRemoveAll(ctx, dst, nil); rmErr != nil {
			return snapshotManifest{}, fmt.Errorf("tools: remove drifted snapshot: %w", rmErr)
		}
		man, err = snapshotOnce(ctx, src, dst, cfg, clone)
	}
	return man, err
}

// snapshotOnce runs one clone pass plus its post-pass manifest verification.
func snapshotOnce(ctx context.Context, src, dst string, cfg ScratchConfig, clone func(*os.File, string) error) (snapshotManifest, error) {
	type deferredSymlink struct {
		rel        string
		fi         fs.FileInfo
		linkTarget string
	}
	var symlinks []deferredSymlink
	man, err := walkSource(ctx, src, cfg, func(rel string, fi fs.FileInfo, linkTarget string) error {
		if fi.Mode()&fs.ModeSymlink != 0 {
			symlinks = append(symlinks, deferredSymlink{rel: rel, fi: fi, linkTarget: linkTarget})
			return nil
		}
		return cloneEntry(ctx, src, dst, rel, fi, "", clone, nil)
	})
	if err != nil {
		return snapshotManifest{}, err
	}
	canonicalEntries := make(map[scratchFileIdentity]string)
	for _, e := range man.entries {
		if err := ctx.Err(); err != nil {
			return snapshotManifest{}, err
		}
		if (e.typ.IsRegular() || e.typ.IsDir()) && (e.dev != 0 || e.ino != 0) {
			canonicalEntries[scratchFileIdentity{dev: e.dev, ino: e.ino}] = e.path
		}
	}
	for _, link := range symlinks {
		if err := ctx.Err(); err != nil {
			return snapshotManifest{}, err
		}
		if err := cloneEntry(ctx, src, dst, link.rel, link.fi, link.linkTarget, clone, canonicalEntries); err != nil {
			return snapshotManifest{}, err
		}
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
		if err := ctx.Err(); err != nil {
			return snapshotManifest{}, err
		}
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
// and byte budgets and ctx while reading directories in bounded batches,
// excluding every entry named .git at every depth, and recording a manifest.
// When visit is non-nil it is called per entry to materialize the clone.
func walkSource(ctx context.Context, src string, cfg ScratchConfig, visit func(rel string, fi fs.FileInfo, linkTarget string) error) (snapshotManifest, error) {
	root, err := os.OpenRoot(src)
	if err != nil {
		return snapshotManifest{}, fmt.Errorf("tools: open snapshot source: %w", err)
	}
	defer func() { _ = root.Close() }()

	var man snapshotManifest
	entries := 1 // root; child names are reserved before descending
	var bytes int64
	var walk func(string, fs.FileInfo) error
	walk = func(rel string, fi fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			size := fi.Size()
			if size < 0 || size > cfg.MaxWorkspaceBytes-bytes {
				return fmt.Errorf("tools: snapshot exceeds %d workspace bytes", cfg.MaxWorkspaceBytes)
			}
			bytes += size
		}
		entry := manifestEntry(rel, fi)
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := root.Readlink(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			entry.linkTarget = target
		}
		man.entries = append(man.entries, entry)
		if visit != nil {
			if err := visit(rel, fi, entry.linkTarget); err != nil {
				return err
			}
		}
		if !fi.IsDir() {
			return nil
		}
		names, err := readScratchDirNames(ctx, root, rel, fi, cfg.MaxWorkspaceFiles-entries, cfg.MaxWorkspaceFiles)
		if err != nil {
			return err
		}
		entries += len(names)
		for _, name := range names {
			childRel := path.Join(rel, name)
			child, err := root.Lstat(filepath.FromSlash(childRel))
			if err != nil {
				return err
			}
			if err := walk(childRel, child); err != nil {
				return err
			}
		}
		return nil
	}
	fi, err := root.Lstat(".")
	if err != nil {
		return snapshotManifest{}, err
	}
	if entries > cfg.MaxWorkspaceFiles {
		return snapshotManifest{}, fmt.Errorf("tools: snapshot exceeds %d workspace entries", cfg.MaxWorkspaceFiles)
	}
	if err := walk(".", fi); err != nil {
		return snapshotManifest{}, err
	}
	return man, nil
}

func readScratchDirNames(ctx context.Context, root *os.Root, rel string, expected fs.FileInfo, remaining, limit int) ([]string, error) {
	dir, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	opened, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(expected, opened) {
		return nil, errSnapshotDrift
	}
	var names []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := dir.Readdirnames(128)
		for _, name := range batch {
			if name == ".git" {
				continue
			}
			names = append(names, name)
			if len(names) > remaining {
				return nil, fmt.Errorf("tools: snapshot exceeds %d workspace entries", limit)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}

// manifestEntry records one source entry. Identity fields degrade to zero on
// platforms without them; the comparison is then correspondingly weaker.
func manifestEntry(rel string, fi fs.FileInfo) snapshotEntry {
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
	return e
}

func manifestsEqual(a, b snapshotManifest) bool {
	if len(a.entries) != len(b.entries) {
		return false
	}
	for i := range a.entries {
		if !snapshotEntriesEqual(a.entries[i], b.entries[i]) {
			return false
		}
	}
	return true
}

func snapshotEntriesEqual(x, y snapshotEntry) bool {
	return x.path == y.path && x.typ == y.typ && x.fullMode == y.fullMode && x.size == y.size &&
		x.dev == y.dev && x.ino == y.ino && x.nlink == y.nlink &&
		x.modTime.Equal(y.modTime) && x.changeTime.Equal(y.changeTime) &&
		x.linkTarget == y.linkTarget
}

// cloneEntry materializes one source entry inside dst. Directories are
// created writable and receive their real mode post-order; regular files are
// cloned from a stable open handle with identity checks before and after;
// symlinks follow the classification policy; sockets, FIFOs, and devices are
// not workspace content and are skipped.
func cloneEntry(ctx context.Context, src, dst, rel string, fi fs.FileInfo, linkTarget string, clone func(*os.File, string) error, canonicalEntries map[scratchFileIdentity]string) error {
	target := filepath.Join(dst, filepath.FromSlash(rel))
	switch {
	case fi.IsDir():
		if rel == "." {
			target = dst
		}
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		return os.Chmod(target, 0o700)
	case fi.Mode()&fs.ModeSymlink != 0:
		rewritten, err := rewriteSymlinkTarget(ctx, src, rel, linkTarget, canonicalEntries)
		if err != nil {
			return err
		}
		return os.Symlink(rewritten, target)
	case fi.Mode().IsRegular():
		return cloneRegular(ctx, src, rel, fi, target, clone)
	default:
		return nil // sockets, FIFOs, devices: not workspace content
	}
}

// cloneRegular clones one regular file from an opened handle, so a path swap
// between the walk and the copy cannot substitute content, then applies the
// permission bits explicitly (special bits stripped, umask-proof) and
// re-checks the source for drift.
func cloneRegular(ctx context.Context, src, rel string, walkInfo fs.FileInfo, target string, clone func(*os.File, string) error) error {
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(walkInfo, opened) {
		return fmt.Errorf("tools: snapshot source %q replaced during traversal: %w", rel, errSnapshotDrift)
	}
	if err := clone(f, target); err != nil {
		if !isCloneFallbackError(err) {
			return fmt.Errorf("tools: clone %q: %w", rel, err)
		}
		if err := copyFromHandle(ctx, f, target, walkInfo.Size()); err != nil {
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
func copyFromHandle(ctx context.Context, f *os.File, target string, expectedSize int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedSize < 0 {
		return errSnapshotDrift
	}
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
	fail := func(err error) error {
		_ = out.Close()
		_ = os.Remove(target)
		return err
	}
	buf := make([]byte, 1<<20)
	remaining := expectedSize
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		chunk := int64(len(buf))
		if remaining < chunk {
			chunk = remaining
		}
		n, readErr := f.Read(buf[:chunk])
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if n > 0 {
			written, writeErr := out.Write(buf[:n])
			if writeErr != nil || written != n {
				if writeErr != nil {
					return fail(writeErr)
				}
				return fail(io.ErrShortWrite)
			}
			remaining -= int64(n)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return fail(io.ErrUnexpectedEOF)
			}
			return fail(readErr)
		}
		if n == 0 {
			return fail(io.ErrNoProgress)
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	var extra [1]byte
	n, readErr := f.Read(extra[:])
	if n != 0 {
		return fail(errSnapshotDrift)
	}
	if readErr == nil {
		return fail(io.ErrNoProgress)
	}
	if !errors.Is(readErr, io.EOF) {
		return fail(readErr)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
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
func rewriteSymlinkTarget(ctx context.Context, srcRoot, rel, target string, canonicalEntries map[scratchFileIdentity]string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	linkDir := filepath.Dir(filepath.Join(srcRoot, filepath.FromSlash(rel)))
	var absTarget string
	if filepath.IsAbs(target) {
		absTarget = filepath.Clean(target)
	} else {
		absTarget = filepath.Clean(filepath.Join(linkDir, target))
	}
	inside := func(root, p string) bool {
		return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
	}
	rewriteInternal := func(p string) (string, error) {
		// Always normalize to the relative form anchored at the link's own
		// directory — never verbatim. A verbatim relative target that
		// lexically leaves and re-enters the root ("../<rootname>/x") would
		// classify differently on the second clone pass (its prefix no
		// longer matches the new root), dangling in the work tree and
		// producing a phantom diff entry on every capture. Rel() is a fixed
		// point, so both passes agree.
		relTarget, err := filepath.Rel(linkDir, p)
		if err != nil {
			return "", fmt.Errorf("tools: rewrite internal symlink %q: %w", rel, err)
		}
		return relTarget, nil
	}
	if inside(srcRoot, absTarget) {
		return rewriteInternal(absTarget)
	}
	resolved, complete, err := resolveSymlinkTarget(ctx, absTarget)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", fmt.Errorf("tools: resolve external symlink target %q: %w", rel, err)
	}
	if inside(srcRoot, resolved) {
		return rewriteInternal(resolved)
	}
	if canonicalRel, internal, err := relativeToCanonicalIdentity(ctx, resolved, canonicalEntries); err != nil {
		return "", fmt.Errorf("tools: classify symlink target %q: %w", rel, err)
	} else if internal {
		return rewriteInternal(filepath.Join(srcRoot, filepath.FromSlash(canonicalRel)))
	}
	if complete {
		fi, err := os.Stat(resolved)
		if err != nil {
			return "", fmt.Errorf("tools: stat resolved symlink target %q: %w", rel, err)
		}
		_, _, nlink, _ := statIdentity(fi)
		if fi.Mode().IsRegular() {
			if !scratchIdentitySupported || nlink > 1 {
				return "", fmt.Errorf("tools: external regular symlink target %q has no provably unrelated identity", rel)
			}
		} else {
			return "", fmt.Errorf("tools: external symlink target %q is not a regular file", rel)
		}
		return resolved, nil
	}
	if !scratchIdentitySupported {
		return "", fmt.Errorf("tools: incomplete external symlink target %q cannot be proven outside the workspace", rel)
	}
	return resolved, nil
}

// relativeToCanonicalIdentity recognizes aliases of any recorded canonical
// file or directory, including a missing suffix below an aliased directory.
func relativeToCanonicalIdentity(ctx context.Context, target string, canonicalEntries map[scratchFileIdentity]string) (string, bool, error) {
	current := filepath.Clean(target)
	var suffix []string
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		fi, err := os.Stat(current)
		if err == nil {
			dev, ino, _, _ := statIdentity(fi)
			if rel, ok := canonicalEntries[scratchFileIdentity{dev: dev, ino: ino}]; ok && (dev != 0 || ino != 0) {
				for i := len(suffix) - 1; i >= 0; i-- {
					rel = path.Join(rel, suffix[i])
				}
				return rel, true, nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// resolveSymlinkTarget resolves every existing symlink component while
// preserving a missing suffix. filepath.EvalSymlinks alone cannot resolve a
// dangling link whose target itself passes through a workspace alias.
func resolveSymlinkTarget(ctx context.Context, target string) (string, bool, error) {
	const maxDanglingLinks = 255
	current := filepath.Clean(target)
	complete := true
	var suffix []string
	for links := 0; ; {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		resolved, err := filepath.EvalSymlinks(current)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, complete, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		complete = false
		fi, statErr := os.Lstat(current)
		if statErr == nil && fi.Mode()&fs.ModeSymlink != 0 {
			if links >= maxDanglingLinks {
				return "", false, fmt.Errorf("too many dangling symlinks")
			}
			link, err := os.Readlink(current)
			if err != nil {
				return "", false, err
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(current), link)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				link = filepath.Join(link, suffix[i])
			}
			current = filepath.Clean(link)
			suffix = suffix[:0]
			links++
			continue
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return "", false, statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// relativeToRootIdentity recognizes filesystem aliases (for example Darwin's
// /System/Volumes/Data firmlink) by walking ancestors and comparing identity,
// then returns the target suffix beneath root.
func relativeToRootIdentity(ctx context.Context, root, target string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", false, err
	}
	current := filepath.Clean(target)
	var suffix []string
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		fi, err := os.Stat(current)
		if err == nil {
			if os.SameFile(rootInfo, fi) {
				rel := "."
				for i := len(suffix) - 1; i >= 0; i-- {
					rel = filepath.Join(rel, suffix[i])
				}
				return rel, true, nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
