package sqlitedsn

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestWithBusyTimeoutPassesThroughMemory(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		got, err := WithBusyTimeout(path, 5*time.Second)
		if err != nil || got != path {
			t.Errorf("WithBusyTimeout(%q) = %q, %v; want %q unchanged", path, got, err, path)
		}
	}
}

func TestWithBusyTimeoutOpensExactFile(t *testing.T) {
	for _, name := range []string{"plain.db", "space name.db", "query?name.db", "hash#name.db", "percent%41name.db"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && name == "query?name.db" {
				t.Skip("'?' is not valid in Windows file names")
			}
			path := filepath.Join(t.TempDir(), name)
			dsn, err := WithBusyTimeout(path, 1500*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
				t.Fatalf("create table via %q: %v", dsn, err)
			}
			var timeout int
			if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
				t.Fatal(err)
			}
			if timeout != 1500 {
				t.Errorf("busy_timeout = %d, want 1500", timeout)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("database not created at %q (dsn %q): %v", path, dsn, err)
			}
		})
	}
}

func TestWithBusyTimeoutKeepsURIParameters(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "uri.db"))
	dsn, err := WithBusyTimeout("file:"+path+"?mode=rwc", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("mode") != "rwc" || q.Get("_pragma") != "busy_timeout(2000)" {
		t.Errorf("WithBusyTimeout query = %q, want mode=rwc and _pragma=busy_timeout(2000)", u.RawQuery)
	}
}

func TestWithBusyTimeoutResolvesRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	dsn, err := WithBusyTimeout("relative.db", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Open from another directory so only an absolute DSN still reaches dir.
	t.Chdir(t.TempDir())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("create table via %q: %v", dsn, err)
	}
	// Compare the file that opened, not the URL path: on Windows the URL path
	// is "/C:/..." while filepath.Abs returns "C:\...".
	if _, err := os.Stat(filepath.Join(dir, "relative.db")); err != nil {
		t.Errorf("relative.db not created in the working directory (dsn %q): %v", dsn, err)
	}
}

// modernc applies busy_timeout pragmas first but leaves the order of two of
// them undefined, so the caller's own timeout must be the only one.
func TestWithBusyTimeoutKeepsCallerBusyTimeout(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "caller.db"))
	dsn, err := WithBusyTimeout("file:"+path+"?_pragma=busy_timeout(250)", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query()["_pragma"]; len(got) != 1 || got[0] != "busy_timeout(250)" {
		t.Fatalf("WithBusyTimeout pragmas = %q, want only the caller's busy_timeout(250)", got)
	}
}

// TestLinkedSQLiteHasWALResetFix fails when the linked SQLite predates 3.51.3,
// the first release with the WAL-reset corruption fix
// (https://www.sqlite.org/wal.html, section 11). go-llm's stores run in WAL
// mode and can share a file across processes, the bug's trigger, so a go.mod
// downgrade of modernc.org/sqlite below v1.46.2 must not pass CI. modernc v1.46.2
// still embeds SQLite 3.40.0 on netbsd/amd64 and 3.41.2 on freebsd/386 and
// freebsd/arm; this test fails there by design, because those targets do not
// have the fix.
func TestLinkedSQLiteHasWALResetFix(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("parse sqlite_version() %q: %v", version, err)
	}
	if slices.Compare([]int{major, minor, patch}, []int{3, 51, 3}) < 0 {
		t.Fatalf("linked SQLite %s predates 3.51.3 (WAL-reset corruption fix); keep modernc.org/sqlite at v1.46.2 or later", version)
	}
}
