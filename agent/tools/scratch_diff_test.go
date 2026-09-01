package tools

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// diffFixture builds a canonical tree, snapshots it into reference and
// workspace trees, and returns everything a diff test needs.
type diffFixture struct {
	canonical string
	reference string
	workspace string
	manifest  snapshotManifest
	cfg       ScratchConfig
}

func newDiffFixture(t *testing.T, cfg ScratchConfig) *diffFixture {
	t.Helper()
	canon := t.TempDir()
	files := map[string]string{
		"keep.txt":          "keep",
		"update.txt":        "before",
		"delete.txt":        "gone soon",
		"same-size.txt":     "aaaa",
		"mode-only.txt":     "stable",
		"combo.txt":         "combo",
		"dir/child.txt":     "child",
		".agent/ledger.txt": "agent state",
	}
	for rel, data := range files {
		p := filepath.Join(canon, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("keep.txt", filepath.Join(canon, "link")); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(t.TempDir(), "reference")
	man, err := snapshotCanonical(context.Background(), canon, ref, cfg, cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "workspace")
	if _, err := snapshotCanonical(context.Background(), ref, work, cfg, cloneFile); err != nil {
		t.Fatal(err)
	}
	return &diffFixture{canonical: canon, reference: ref, workspace: work, manifest: man, cfg: cfg}
}

func (f *diffFixture) diff(t *testing.T) scratchOutcome {
	t.Helper()
	out, err := diffTrees(context.Background(), f.reference, f.workspace, f.manifest, f.cfg)
	if err != nil {
		t.Fatalf("diffTrees: %v", err)
	}
	return out
}

func changeByPath(t *testing.T, out scratchOutcome, path string) scratchChange {
	t.Helper()
	for _, c := range out.changes {
		if c.path == path {
			return c
		}
	}
	t.Fatalf("no change recorded for %q in %+v", path, out.changes)
	return scratchChange{}
}

func TestScratchDiffNoChanges(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	out := f.diff(t)
	if len(out.changes) != 0 || out.truncated {
		t.Fatalf("identical trees must produce an empty outcome, got %+v", out)
	}
}

func TestScratchDiffCreatePromotable(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	if err := os.WriteFile(filepath.Join(f.workspace, "dir/new.txt"), []byte("hello\nworld\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	out := f.diff(t)
	c := changeByPath(t, out, "dir/new.txt")
	if c.kind != scratchChangeCreate || !c.promotable {
		t.Fatalf("new text file must be a promotable create: %+v", c)
	}
	if !bytes.Equal(c.data, []byte("hello\nworld\n")) {
		t.Fatalf("retained create bytes wrong: %q", c.data)
	}
	if c.hash != ContentHash([]byte("hello\nworld\n")) {
		t.Fatalf("create hash mismatch: %s", c.hash)
	}
	if c.mode.Perm() != 0o640 {
		t.Fatalf("create mode = %v", c.mode)
	}
	if !strings.Contains(c.preview, `"hello\n"`) || !strings.Contains(c.preview, `"world\n"`) {
		t.Fatalf("preview must show escaped additions with newline boundaries:\n%s", c.preview)
	}
	// Parent evidence is taken from the canonical manifest, not the clones.
	var canonDir snapshotEntry
	for _, e := range f.manifest.entries {
		if e.path == "dir" {
			canonDir = e
		}
	}
	if canonDir.path == "" {
		t.Fatal("manifest missing dir entry")
	}
	if !c.parent.existed || c.parent.path != "dir" || c.parent.ino != canonDir.ino || c.parent.dev != canonDir.dev {
		t.Fatalf("parent evidence must bind the canonical dir identity: %+v vs %+v", c.parent, canonDir)
	}
}

func TestScratchDiffKindsAndRetention(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	ws := f.workspace
	if err := os.WriteFile(filepath.Join(ws, "update.txt"), []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws, "mode-only.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "combo.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws, "combo.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("update.txt", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	// Type change: replace a file with a directory.
	if err := os.Remove(filepath.Join(ws, "same-size.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "same-size.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	out := f.diff(t)
	cases := map[string]scratchChangeKind{
		"update.txt":    scratchChangeUpdate,
		"delete.txt":    scratchChangeDelete,
		"mode-only.txt": scratchChangeOther,
		"combo.txt":     scratchChangeUpdate,
		"link":          scratchChangeOther,
		"same-size.txt": scratchChangeOther,
	}
	for path, kind := range cases {
		c := changeByPath(t, out, path)
		if c.kind != kind {
			t.Fatalf("%s kind = %s, want %s", path, c.kind, kind)
		}
		if c.promotable {
			t.Fatalf("%s must never be promotable in MVP (create-only)", path)
		}
		if len(c.data) != 0 || c.preview != "" {
			t.Fatalf("%s must retain metadata only, got %d data bytes preview=%q", path, len(c.data), c.preview)
		}
	}
}

func TestScratchDiffMtimeBackdatedSameSizeUpdate(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	p := filepath.Join(f.workspace, "same-size.txt")
	refInfo, err := os.Lstat(filepath.Join(f.reference, "same-size.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bbbb"), 0o644); err != nil { // same size, new bytes
		t.Fatal(err)
	}
	if err := os.Chtimes(p, refInfo.ModTime(), refInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	out := f.diff(t)
	c := changeByPath(t, out, "same-size.txt")
	if c.kind != scratchChangeUpdate {
		t.Fatalf("same-size backdated-mtime content change missed: %+v", c)
	}
}

func TestScratchDiffCreateGates(t *testing.T) {
	cfg := cloneFixtureConfig()
	f := newDiffFixture(t, cfg)
	ws := f.workspace
	if err := os.WriteFile(filepath.Join(ws, "binary.bin"), []byte{0xff, 0xfe, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "nul.txt"), []byte("a\x00b"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), int(cfg.MaxFileBytes)+1)
	if err := os.WriteFile(filepath.Join(ws, "oversize.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	// Text that fits the retention cap but renders over the 64 KiB preview cap.
	wide := bytes.Repeat([]byte("y"), 100<<10)
	if err := os.WriteFile(filepath.Join(ws, "preview-oversize.txt"), wide, 0o644); err != nil {
		t.Fatal(err)
	}
	// Parent created by the command: not promotable.
	if err := os.MkdirAll(filepath.Join(ws, "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "newdir/in-new.txt"), []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Command-created hard-link pair: neither side promotable.
	if err := os.WriteFile(filepath.Join(ws, "linked-a.txt"), []byte("pair"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(ws, "linked-a.txt"), filepath.Join(ws, "linked-b.txt")); err != nil {
		t.Fatal(err)
	}

	out := f.diff(t)
	for _, path := range []string{
		"binary.bin", "nul.txt", "oversize.txt", "preview-oversize.txt",
		"newdir/in-new.txt", "linked-a.txt", "linked-b.txt",
	} {
		c := changeByPath(t, out, path)
		if c.kind != scratchChangeCreate {
			t.Fatalf("%s kind = %s, want create", path, c.kind)
		}
		if c.promotable {
			t.Fatalf("%s must not be promotable (reason: %s)", path, c.reason)
		}
		if len(c.data) != 0 {
			t.Fatalf("%s must not retain bytes, got %d", path, len(c.data))
		}
		if c.reason == "" {
			t.Fatalf("%s needs a reason", path)
		}
	}
	if c := changeByPath(t, out, "oversize.txt"); c.hash == "" || c.size != int64(len(big)) {
		t.Fatalf("oversize create must keep hash/size metadata: %+v", c)
	}
}

func TestScratchDiffDeterministicOrder(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	for _, name := range []string{"z.txt", "a.txt", "m.txt"} {
		if err := os.WriteFile(filepath.Join(f.workspace, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := f.diff(t)
	paths := make([]string, len(out.changes))
	for i, c := range out.changes {
		paths[i] = c.path
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("changes must be sorted by path: %v", paths)
	}
}

func TestScratchDiffChangedFilesTruncates(t *testing.T) {
	cfg := cloneFixtureConfig()
	cfg.MaxChangedFiles = 2
	f := newDiffFixture(t, cfg)
	for _, name := range []string{"c1.txt", "c2.txt", "c3.txt"} {
		if err := os.WriteFile(filepath.Join(f.workspace, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := f.diff(t)
	if !out.truncated {
		t.Fatal("exceeding MaxChangedFiles must truncate the outcome")
	}
	for _, c := range out.changes {
		if c.promotable {
			t.Fatalf("truncated outcome must leave nothing promotable: %+v", c)
		}
	}
}

func TestScratchDiffRetentionBudgetCountsDataAndPreview(t *testing.T) {
	cfg := cloneFixtureConfig()
	// Two identical creates. Each costs len(data)=100 plus its rendered
	// preview (~105 bytes). Budget 300 admits both under a data-only count
	// (200) but only one under the correct data+preview count (~410).
	cfg.MaxTotalBytes = 300
	f := newDiffFixture(t, cfg)
	payload := bytes.Repeat([]byte("x"), 100)
	for _, name := range []string{"r1.txt", "r2.txt"} {
		if err := os.WriteFile(filepath.Join(f.workspace, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := f.diff(t)
	if out.truncated {
		t.Fatal("retention exhaustion must not truncate the diff itself")
	}
	first := changeByPath(t, out, "r1.txt")
	second := changeByPath(t, out, "r2.txt")
	if !first.promotable || len(first.data) == 0 {
		t.Fatalf("first create should fit the budget: %+v", first)
	}
	if second.promotable || len(second.data) != 0 {
		t.Fatalf("second create must be dropped by the data+preview budget: %+v", second)
	}
	if !strings.Contains(second.reason, "retention") {
		t.Fatalf("second create needs a retention reason, got %q", second.reason)
	}
}

func TestScratchDiffCaptureDriftInvalidates(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	if err := os.WriteFile(filepath.Join(f.workspace, "new.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := diffTreesWithHook(context.Background(), f.reference, f.workspace, f.manifest, f.cfg, func() {
		// A surviving descendant of the command mutates the private tree
		// between classification and the final manifest rewalk.
		if err := os.WriteFile(filepath.Join(f.workspace, "late.txt"), []byte("late"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("capture drift is an outcome, not an error: %v", err)
	}
	if !out.truncated {
		t.Fatal("post-classification drift must truncate the outcome")
	}
	for _, c := range out.changes {
		if c.promotable {
			t.Fatalf("drifted outcome must leave nothing promotable: %+v", c)
		}
	}
}

func TestScratchDiffAgentAndReservedExcluded(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	if err := os.WriteFile(filepath.Join(f.workspace, ".agent/ledger.txt"), []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, scratchPromoteTempPrefix+"abc"), []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.workspace, scratchPromoteTempPrefix+"stage")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "artifact"), []byte("temp"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.diff(t)
	for _, c := range out.changes {
		if strings.HasPrefix(c.path, ".agent") || isScratchPromoteTempPath(c.path) {
			t.Fatalf("reserved path leaked into the changeset: %+v", c)
		}
	}
}

func TestScratchDiffCancellation(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := diffTrees(ctx, f.reference, f.workspace, f.manifest, f.cfg); err == nil {
		t.Fatal("cancelled capture must fail")
	}
}

func TestScratchDiffControlCharacterPathSurvives(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	name := "evil\nname.txt"
	if err := os.WriteFile(filepath.Join(f.workspace, name), []byte("x"), 0o644); err != nil {
		t.Skipf("filesystem rejects control characters: %v", err)
	}
	out := f.diff(t)
	c := changeByPath(t, out, name)
	if c.kind != scratchChangeCreate {
		t.Fatalf("control-character path must be classified: %+v", c)
	}
}

// --- review round ---

func TestScratchDiffSizeDifferingUpdateCarriesHash(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	next := []byte("after is longer")
	if err := os.WriteFile(filepath.Join(f.workspace, "update.txt"), next, 0o644); err != nil {
		t.Fatal(err)
	}
	out := f.diff(t)
	c := changeByPath(t, out, "update.txt")
	if c.hash != ContentHash(next) {
		t.Fatalf("size-differing update must carry the work-side hash, got %q", c.hash)
	}
}

func TestScratchDiffHostileReasonEscaped(t *testing.T) {
	f := newDiffFixture(t, cloneFixtureConfig())
	// A command-created unreadable file whose read failure would embed raw
	// path bytes in the reason; the stored reason must be control-safe.
	p := filepath.Join(f.workspace, "evil\x1b[31mname.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Skipf("filesystem rejects control characters: %v", err)
	}
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	out := f.diff(t)
	for _, c := range out.changes {
		if strings.ContainsRune(c.reason, 0x1b) || strings.ContainsRune(c.reason, '\n') {
			t.Fatalf("reason leaked control bytes: %q", c.reason)
		}
	}
}

func TestScratchDiffCompareRespectsContext(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("z"), 1<<20)
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), big, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	man, err := walkSource(context.Background(), dir, cloneFixtureConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]snapshotEntry{}
	for _, entry := range man.entries {
		entries[entry.path] = entry
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, _, err := compareFilePair(ctx, root, entries["a"], root, entries["b"]); err == nil {
		t.Fatal("cancelled context must stop the stream compare")
	}
	if _, err := hashFile(ctx, root, entries["a"]); err == nil {
		t.Fatal("cancelled context must stop the stream hash")
	}
}

func TestOpenManifestRegularRejectsSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := walkSource(context.Background(), dir, cloneFixtureConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry snapshotEntry
	for _, candidate := range man.entries {
		if candidate.path == "entry" {
			entry = candidate
		}
	}
	if entry.path == "" {
		t.Fatal("manifest missing entry")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if f, err := openManifestRegular(root, entry); err == nil {
		_ = f.Close()
		t.Fatal("capture opened a path swapped from a regular file to a symlink")
	}
}

type cancelAfterErrChecks struct {
	context.Context
	checks int
	after  int
}

func (c *cancelAfterErrChecks) Err() error {
	c.checks++
	if c.checks >= c.after {
		return context.Canceled
	}
	return nil
}

func TestReadStableChecksContextBetweenChunks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "large"), bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := walkSource(context.Background(), dir, cloneFixtureConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry snapshotEntry
	for _, candidate := range man.entries {
		if candidate.path == "large" {
			entry = candidate
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	ctx := &cancelAfterErrChecks{Context: context.Background(), after: 3}
	if _, err := readStable(ctx, root, entry); !errors.Is(err, context.Canceled) {
		t.Fatalf("chunked read error = %v, want context.Canceled", err)
	}
}
