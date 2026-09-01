package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// scratchPromoteTempPrefix reserves the basename namespace promotion uses
	// for its same-directory temp files. Entries carrying it are never
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
		change, changed, err := classifyChange(ctx, reference, workspace, p, ref, inRef, work, inWork)
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
			gateCreate(ctx, workspace, &change, canonEntries, cfg, &retained)
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
		if strings.HasPrefix(path.Base(e.path), scratchPromoteTempPrefix) {
			continue
		}
		m[e.path] = e
	}
	return m
}

// classifyChange decides whether one path changed and what kind of change it
// is. A create starts out promotable=true and is then gated by gateCreate;
// every other kind is report-only by construction (D5).
func classifyChange(ctx context.Context, reference, workspace, p string, ref snapshotEntry, inRef bool, work snapshotEntry, inWork bool) (scratchChange, bool, error) {
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
func classifyCommon(ctx context.Context, reference, workspace, p string, ref, work snapshotEntry) (scratchChange, bool, error) {
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
			equal, workHash, err = compareFilePair(ctx,
				filepath.Join(reference, filepath.FromSlash(p)),
				filepath.Join(workspace, filepath.FromSlash(p)))
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
			if h, herr := hashFile(ctx, filepath.Join(workspace, filepath.FromSlash(p))); herr == nil {
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
func gateCreate(ctx context.Context, workspace string, c *scratchChange, canonical map[string]snapshotEntry, cfg ScratchConfig, retained *int64) {
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
		if hash, err := hashFile(ctx, filepath.Join(workspace, filepath.FromSlash(c.path))); err == nil {
			c.hash = hash
		}
		demote(fmt.Sprintf("create exceeds the %d-byte retention cap", cfg.MaxFileBytes))
		return
	}
	data, err := readStable(filepath.Join(workspace, filepath.FromSlash(c.path)), c.size)
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

// readStable reads a private-tree file through one handle with before/after
// stat checks, so content mutating mid-read is refused rather than retained.
func readStable(p string, wantSize int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != wantSize {
		return nil, fmt.Errorf("size changed before read")
	}
	data, err := io.ReadAll(io.LimitReader(f, wantSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != wantSize {
		return nil, fmt.Errorf("short or grown read")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, fmt.Errorf("content changed during read")
	}
	return data, nil
}

// hashFile streams one file into ContentHash form without retaining bytes.
func hashFile(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
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
	return hex.EncodeToString(h.Sum(nil)), nil
}

// compareFilePair stream-compares two files and returns equality plus the
// second file's content hash.
func compareFilePair(ctx context.Context, a, b string) (bool, string, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = fa.Close() }()
	fb, err := os.Open(b)
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
			// Drain the longer side into the hash so the reported hash is
			// always the complete work-side content.
			if !doneB {
				if _, err := io.Copy(h, fb); err != nil {
					return false, "", err
				}
			}
			break
		}
	}
	return equal, hex.EncodeToString(h.Sum(nil)), nil
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
