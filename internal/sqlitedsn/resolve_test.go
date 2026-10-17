package sqlitedsn

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		"user in authority":  {"file://user@localhost/x.db", "names host"},
		"fragment":           {"file:x.db#frag", "has a fragment"},
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
	for _, other := range []string{filepath.Join(base, "b.db"), `C:\data\b.db`} {
		if err := CheckMainFile(context.Background(), db, other); err == nil || !strings.Contains(err.Error(), strconv.Quote(other)) {
			t.Fatalf("CheckMainFile(%q) = %v, want an error naming %q", other, err, other)
		}
	}
}

func TestCheckMainFileRejectsBeforeWALInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	dsn, _, err := Resolve(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := CheckMainFile(t.Context(), db, filepath.Join(t.TempDir(), "other.db")); err == nil {
		t.Fatal("CheckMainFile with a different path succeeded, want a mismatch error")
	}
	// Opening the lazy connection can create a file, but checking its name
	// must not initialize the database header or create WAL sidecars.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("CheckMainFile initialized %q to %d bytes, want an empty file", path, info.Size())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("CheckMainFile left %q (stat error %v), want no sidecar", path+suffix, err)
		}
	}
}

func TestCheckMainFileWhileDatabaseLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	holder := pinnedHolder(t, path)
	execConn(t, holder, "BEGIN EXCLUSIVE", "CREATE TABLE t (x)")
	dsn, disk, err := Resolve(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var tables int
	assertBusy(t, db.QueryRow("SELECT COUNT(*) FROM sqlite_schema").Scan(&tables))
	if err := CheckMainFile(t.Context(), db, disk); err != nil {
		t.Fatalf("CheckMainFile(%q) under an exclusive lock = %v, want filename check without reading the schema", disk, err)
	}
}

func TestCheckMainFileIgnoresShadowTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.db")
	dsn, disk, err := Resolve(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE pragma_database_list (name TEXT, file TEXT); INSERT INTO pragma_database_list VALUES ('main', 'spoofed.db')"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMainFile(t.Context(), db, disk); err != nil {
		t.Fatalf("CheckMainFile(%q) with a shadow table = %v, want SQLite's actual filename", disk, err)
	}
	if err := CheckMainFile(t.Context(), db, "spoofed.db"); err == nil {
		t.Fatal("CheckMainFile accepted the shadow table's spoofed filename, want a mismatch error")
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

func TestResolveRequiresExistingFileForReadModes(t *testing.T) {
	for _, mode := range []string{"ro", "rw"} {
		for _, name := range []string{"missing.db", "missing-parent/missing.db"} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				base := t.TempDir()
				path := filepath.Join(base, name)
				input := "file:" + uriPath(path) + "?mode=" + mode
				dsn, disk, err := Resolve(input, time.Second)
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Resolve(%q) = %v, want a missing-file error", input, err)
				}
				// Callers checking output collisions still need the future target,
				// even though this URI cannot currently open a database.
				want, err := filepath.Abs(base)
				if runtime.GOOS != "windows" {
					want, err = filepath.EvalSymlinks(base)
				}
				if err != nil {
					t.Fatal(err)
				}
				want = filepath.Join(want, name)
				if dsn != "" || disk != want {
					t.Errorf("Resolve(%q) = DSN %q, disk %q; want empty DSN and disk %q", input, dsn, disk, want)
				}
				if entries, err := os.ReadDir(base); err != nil || len(entries) != 0 {
					t.Errorf("Resolve(%q) left %v, %v; want no files or directories", input, entries, err)
				}
			})
		}
		t.Run(mode+"/existing", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.db")
			want := sqliteMainFile(t, path)
			input := "file:" + uriPath(path) + "?mode=" + mode
			dsn, disk, err := Resolve(input, time.Second)
			if err != nil {
				t.Fatalf("Resolve(%q) = %v, want existing database accepted", input, err)
			}
			if got := sqliteMainFile(t, dsn); got != disk || got != want {
				t.Errorf("Resolve(%q) = disk %q, SQLite opened %q; want %q", input, disk, got, want)
			}
		})
	}
}
