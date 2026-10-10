//go:build darwin

package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// requireAPFS skips off APFS. It reads the filesystem type itself rather than
// asking storedNameFS, so a broken allowlist fails these tests instead of
// skipping them.
func requireAPFS(t *testing.T, dir string) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || unix.ByteSliceToString(fs.Fstypename[:]) != "apfs" {
		t.Skipf("not APFS: %s", unix.ByteSliceToString(fs.Fstypename[:]))
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// spellingFixture writes onDisk under a fresh APFS root and returns a
// workspace whose guard records paths and whose scans are counted. It skips
// when the filesystem does not resolve request to onDisk.
func spellingFixture(t *testing.T, onDisk, request string) (*Workspace, *[]string, *int) {
	t.Helper()
	root := t.TempDir()
	requireAPFS(t, root)
	p := filepath.Join(root, filepath.FromSlash(onDisk))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(request))); err != nil {
		t.Skipf("filesystem does not resolve %q to %q", request, onDisk)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	seen, scans := new([]string), new(int)
	ws.SetScopeGuard(func(rel string, _ bool) error { *seen = append(*seen, rel); return nil })
	ws.beforeNameScan = func() { *scans++ }
	return ws, seen, scans
}

func TestStoredNameFS(t *testing.T) {
	for name, want := range map[string]bool{"apfs": true, "hfs": false, "msdos": false, "exfat": false, "smbfs": false, "nfs": false, "": false} {
		var fs unix.Statfs_t
		copy(fs.Fstypename[:], name)
		if got := storedNameFS(&fs); got != want {
			t.Errorf("storedNameFS(%q) = %v, want %v", name, got, want)
		}
	}
}

// storedReply builds a getattrlist reply for storedName's request.
func storedReply(name string, dev int32, ino uint64, links *uint32) []byte {
	fixed := 24
	if links != nil {
		fixed = 28
	}
	b := make([]byte, fixed+len(name)+1)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], uint32(len(b)))
	ne.PutUint32(b[4:], uint32(fixed-4))
	ne.PutUint32(b[8:], uint32(len(name)+1))
	ne.PutUint32(b[12:], uint32(dev))
	ne.PutUint64(b[16:], ino)
	if links != nil {
		ne.PutUint32(b[24:], *links)
	}
	copy(b[fixed:], name)
	return b
}

func TestParseStoredEntry(t *testing.T) {
	one := uint32(1)
	if e, err := parseStoredEntry(storedReply("Secret", 7, 42, nil), false); err != nil || e != (storedEntry{name: "Secret", dev: 7, ino: 42}) {
		t.Fatalf("directory reply = %+v, %v", e, err)
	}
	if e, err := parseStoredEntry(storedReply("File", 7, 43, &one), true); err != nil || e != (storedEntry{name: "File", dev: 7, ino: 43, links: 1}) {
		t.Fatalf("file reply = %+v, %v", e, err)
	}
	mutate := func(f func([]byte) []byte) []byte { return f(storedReply("ok", 1, 1, nil)) }
	ne := binary.NativeEndian
	// Each row breaks exactly one bound and is otherwise well formed, so
	// removing that bound makes the row parse (D-M10).
	for name, buf := range map[string][]byte{
		"short buffer":      make([]byte, 10),
		"total past buffer": mutate(func(b []byte) []byte { ne.PutUint32(b[0:], uint32(len(b)+1)); return b }),
		// Offset 16 puts the name over the file ID ("AAAA") and runs into
		// "ok\0", so only the start-inside-fixed bound rejects it.
		"name inside fixed": func() []byte {
			b := storedReply("ok", 1, 0x4141414141414141, nil)
			ne.PutUint32(b[4:], 16)
			ne.PutUint32(b[8:], 7)
			return b
		}(),
		"negative offset":    mutate(func(b []byte) []byte { ne.PutUint32(b[4:], 0xfffffff0); return b }),
		"empty name":         storedReply("", 1, 1, nil),
		"name past total":    mutate(func(b []byte) []byte { ne.PutUint32(b[8:], 9); return b }),
		"missing terminator": mutate(func(b []byte) []byte { b[len(b)-1] = 'x'; return b }),
		"dot dot":            storedReply("..", 1, 1, nil),
		"slash":              storedReply("a/b", 1, 1, nil),
		"embedded nul":       storedReply("a\x00b", 1, 1, nil),
	} {
		if _, err := parseStoredEntry(buf, false); err == nil {
			t.Errorf("%s: parsed a malformed reply", name)
		}
	}
}

// TestCanonicalNameStoredNameSkipsScan pins C (#558): every alias the scan
// exists for resolves to the stored spelling without listing a parent.
func TestCanonicalNameStoredNameSkipsScan(t *testing.T) {
	for _, tc := range []struct{ name, onDisk, request, wantGuard string }{
		{"exact", "dir/target.txt", "dir/target.txt", "dir/target.txt"},
		{"case alias", "Secret/File", "secret/FILE", "Secret/File"},
		{"normalization alias", "café/x", "café/x", "café/x"},
		{"kelvin sign", "Keys/x", "Keys/x", "Keys/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, seen, scans := spellingFixture(t, tc.onDisk, tc.request)
			if _, err := NewReadFile(ws).Invoke(context.Background(), json.RawMessage(`{"path":`+jsonString(tc.request)+`}`)); err != nil {
				t.Fatal(err)
			}
			if len(*seen) != 1 || (*seen)[0] != tc.wantGuard {
				t.Fatalf("guard saw %q, want [%q]", *seen, tc.wantGuard)
			}
			if *scans != 0 {
				t.Fatalf("listing scans = %d, want 0 on APFS", *scans)
			}
		})
	}
}

// TestCanonicalPathForUndoUsesStoredName covers the mutation path.
func TestCanonicalPathForUndoUsesStoredName(t *testing.T) {
	ws, _, scans := spellingFixture(t, "Secret/File", "secret/FILE")
	got, err := ws.CanonicalPathForUndo("secret/FILE")
	if err != nil || got != "Secret/File" || *scans != 0 {
		t.Fatalf("CanonicalPathForUndo = %q, %v; scans %d (want Secret/File, 0)", got, err, *scans)
	}
}

// TestCanonicalNameHardlinkFallsBackToScan pins D2: a multiply-linked file is
// never named from metadata; the link count comes from the same reply.
func TestCanonicalNameHardlinkFallsBackToScan(t *testing.T) {
	ws, seen, scans := spellingFixture(t, "File", "File")
	if err := os.Link(filepath.Join(ws.root, "File"), filepath.Join(ws.root, "Other")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.readAll("File"); err != nil {
		t.Fatal(err)
	}
	if *scans != 1 || len(*seen) != 1 || (*seen)[0] != "File" {
		t.Fatalf("scans=%d guard=%q, want one scan and [File]", *scans, *seen)
	}
}

// TestCanonicalNameSymlinkComponentFallsBackToScan pins D4: only directories
// and regular files take the stored name; a symlink component is listed and
// then denied as before.
func TestCanonicalNameSymlinkComponentFallsBackToScan(t *testing.T) {
	ws, _, scans := spellingFixture(t, "real/x", "real/x")
	if err := os.Symlink("real", filepath.Join(ws.root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.readAll("link/x"); !errors.Is(err, errSymlink) {
		t.Fatalf("read through symlink err = %v, want errSymlink", err)
	}
	if *scans != 1 {
		t.Fatalf("listing scans = %d, want 1 (symlink falls back)", *scans)
	}
}

// TestProvenSpellingFallbacks pins D3, D4 and D7 on the function itself.
func TestProvenSpellingFallbacks(t *testing.T) {
	root := t.TempDir()
	requireAPFS(t, root)
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()
	stat := func(name string) unix.Stat_t {
		var st unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
		return st
	}
	other := stat("b")
	if _, ok, err := provenSpelling(parent, "a", &other); ok || !errors.Is(err, errFileChanged) {
		t.Fatalf("another object's identity: ok=%v err=%v, want errFileChanged", ok, err)
	}
	foreign := stat("a")
	foreign.Dev++
	if _, ok, err := provenSpelling(parent, "a", &foreign); ok || err != nil {
		t.Fatalf("entry on another device: ok=%v err=%v, want fallback", ok, err)
	}
	if _, ok, err := provenSpelling(parent, "missing", &other); ok || err != nil {
		t.Fatalf("failed query: ok=%v err=%v, want fallback", ok, err)
	}
	// The own-identity row needs a case-insensitive volume to look "A" up.
	if _, err := os.Stat(filepath.Join(root, "A")); err == nil {
		own := stat("a")
		if name, ok, err := provenSpelling(parent, "A", &own); !ok || err != nil || name != "a" {
			t.Fatalf("own identity: %q ok=%v err=%v, want a", name, ok, err)
		}
	}
}

// TestCanonicalNameForeignVolumeScans covers the APFS allowlist on a real
// non-APFS volume, which CI does not have. Set GO_LLM_SPELLING_FOREIGN_FS_DIR
// to a directory on one (for example an attached ExFAT disk image) to run it.
func TestCanonicalNameForeignVolumeScans(t *testing.T) {
	base := os.Getenv("GO_LLM_SPELLING_FOREIGN_FS_DIR")
	if base == "" {
		t.Skip("GO_LLM_SPELLING_FOREIGN_FS_DIR not set")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(base, &fs); err != nil || unix.ByteSliceToString(fs.Fstypename[:]) == "apfs" {
		t.Fatalf("GO_LLM_SPELLING_FOREIGN_FS_DIR must be on a non-APFS volume (%s): %v", unix.ByteSliceToString(fs.Fstypename[:]), err)
	}
	root, err := os.MkdirTemp(base, "spelling-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, "Secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Secret", "File"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	scans := 0
	ws.beforeNameScan = func() { scans++ }
	if _, err := ws.readAll("Secret/File"); err != nil {
		t.Fatal(err)
	}
	if scans != 2 {
		t.Fatalf("listing scans = %d, want 2 (every component off APFS is listed)", scans)
	}
}
