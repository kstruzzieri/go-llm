package sqlitedsn

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sqliteMainFile opens dsn, switches it to WAL as the store openers do, and
// returns the main database's file name as SQLite itself reports it. That
// name is SQLite's full pathname of the file it opened ("" for a temporary or
// in-memory database), so it is the oracle Resolve is checked against.
func sqliteMainFile(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := EnableWAL(context.Background(), db); err != nil {
		t.Fatalf("EnableWAL(%q): %v", dsn, err)
	}
	var file string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	return file
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// uriPath renders an absolute path as a file: URI filename, with the leading
// "/" Windows drive paths need ("/C:/x").
func uriPath(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// TestResolveMatchesSQLite checks Resolve's disk path against the file SQLite
// opens for the DSN Resolve returns, for every input form the store openers
// accept. Each case runs from its own working directory under a fresh tree.
func TestResolveMatchesSQLite(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, base string) string // returns the input; cwd is base/cwd
	}{
		{"plain absolute", func(t *testing.T, base string) string { return filepath.Join(base, "real", "t.db") }},
		{"plain relative", func(t *testing.T, base string) string { return "t.db" }},
		{"plain dotdot after symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "sub"), filepath.Join(base, "cwd", "link"))
			return filepath.Join("link", "..", "t.db")
		}},
		{"plain final-component symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "real.db"), filepath.Join(base, "cwd", "t.db"))
			return "t.db"
		}},
		{"plain relative symlink target", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join("..", "real", "real.db"), filepath.Join(base, "cwd", "t.db"))
			return "t.db"
		}},
		{"plain symlink to symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "real.db"), filepath.Join(base, "real", "hop.db"))
			symlinkOrSkip(t, filepath.Join(base, "real", "hop.db"), filepath.Join(base, "cwd", "t.db"))
			return "t.db"
		}},
		{"uri absolute", func(t *testing.T, base string) string { return "file:" + uriPath(filepath.Join(base, "real", "t.db")) }},
		{"uri escaped space", func(t *testing.T, base string) string {
			return "file:" + uriPath(filepath.Join(base, "real")) + "/a%20b.db"
		}},
		{"uri relative", func(t *testing.T, base string) string { return "file:t.db" }},
		{"uri dotdot after symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "sub"), filepath.Join(base, "cwd", "link"))
			return "file:" + uriPath(filepath.Join(base, "cwd")) + "/link/../t.db"
		}},
		{"uri relative dotdot after symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "sub"), filepath.Join(base, "cwd", "link"))
			return "file:link/../t.db"
		}},
		{"uri dotdot over missing directory", func(t *testing.T, base string) string {
			return "file:" + uriPath(filepath.Join(base, "cwd")) + "/missing/../t.db"
		}},
		{"uri final-component symlink", func(t *testing.T, base string) string {
			symlinkOrSkip(t, filepath.Join(base, "real", "real.db"), filepath.Join(base, "cwd", "t.db"))
			return "file:" + uriPath(filepath.Join(base, "cwd", "t.db"))
		}},
		{"uri with query", func(t *testing.T, base string) string {
			return "file:" + uriPath(filepath.Join(base, "real", "t.db")) + "?cache=private&_pragma=busy_timeout(250)"
		}},
		{"memory", func(t *testing.T, base string) string { return ":memory:" }},
		{"uri memory", func(t *testing.T, base string) string { return "file::memory:" }},
		{"uri mode=memory", func(t *testing.T, base string) string { return "file:m.db?mode=memory" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			mkdirAll(t, filepath.Join(base, "cwd"))
			mkdirAll(t, filepath.Join(base, "real", "sub"))
			input := tc.setup(t, base)
			t.Chdir(filepath.Join(base, "cwd"))
			dsn, disk, err := Resolve(input, time.Second)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", input, err)
			}
			if disk != "" && !filepath.IsAbs(disk) {
				t.Fatalf("Resolve(%q) disk path %q is not absolute", input, disk)
			}
			if got := sqliteMainFile(t, dsn); got != disk {
				t.Fatalf("Resolve(%q) = disk %q, but SQLite opened %q (dsn %q)", input, disk, got, dsn)
			}
		})
	}
}

// TestResolveAbsolutizesRelativeURI: a relative file: URI is pinned to the
// working directory at Resolve time, so a connection database/sql opens later,
// after the process changed directory, still reaches the hardened file.
func TestResolveAbsolutizesRelativeURI(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	dsn, disk, err := Resolve("file:rel.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Opaque != "" {
		t.Fatalf("DSN %q keeps the relative opaque filename %q", dsn, u.Opaque)
	}
	t.Chdir(t.TempDir())
	if got := sqliteMainFile(t, dsn); got != disk {
		t.Fatalf("after chdir SQLite opened %q, want the resolved %q (dsn %q)", got, disk, dsn)
	}
}

// TestResolveKeepsMemoryURISpelling: a memory URI names no file and keeps its
// spelling, which shared-cache identity depends on.
func TestResolveKeepsMemoryURISpelling(t *testing.T) {
	dsn, disk, err := Resolve("file::memory:?cache=shared", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if disk != "" || u.Opaque != ":memory:" || u.Query().Get("cache") != "shared" {
		t.Fatalf("Resolve = (%q, %q), want the :memory: URI kept and no disk path", dsn, disk)
	}
}

// TestResolveRejects: inputs whose target cannot be predicted, or that SQLite
// reads differently from Go, are refused before an opener creates anything.
func TestResolveRejects(t *testing.T) {
	t.Chdir(t.TempDir())
	for name, tc := range map[string]struct{ input, want string }{
		"NUL in filename":    {"file:a%00b.db", "NUL in its filename"},
		"NUL in query key":   {"file:x.db?mode=memory&mode%00x=rwc", "NUL in its query"},
		"duplicate mode":     {"file:x.db?mode=ro&mode=rwc", "repeats mode"},
		"duplicate mode mem": {"file:x.db?mode=memory&mode=rwc", "repeats mode"},
		"vfs parameter":      {"file:x.db?vfs=unix-none", "selects a VFS"},
		"semicolon pair":     {"file:x.db?a=1;b=2", "read differently"},
		"bad query escape":   {"file:x.db?a=%zz", "read differently"},
		"foreign host":       {"file://server/x.db", "names host"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Resolve(tc.input, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve(%q) = %v, want an error containing %q", tc.input, err, tc.want)
			}
		})
	}
}

func TestResolveBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	dsn, _, err := Resolve(path, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var timeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 1500 {
		t.Fatalf("busy_timeout = %d, want 1500", timeout)
	}
}

// TestCheckMainFile: the post-open check passes when SQLite opened the
// predicted file and fails, naming both, when it did not.
func TestCheckMainFile(t *testing.T) {
	base := t.TempDir()
	dsn, disk, err := Resolve(filepath.Join(base, "a.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := CheckMainFile(context.Background(), db, disk); err != nil {
		t.Fatalf("CheckMainFile(predicted): %v", err)
	}
	other := filepath.Join(base, "b.db")
	if err := CheckMainFile(context.Background(), db, other); err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("CheckMainFile(other) = %v, want an error naming %q", err, other)
	}
}

func TestResolveWindowsDriveRelative(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-relative paths exist only on Windows")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	vol := filepath.VolumeName(dir)
	input := vol + "dr.db" // C:dr.db: relative to the working directory on drive C
	dsn, disk, err := Resolve(input, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := sqliteMainFile(t, dsn); got != disk {
		t.Fatalf("Resolve(%q) = disk %q, SQLite opened %q", input, disk, got)
	}
}
