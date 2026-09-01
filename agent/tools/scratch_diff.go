package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// scratchPromoteTempPrefix reserves the basename namespace promotion uses
	// for its same-directory staging directories. Subtrees carrying it are never
	// captured as artifacts, so promotion can never collide with a
	// command-created file of the same name.
	scratchPromoteTempPrefix = ".golem-scratch-promote-"

	// scratchPreviewCap bounds the complete escaped additions preview a
	// promotable create must fit (S3). Larger text is report-only: an
	// approval prompt must show everything it approves.
	scratchPreviewCap = 64 << 10
)

// scratchChangeKind classifies one observed divergence.
type scratchChangeKind string

const (
	scratchChangeCreate scratchChangeKind = "create"
	scratchChangeUpdate scratchChangeKind = "update"
	scratchChangeDelete scratchChangeKind = "delete"
	scratchChangeOther  scratchChangeKind = "other" // type/mode/symlink/special change
)

// scratchParentEvidence records the canonical identity of a created file's
// parent directory at snapshot time. Promotion re-verifies the live parent
// against it before writing.
type scratchParentEvidence struct {
	path    string // slash-relative parent ("." for the workspace root)
	dev     uint64
	ino     uint64
	mode    fs.FileMode // full mode including type bits
	existed bool
}

// scratchChange is one observed divergence between the pristine reference
// tree and the work tree after the command ran. Bytes are retained only for
// promotable creates; everything else keeps bounded metadata and hashes.
type scratchChange struct {
	path       string // slash-separated, workspace-relative
	kind       scratchChangeKind
	mode       fs.FileMode // full mode of the work entry (zero for deletes)
	size       int64
	nlink      uint64
	hash       string // ContentHash of work content where read
	data       []byte // promotable creates only
	preview    string // complete escaped additions render, promotable creates only
	promotable bool
	reason     string // why not promotable, when !promotable
	parent     scratchParentEvidence
}

// scratchOutcome is the immutable capture of one scratch session. Truncated
// means the diff is incomplete (limit, drift, or capture error) and nothing
// in it is promotable.
type scratchOutcome struct {
	id         string
	capturedAt time.Time
	changes    []scratchChange
	truncated  bool
	captureErr string
	cleanupErr string
}

// diffTrees computes the exact bounded changeset between the reference and
// work trees. Content equality never depends on timestamps: every same-size
// regular pair is stream-compared. Metadata may prove difference, never
// equality.
func diffTrees(ctx context.Context, reference, workspace string, canonical snapshotManifest, cfg ScratchConfig) (scratchOutcome, error) {
	return diffTreesWithHook(ctx, reference, workspace, canonical, cfg, nil)
}

// diffTreesWithHook is diffTrees with a deterministic seam between
// classification and the final drift rewalk, so tests can stand in for a
// surviving command descendant mutating the private trees.
func diffTreesWithHook(ctx context.Context, reference, workspace string, canonical snapshotManifest, cfg ScratchConfig, betweenPasses func()) (scratchOutcome, error) {
	refMan, err := walkSource(ctx, reference, cfg, nil)
	if err != nil {
		return scratchOutcome{}, fmt.Errorf("tools: walk reference tree: %w", err)
	}
	workMan, err := walkSource(ctx, workspace, cfg, nil)
	if err != nil {
		return scratchOutcome{}, fmt.Errorf("tools: walk work tree: %w", err)
	}
	refRoot, err := os.OpenRoot(reference)
	if err != nil {
		return scratchOutcome{}, fmt.Errorf("tools: open reference tree: %w", err)
	}
	defer func() { _ = refRoot.Close() }()
	workRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return scratchOutcome{}, fmt.Errorf("tools: open work tree: %w", err)
	}
	defer func() { _ = workRoot.Close() }()
	refEntries := diffIndex(refMan)
	workEntries := diffIndex(workMan)
	canonEntries := map[string]snapshotEntry{}
	for _, e := range canonical.entries {
		canonEntries[e.path] = e
	}

	union := make([]string, 0, len(refEntries)+8)
	for p := range refEntries {
		union = append(union, p)
	}
	for p := range workEntries {
		if _, ok := refEntries[p]; !ok {
			union = append(union, p)
		}
	}
	sort.Strings(union)

	var out scratchOutcome
	var retained int64
	for _, p := range union {
		if err := ctx.Err(); err != nil {
			return scratchOutcome{}, err
		}
		ref, inRef := refEntries[p]
		work, inWork := workEntries[p]
		change, changed, err := classifyChange(ctx, refRoot, workRoot, p, ref, inRef, work, inWork)
		if err != nil {
			return scratchOutcome{}, err
		}
		if !changed {
			continue
		}
		if len(out.changes) >= cfg.MaxChangedFiles {
			out.truncated = true
			out.captureErr = fmt.Sprintf("changeset exceeds %d entries", cfg.MaxChangedFiles)
			break
		}
		if change.kind == scratchChangeCreate && change.promotable {
			gateCreate(ctx, workRoot, &change, work, canonEntries, cfg, &retained)
		}
		out.changes = append(out.changes, change)
	}

	if betweenPasses != nil {
		betweenPasses()
	}

	// Final drift rewalk of both private trees: a surviving descendant of
	// the command mutating either tree invalidates the entire outcome.
	if !out.truncated {
		refMan2, err := walkSource(ctx, reference, cfg, nil)
		if err != nil {
			return scratchOutcome{}, err
		}
		workMan2, err := walkSource(ctx, workspace, cfg, nil)
		if err != nil {
			return scratchOutcome{}, err
		}
		if !manifestsEqual(refMan, refMan2) || !manifestsEqual(workMan, workMan2) {
			out.truncated = true
			out.captureErr = "private trees changed during capture"
		}
	}
	if out.truncated {
		for i := range out.changes {
			out.changes[i].promotable = false
			out.changes[i].data = nil
			out.changes[i].preview = ""
			if out.changes[i].reason == "" {
				out.changes[i].reason = "outcome truncated; nothing is promotable"
			}
		}
	}
	out.capturedAt = time.Now()
	return out, nil
}

// diffIndex maps manifest entries by path, excluding the workspace root
// itself, the top-level .agent subtree, and reserved promotion-temp names —
// none of which are ever captured as artifacts.
func diffIndex(man snapshotManifest) map[string]snapshotEntry {
	m := make(map[string]snapshotEntry, len(man.entries))
	for _, e := range man.entries {
		if e.path == "." {
			continue
		}
		if e.path == ".agent" || strings.HasPrefix(e.path, ".agent/") {
			continue
		}
		if isScratchPromoteTempPath(e.path) {
			continue
		}
		m[e.path] = e
	}
	return m
}

func isScratchPromoteTempPath(p string) bool {
	for _, component := range strings.Split(p, "/") {
		if strings.HasPrefix(component, scratchPromoteTempPrefix) {
			return true
		}
	}
	return false
}

// classifyChange decides whether one path changed and what kind of change it
// is. A create starts out promotable=true and is then gated by gateCreate;
// every other kind is report-only by construction (D5).
func classifyChange(ctx context.Context, reference, workspace *os.Root, p string, ref snapshotEntry, inRef bool, work snapshotEntry, inWork bool) (scratchChange, bool, error) {
	switch {
	case inWork && !inRef:
		c := scratchChange{
			path:  p,
			kind:  scratchChangeCreate,
			mode:  work.typ | work.mode,
			size:  work.size,
			nlink: work.nlink,
		}
		if !work.typ.IsRegular() {
			c.reason = "only regular-file creates are promotable"
			return c, true, nil
		}
		c.promotable = true
		return c, true, nil
	case inRef && !inWork:
		return scratchChange{
			path:   p,
			kind:   scratchChangeDelete,
			size:   ref.size,
			reason: "deletes are report-only in MVP",
		}, true, nil
	default:
		return classifyCommon(ctx, reference, workspace, p, ref, work)
	}
}

// classifyCommon compares a path present in both trees.
func classifyCommon(ctx context.Context, reference, workspace *os.Root, p string, ref, work snapshotEntry) (scratchChange, bool, error) {
	base := scratchChange{
		path:  p,
		mode:  work.typ | work.mode,
		size:  work.size,
		nlink: work.nlink,
	}
	if ref.typ != work.typ {
		base.kind = scratchChangeOther
		base.reason = "entry type changed; report-only"
		return base, true, nil
	}
	switch {
	case work.typ&fs.ModeSymlink != 0:
		if ref.linkTarget != work.linkTarget {
			base.kind = scratchChangeOther
			base.reason = "symlink target changed; report-only"
			return base, true, nil
		}
		return scratchChange{}, false, nil
	case work.typ.IsDir():
		if ref.fullMode != work.fullMode {
			base.kind = scratchChangeOther
			base.reason = "directory mode changed; report-only"
			return base, true, nil
		}
		return scratchChange{}, false, nil
	case work.typ.IsRegular():
		equal := ref.size == work.size
		var workHash string
		if equal {
			// Same size proves nothing: stream-compare the pair. Metadata
			// may prove difference, never equality.
			var err error
			equal, workHash, err = compareFilePair(ctx, reference, ref, workspace, work)
			if err != nil {
				return scratchChange{}, false, err
			}
		}
		if equal {
			if ref.fullMode != work.fullMode {
				base.kind = scratchChangeOther
				base.reason = "mode-only change; report-only"
				return base, true, nil
			}
			return scratchChange{}, false, nil
		}
		if workHash == "" {
			if h, herr := hashFile(ctx, workspace, work); herr == nil {
				workHash = h
			}
		}
		base.kind = scratchChangeUpdate
		base.hash = workHash
		base.reason = "updates are report-only in MVP"
		return base, true, nil
	default:
		base.kind = scratchChangeOther
		base.reason = "special file changed; report-only"
		return base, true, nil
	}
}

// gateCreate applies the D5 promotable matrix to a regular-file create and
// retains bounded content for those that pass every gate. Retained cost
// counts data plus the rendered preview against MaxTotalBytes.
func gateCreate(ctx context.Context, workspace *os.Root, c *scratchChange, work snapshotEntry, canonical map[string]snapshotEntry, cfg ScratchConfig, retained *int64) {
	demote := func(reason string) {
		c.promotable = false
		c.data = nil
		c.preview = ""
		c.reason = sanitizeScratchText(reason)
	}
	if c.nlink > 1 {
		demote("command-created hard-link topology is not promotable")
		return
	}
	parentPath := path.Dir(c.path)
	pe, ok := canonical[parentPath]
	if parentPath != "." && (!ok || !pe.typ.IsDir()) {
		demote("parent directory did not exist at snapshot time")
		return
	}
	c.parent = scratchParentEvidence{path: parentPath, existed: true}
	if parentPath == "." {
		if rootEntry, ok := canonical["."]; ok {
			c.parent.dev, c.parent.ino, c.parent.mode = rootEntry.dev, rootEntry.ino, rootEntry.fullMode
		}
	} else {
		c.parent.dev, c.parent.ino, c.parent.mode = pe.dev, pe.ino, pe.fullMode
	}
	if c.size > cfg.MaxFileBytes {
		if hash, err := hashFile(ctx, workspace, work); err == nil {
			c.hash = hash
		}
		demote(fmt.Sprintf("create exceeds the %d-byte retention cap", cfg.MaxFileBytes))
		return
	}
	if c.size > cfg.MaxTotalBytes-*retained {
		demote("retention budget exhausted; create is report-only")
		return
	}
	data, err := readStable(ctx, workspace, work)
	if err != nil {
		demote(fmt.Sprintf("create could not be read stably: %v", err))
		return
	}
	c.hash = ContentHash(data)
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		demote("binary or NUL-containing content is not approval-grade")
		return
	}
	preview := renderAdditionsPreview(data)
	if len(preview) > scratchPreviewCap {
		demote(fmt.Sprintf("complete escaped preview exceeds %d bytes", scratchPreviewCap))
		return
	}
	cost := int64(len(data)) + int64(len(preview))
	if *retained+cost > cfg.MaxTotalBytes {
		demote("retention budget exhausted; create is report-only")
		return
	}
	*retained += cost
	c.data = data
	c.preview = preview
}

// readStable reads a classified private-tree file through one rooted handle,
// refusing identity or metadata drift before or after the bounded read.
func readStable(ctx context.Context, root *os.Root, expected snapshotEntry) ([]byte, error) {
	f, err := openManifestRegular(root, expected)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data := make([]byte, 0, int(min(expected.size, 64<<10)))
	buf := make([]byte, 64<<10)
	remaining := expected.size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		want := int64(len(buf))
		if remaining < want {
			want = remaining
		}
		n, readErr := f.Read(buf[:want])
		if n > 0 {
			data = append(data, buf[:n]...)
			remaining -= int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("short read")
			}
			return nil, readErr
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var extra [1]byte
	n, readErr := f.Read(extra[:])
	if n != 0 {
		return nil, fmt.Errorf("grown read")
	}
	if readErr == nil {
		return nil, io.ErrNoProgress
	}
	if !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if err := checkManifestRegular(f, expected); err != nil {
		return nil, err
	}
	return data, nil
}

// hashFile streams one classified file into ContentHash form.
func hashFile(ctx context.Context, root *os.Root, expected snapshotEntry) (string, error) {
	f, err := openManifestRegular(root, expected)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		_, _ = h.Write(buf[:n])
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if err := checkManifestRegular(f, expected); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// compareFilePair stream-compares two classified files and returns equality
// plus the second file's content hash.
func compareFilePair(ctx context.Context, aRoot *os.Root, a snapshotEntry, bRoot *os.Root, b snapshotEntry) (bool, string, error) {
	fa, err := openManifestRegular(aRoot, a)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = fa.Close() }()
	fb, err := openManifestRegular(bRoot, b)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = fb.Close() }()
	h := sha256.New()
	equal := true
	bufA := make([]byte, 64<<10)
	bufB := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return false, "", err
		}
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		_, _ = h.Write(bufB[:nb])
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			equal = false
		}
		doneA := errA == io.EOF || errA == io.ErrUnexpectedEOF
		doneB := errB == io.EOF || errB == io.ErrUnexpectedEOF
		if errA != nil && !doneA {
			return false, "", errA
		}
		if errB != nil && !doneB {
			return false, "", errB
		}
		if doneA || doneB {
			if !doneA || !doneB {
				equal = false
			}
			break
		}
	}
	if err := checkManifestRegular(fa, a); err != nil {
		return false, "", err
	}
	if err := checkManifestRegular(fb, b); err != nil {
		return false, "", err
	}
	return equal, hex.EncodeToString(h.Sum(nil)), nil
}

// openManifestRegular opens one classified private-tree entry through an
// os.Root handle, so symlink swaps cannot escape the tree. O_NONBLOCK keeps a
// regular-to-FIFO swap from hanging before the opened identity can be checked.
func openManifestRegular(root *os.Root, expected snapshotEntry) (*os.File, error) {
	f, err := root.OpenFile(filepath.FromSlash(expected.path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := checkManifestRegular(f, expected); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkManifestRegular(f *os.File, expected snapshotEntry) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	actual := manifestEntry(expected.path, fi)
	if !actual.typ.IsRegular() || !snapshotEntriesEqual(expected, actual) {
		return fmt.Errorf("tools: capture path %q changed after classification: %w", expected.path, errSnapshotDrift)
	}
	return nil
}

// renderAdditionsPreview renders the complete all-additions diff for one
// created text file. Every line is escaped with strconv.QuoteToGraphic so
// control/format runes and exact newline boundaries are visible.
func renderAdditionsPreview(data []byte) string {
	var b strings.Builder
	rest := string(data)
	for len(rest) > 0 {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
			rest = rest[i+1:]
		} else {
			rest = ""
		}
		b.WriteString("+ ")
		b.WriteString(strconv.QuoteToGraphic(line))
		b.WriteByte('\n')
	}
	return b.String()
}
