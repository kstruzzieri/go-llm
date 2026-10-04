package sqlitedsn

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
		if err := windowsURIFilename(mustParse(t, in)); err != nil {
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
	} {
		if err := windowsURIFilename(mustParse(t, in)); err == nil {
			t.Errorf("windowsURIFilename(%q) = nil, want an error", in)
		}
	}
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
	db, err := sql.Open("sqlite", "file://localhost/"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("open file://localhost/ URI: %v", err)
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
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}
