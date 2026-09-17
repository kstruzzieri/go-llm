//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWorkspaceRootReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("FORBIDDEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := ws.readAll("secret.txt"); !errors.Is(err, errFileChanged) {
		t.Fatalf("replacement read: %q, %v", data, err)
	}
	if _, err := ws.openDir("."); !errors.Is(err, errFileChanged) {
		t.Fatalf("replacement directory: %v", err)
	}
}

func TestWorkspacePinnedDuringPolicy(t *testing.T) {
	for _, listing := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "list"}[listing], func(t *testing.T) {
			root := t.TempDir()
			original := filepath.Join(root, "public")
			replacement := filepath.Join(root, "replacement")
			for _, dir := range []string{original, replacement} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(original, "file.txt"), []byte("ORIGINAL\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(replacement, "file.txt"), []byte("FORBIDDEN\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(replacement, "secret.txt"), []byte("FORBIDDEN\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ws, err := NewWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			swapped := false
			ws.SetScopeGuard(func(rel string, write bool) error {
				if !swapped {
					swapped = true
					if err := os.Rename(original, original+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, original); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			if listing {
				result, err := NewList(ws).Invoke(context.Background(), json.RawMessage(`{"path":"public"}`))
				if err != nil || result.Content != "public/file.txt" {
					t.Fatalf("list = %+v, %v", result, err)
				}
			} else {
				data, err := ws.readAll("public/file.txt")
				if err != nil || string(data) != "ORIGINAL\n" {
					t.Fatalf("read = %q, %v", data, err)
				}
			}
		})
	}
}

func TestGlobRejectsUnsafePatterns(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"../*", "sub/../*", "/absolute/*", "nul\x00*"} {
		raw, _ := json.Marshal(globArgs{Pattern: pattern})
		result, err := NewGlob(ws).Invoke(context.Background(), raw)
		if err != nil || !result.IsError || result.Content != "path denied by workspace policy" {
			t.Errorf("%q: %+v, %v", pattern, result, err)
		}
	}
}

func TestWorkspacePinnedRoot(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "original", false: "missing"}[present], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if present {
				if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("ORIGINAL\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ws, err := NewWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			pinned, release, err := ws.workspaceRoot()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ws.pinnedRoot = pinned
			if err := os.Rename(root, root+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("FORBIDDEN\n"), 0600); err != nil {
				t.Fatal(err)
			}
			data, err := ws.readAll("secret.txt")
			if present {
				if err != nil || string(data) != "ORIGINAL\n" {
					t.Fatalf("pinned read %q, %v", data, err)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing pinned read %q, %v", data, err)
			}
			result, err := NewList(ws).Invoke(context.Background(), json.RawMessage(`{}`))
			want := "no entries"
			if present {
				want = "secret.txt"
			}
			if err != nil || result.Content != want {
				t.Fatalf("pinned list %+v, %v", result, err)
			}
			if _, err := pinned.Stat(); err != nil {
				t.Fatalf("borrowed root closed: %v", err)
			}
		})
	}
}

func TestWorkspaceWalkAndMetadataPinned(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(root, "public"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "public", "original"), []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	ws.SetScopeGuard(func(rel string, write bool) error {
		if !swapped {
			swapped = true
			if err := os.Rename(root, root+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "public"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "public", "secret"), []byte("FORBIDDEN\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	var names []string
	err = ws.walk(context.Background(), func(rel string, d fs.DirEntry) error {
		names = append(names, rel)
		if rel == "public/original" {
			info, err := d.Info()
			if err != nil || info.Size() != 9 {
				t.Fatalf("metadata %v, %v", info, err)
			}
		}
		return nil
	})
	if err != nil || len(names) != 2 || names[0] != "public" || names[1] != "public/original" {
		t.Fatalf("walk %v, %v", names, err)
	}
}

func TestWorkspacePermissionCompatibility(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission assertions require non-root")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "search")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("ORIGINAL\n"), 0400); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0111); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }()
	data, err := ws.readAll("search/file")
	if err != nil || string(data) != "ORIGINAL\n" {
		t.Fatalf("search-only read %q, %v", data, err)
	}
	if err := os.Chmod(file, 0111); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.resolveFile("search/file"); err != nil {
		t.Fatalf("execute-only metadata: %v", err)
	}
	if _, _, err := ws.resolveDir("search"); err != nil {
		t.Fatalf("search-only metadata: %v", err)
	}
	if err := os.Chmod(dir, 0400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.resolveDir("search"); err != nil {
		t.Fatalf("readable non-executable directory metadata: %v", err)
	}
}

func TestWorkspaceFIFOAndDescriptorFlags(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := ws.openRegularFile("pipe"); !errors.Is(err, errNotRegular) {
		if f != nil {
			_ = f.Close()
		}
		t.Fatalf("FIFO: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "regular"), []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := ws.openRegularFile("regular")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var flags int
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Control(func(fd uintptr) { flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		t.Fatalf("regular flags %#x: %v", flags, err)
	}
	flags, err = unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor flags %#x: %v", flags, err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "ORIGINAL\n" {
		t.Fatalf("regular read %q, %v", data, err)
	}
}

func TestWorkspaceAncestorNoFollowRace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "public")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	ws.beforeReadOpen = func() {
		ws.beforeReadOpen = nil
		if err := os.Rename(dir, dir+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir+"-original", dir); err != nil {
			t.Fatal(err)
		}
	}
	f, err := ws.openRegularFile("public/file")
	if f != nil {
		_ = f.Close()
	}
	if err == nil {
		t.Fatal("followed substituted ancestor symlink with identical target identity")
	}
}
func TestWorkspaceFIFOReplacement(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	ws.beforeReadOpen = func() {
		ws.beforeReadOpen = nil
		if err := os.Rename(file, file+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(file, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := ws.openRegularFile("file")
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, errFileChanged) {
		t.Fatalf("replaced FIFO: %v", err)
	}
}

func TestWorkspaceEntryMetadataRelative(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("FORBIDDEN LONGER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "link"), 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := readWorkspaceEntries(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil || info.Size() != 9 {
		t.Fatalf("file metadata: %v, %v", info, err)
	}
	if entries[1].Type() != fs.ModeSymlink {
		t.Fatalf("link metadata: %v", entries[1].Type())
	}
}

func TestWorkspaceCanonicalReadPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Secret"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Secret", "File"), []byte("FORBIDDEN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "secret", "file")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	ws.SetScopeGuard(func(rel string, write bool) error {
		seen = append(seen, rel)
		if rel == "Secret/File" {
			return errors.New("private host detail")
		}
		return nil
	})
	result, err := NewReadFile(ws).Invoke(context.Background(), json.RawMessage(`{"path":"secret/file"}`))
	if err != nil || result.Content != "path denied by workspace policy" || !result.IsError {
		t.Fatalf("canonical denial %+v, %v", result, err)
	}
	if len(seen) != 1 || seen[0] != "Secret/File" {
		t.Fatalf("point policy paths %v", seen)
	}
}

func TestWorkspaceDeniedPointFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "File"), []byte("private"), 0000); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	hostErr := errors.New("host-private-policy")
	cases := []struct {
		path      string
		directory bool
	}{{"missing", false}, {"missing/child", false}, {"Dir", false}, {"File", true}, {"File", false}}
	for _, tc := range cases {
		t.Run(tc.path+map[bool]string{false: "-file", true: "-dir"}[tc.directory], func(t *testing.T) {
			var seen []string
			ws.SetScopeGuard(func(rel string, write bool) error { seen = append(seen, rel); return hostErr })
			f, _, err := ws.openRead(tc.path, tc.directory)
			if f != nil {
				_ = f.Close()
			}
			if !errors.Is(err, errScopeDenied) || !errors.Is(err, hostErr) || toolErrMessage(err) != "path denied by workspace policy" {
				t.Fatalf("denied failure: %v (%s)", err, toolErrMessage(err))
			}
			if len(seen) != 1 || seen[0] != tc.path {
				t.Fatalf("point guard calls: %v", seen)
			}
		})
	}
}

func TestWorkspaceCanonicalAliasWithHardlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "File"), []byte("ORIGINAL\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "file")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if err := os.Link(filepath.Join(root, "File"), filepath.Join(root, "Other")); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	ws.SetScopeGuard(func(rel string, write bool) error { seen = append(seen, rel); return nil })
	data, err := ws.readAll("file")
	if err != nil || string(data) != "ORIGINAL\n" {
		t.Fatalf("case alias with hardlink: %q, %v", data, err)
	}
	if len(seen) != 1 || seen[0] != "File" {
		t.Fatalf("canonical guard: %v", seen)
	}
}

func TestWorkspaceSearchOnlyCanonicalPolicy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission assertions require non-root")
	}
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "search")
			if err := os.MkdirAll(filepath.Join(dir, "Secret"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"Secret.txt", "Secret/file", "Allowed.txt", "Privé.txt", "Café.txt"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "secret.txt")); err != nil {
				t.Skip("case-sensitive filesystem")
			}
			ws, err := NewWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			var seen []string
			ws.SetScopeGuard(func(rel string, _ bool) error {
				seen = append(seen, rel)
				if rel == "search/Secret.txt" || rel == "search/Privé.txt" || strings.HasPrefix(rel, "search/Secret/") {
					return errors.New("private policy detail")
				}
				return nil
			})
			if err := os.Chmod(dir, 0111); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			if _, err := os.ReadDir(dir); !errors.Is(err, fs.ErrPermission) {
				t.Skip("filesystem does not enforce search-only directory permissions")
			}
			prefix := "search/"
			if scoped {
				var cleanup func()
				ws, _, cleanup, err = newScopedWorkspace(ws, "search")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(cleanup)
				prefix = ""
			}
			for _, name := range []string{"Secret.txt", "secret.txt", "secret/file", "Prive\u0301.txt"} {
				seen = nil
				raw, _ := json.Marshal(map[string]string{"path": prefix + name})
				out, err := NewReadFile(ws).Invoke(context.Background(), raw)
				if err != nil || !out.IsError || out.Content != "path denied by workspace policy" {
					t.Errorf("%s: got %+v, %v; want sanitized policy denial", name, out, err)
				}
				want := "search/Secret.txt"
				switch name {
				case "secret/file":
					want = "search/Secret/file"
				case "Prive\u0301.txt":
					want = "search/Privé.txt"
				}
				if len(seen) != 1 || seen[0] != want {
					t.Errorf("%s: policy paths %v, want only %s", name, seen, want)
				}
			}
			if scoped && ws.scopeDenials.Load() != 4 {
				t.Errorf("scope denials = %d, want 4", ws.scopeDenials.Load())
			}
			for name, want := range map[string]string{"Allowed.txt": "Allowed.txt", "allowed.txt": "Allowed.txt", "Cafe\u0301.txt": "Café.txt"} {
				data, err := ws.readAll(prefix + name)
				if err != nil || string(data) != want {
					t.Fatalf("known-file control %s: %q, %v", name, data, err)
				}
			}
		})
	}
}

func TestWorkspaceEntriesSkipVanishedName(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// Prime os.File's directory buffer, then unlink a name already returned by
	// the kernel. This exercises stale dirents without a concurrent timing race.
	first, err := f.Readdirnames(1)
	if err != nil {
		t.Fatal(err)
	}
	var vanished, survivor string
	for _, name := range []string{"a", "b", "c"} {
		if name == first[0] {
			continue
		}
		if vanished == "" {
			vanished = name
		} else {
			survivor = name
		}
	}
	if err := os.Remove(filepath.Join(root, vanished)); err != nil {
		t.Fatal(err)
	}
	entries, err := readWorkspaceEntries(f)
	if err != nil || len(entries) != 1 || entries[0].Name() != survivor {
		t.Fatalf("snapshot after unlink: %v, %v; want only %s", entries, err, survivor)
	}
}

// Search must open files from the walked directory descriptor. Re-resolving
// each match by name re-enumerates every ancestor per file, which is quadratic
// in directory size (measured 37x slower on a 3000-file directory).
func TestSearchOpensFromWalkedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("needle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	reopens := 0
	ws.beforeReadOpen = func() { reopens++ }
	out, err := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil || out.IsError {
		t.Fatalf("search: %+v, %v", out, err)
	}
	if want := "a.txt:1: needle\nb.txt:1: needle\nsub/c.txt:1: needle"; out.Content != want {
		t.Fatalf("content = %q, want %q", out.Content, want)
	}
	if reopens != 0 {
		t.Fatalf("search re-resolved %d path components by name; want descriptor opens only", reopens)
	}
	// Control: the seam must fire for a by-name open, or the zero above is vacuous.
	if _, err := ws.readAll("sub/c.txt"); err != nil {
		t.Fatal(err)
	}
	if reopens != 2 {
		t.Fatalf("by-name control fired the seam %d times, want 2 (one per component)", reopens)
	}
}

func TestWorkspaceEntryOpenRegularRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("ORIGINAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := ws.openDir(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := readWorkspaceEntries(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	entry := entries[0].(workspaceEntry)
	if err := os.Rename(filepath.Join(root, "file"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "file"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := entry.openRegular()
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, errFileChanged) {
		t.Fatalf("replaced FIFO: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("moved", filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	f, err = entry.openRegular()
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, errSymlink) {
		t.Fatalf("replaced symlink: %v", err)
	}
}
