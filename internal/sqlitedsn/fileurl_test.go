package sqlitedsn

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWindowsURIPath(t *testing.T) {
	accept := map[string]string{
		`C:\Users\fred\x.db`: "/C:/Users/fred/x.db",
		`c:\a b\#x%41.db`:    "/c:/a b/#x%41.db",
		`D:/mixed\sep/x.db`:  "/D:/mixed/sep/x.db",
		`Z:\`:                "/Z:/",
	}
	for in, want := range accept {
		got, err := windowsURIPath(in)
		if err != nil || got != want {
			t.Errorf("windowsURIPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// The rendered URL is what SQLite parses: no authority, drive kept. This
	// is also the golden for cmd/golem's audit URI, whose Windows test only
	// compiles in CI.
	p, err := windowsURIPath(`C:\audit files\100%.db`)
	if err != nil {
		t.Fatal(err)
	}
	u := &url.URL{Scheme: "file", Path: p, RawQuery: url.Values{"cache": {"private"}, "immutable": {"1"}, "mode": {"ro"}}.Encode()}
	if got, want := u.String(), "file:///C:/audit%20files/100%25.db?cache=private&immutable=1&mode=ro"; got != want {
		t.Errorf("rendered URL = %q, want %q", got, want)
	}
	for _, in := range []string{
		`\\server\share\x.db`, `//server/share/x.db`, `\\?\C:\x.db`, `\\?\UNC\server\share\x.db`,
		`\\.\pipe\x`, `C:`, `C:x.db`, `dir\x.db`, `1:\x.db`,
	} {
		if got, err := windowsURIPath(in); err == nil {
			t.Errorf("windowsURIPath(%q) = %q, want an error", in, got)
		}
	}
}

func TestWindowsURIFilename(t *testing.T) {
	for _, in := range []string{
		"file:///C:/x.db", "file://localhost/C:/x.db", "file:x.db", "file::memory:",
		"file:///C:/x.db?mode=ro", "file:C:/x.db",
	} {
		if err := windowsURIFilename(mustParse(t, in), windowsAbs(`C:\work`)); err != nil {
			t.Errorf("windowsURIFilename(%q) = %v, want nil", in, err)
		}
	}
	for _, in := range []string{
		"file:%5C%5Cserver%5Cshare%5Cx.db",
		"file:/%5C%5C%3F%5CUNC%5Cserver%5Cshare%5Cx.db",
		"file:%5C%5C.%5Cpipe%5Cx",
		"file:////server/share/x.db",
		"file://server/share/x.db",
		"file://LOCALHOST/C:/x.db",
		// A stray % does not decode, but SQLite copies it literally.
		"file:%5C%5Cserver%5Cshare%5C100%.db",
	} {
		if err := windowsURIFilename(mustParse(t, in), windowsAbs(`C:\work`)); err == nil {
			t.Errorf("windowsURIFilename(%q) = nil, want an error", in)
		}
	}
}

// A relative or rooted URI filename resolves against the working directory,
// as SQLite resolves it with GetFullPathNameW. Under a UNC working directory
// it opens the share, so it must be rejected; under a drive it is local.
func TestWindowsURIFilenameResolvesAgainstWorkingDirectory(t *testing.T) {
	unc := windowsAbs(`\\server\share\work`)
	for _, in := range []string{
		"file:x.db", "file:/x.db", "file:///x.db", "file://localhost/x.db", "file:sub/x.db?mode=ro",
		// Only the filename decides whether a file is named: SQLite applies the
		// last mode option, and callers may change the query after FileURL returns.
		"file:shared?mode=memory&cache=shared", "file:x.db?mode=memory&mode=rwc", "file:x.db?mode=memory",
		// SQLite stops the filename at %00 and opens x.db; Windows filepath.Abs
		// fails on the NUL, which must not be read as an in-memory database.
		"file:x.db%00", "file:///x.db%00",
	} {
		if err := windowsURIFilename(mustParse(t, in), unc); err == nil {
			t.Errorf("windowsURIFilename(%q) under a UNC working directory = nil, want an error", in)
		}
	}
	// These name a drive-letter file, or no file at all.
	for _, in := range []string{
		"file:///C:/x.db", "file://localhost/C:/x.db", "file:C:/x.db",
		"file::memory:", "file::memory:?cache=shared",
		"file:", "file:?mode=ro",
	} {
		if err := windowsURIFilename(mustParse(t, in), unc); err != nil {
			t.Errorf("windowsURIFilename(%q) under a UNC working directory = %v, want nil", in, err)
		}
	}
	drive := windowsAbs(`C:\work`)
	for _, in := range []string{"file:x.db", "file:/x.db", "file:///x.db"} {
		if err := windowsURIFilename(mustParse(t, in), drive); err != nil {
			t.Errorf("windowsURIFilename(%q) under a drive working directory = %v, want nil", in, err)
		}
	}
}

// windowsAbs models filepath.Abs on Windows (GetFullPathNameW) with working
// directory cwd, for the forms the tests use: drive-qualified and UNC paths
// are kept, a rooted path takes cwd's drive or share, and a relative path is
// joined to cwd. Like Windows, it fails on a NUL byte.
func windowsAbs(cwd string) func(string) (string, error) {
	return func(p string) (string, error) {
		if strings.Contains(p, "\x00") {
			return "", fmt.Errorf("invalid argument: %q contains NUL", p)
		}
		p = strings.ReplaceAll(p, "/", `\`)
		switch {
		case len(p) >= 3 && p[1] == ':' && p[2] == '\\', strings.HasPrefix(p, `\\`):
			return p, nil
		case strings.HasPrefix(p, `\`):
			return windowsVolume(cwd) + p, nil
		default:
			return cwd + `\` + p, nil
		}
	}
}

// windowsVolume returns the drive ("C:") or share (\\server\share) of cwd.
func windowsVolume(cwd string) string {
	if strings.HasPrefix(cwd, `\\`) {
		parts := strings.SplitN(cwd[2:], `\`, 3)
		return `\\` + parts[0] + `\` + parts[1]
	}
	return cwd[:2]
}

func TestFileURLRejectsEmptyPath(t *testing.T) {
	if u, err := FileURL(""); err == nil {
		t.Fatalf(`FileURL("") = %v, want an error`, u)
	}
}

func TestFileURLKeepsURIQuery(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "q.db"))
	u, err := FileURL("file:" + path + "?mode=ro&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("mode") != "ro" || q.Get("_pragma") != "foreign_keys(1)" {
		t.Errorf("FileURL query = %q, want mode=ro and _pragma=foreign_keys(1)", u.RawQuery)
	}
}

// The new Windows CI job runs this; elsewhere it skips.
func TestFileURLOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path handling")
	}
	dir := t.TempDir()
	for _, in := range []string{
		`\\server\share\x.db`, `\\?\` + filepath.Join(dir, "x.db"), `\\.\pipe\x`,
		"file:%5C%5Cserver%5Cshare%5Cx.db", "file:/%5C%5C%3F%5CUNC%5Cserver%5Cshare%5Cx.db",
		"file:////server/share/x.db", "file://server/share/x.db",
	} {
		if dsn, err := WithBusyTimeout(in, 0); err == nil {
			t.Errorf("WithBusyTimeout(%q) = %q, want an error", in, dsn)
		}
	}
	path := filepath.Join(dir, "local host.db")
	dsn, err := WithBusyTimeout("file://localhost/"+filepath.ToSlash(path), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("open file://localhost/ URI via %q: %v", dsn, err)
	}
	u, err := FileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.String(), "file:///") {
		t.Errorf("FileURL(%q) = %q, want file:///<drive>:/...", path, u)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not created at %q: %v", path, err)
	}

	// A relative URI resolves against the working directory, as SQLite will:
	// FileURL must resolve it with the real filepath.Abs, accept it under a
	// drive, and the DSN must open that directory's file.
	t.Chdir(dir)
	relDSN, err := WithBusyTimeout("file:relative.db", time.Second)
	if err != nil {
		t.Fatalf("WithBusyTimeout(file:relative.db) under %q: %v", dir, err)
	}
	relDB, err := sql.Open("sqlite", relDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relDB.Close() }()
	if _, err := relDB.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("open relative URI %q: %v", relDSN, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "relative.db")); err != nil {
		t.Errorf("relative URI did not create relative.db in %q: %v", dir, err)
	}

	// The real filepath.Abs turns NUL into the device path \\.\NUL, which must be rejected.
	if dsn, err := WithBusyTimeout("file:NUL", 0); err == nil {
		t.Errorf(`WithBusyTimeout("file:NUL") = %q, want an error: filepath.Abs resolves NUL to the \\.\NUL device`, dsn)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}
