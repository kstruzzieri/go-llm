package rag

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

func TestOpenSQLiteStoreReadOnlyRejectsCallerPragmasBeforeSQL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prefix []string
	}{
		{name: "single pragma"},
		{name: "multiple pragmas", prefix: []string{"foreign_keys(1)", "busy_timeout(5000)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "source.db")
			seedReadOnlyMarkerStore(t, path, "original")
			u, err := sqlitedsn.FileURL(path)
			if err != nil {
				t.Fatal(err)
			}
			// A driver pragma can contain SQL after its first statement. The
			// attached handle can write the same file even when main is read-only.
			payload := "busy_timeout(5000); ATTACH DATABASE '" + strings.ReplaceAll(path, "'", "''") + "' AS writable; " +
				"UPDATE writable.readonly_marker SET value = 'changed'; " +
				"CREATE TABLE writable.readonly_injected (value TEXT)"
			u.RawQuery = url.Values{"_pragma": append(tc.prefix, payload)}.Encode()
			store, err := OpenSQLiteStoreReadOnly(u.String())
			if store != nil {
				_ = store.Close()
				t.Errorf("OpenSQLiteStoreReadOnly(%q) returned a store, want nil for caller _pragma", u.String())
			}
			if err == nil || !strings.Contains(err.Error(), "_pragma") {
				t.Errorf("OpenSQLiteStoreReadOnly(%q) error = %v, want explicit _pragma rejection", u.String(), err)
			}

			// Check the source even when the opener reports an error: rejecting
			// after driver connection setup is too late to prevent side effects.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			var marker string
			if err := db.QueryRow("SELECT value FROM readonly_marker").Scan(&marker); err != nil {
				t.Fatalf("read source marker after rejected _pragma: %v", err)
			}
			if marker != "original" {
				t.Errorf("OpenSQLiteStoreReadOnly(%q) left source marker = %q, want original", u.String(), marker)
			}
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name = 'readonly_injected'").Scan(&count); err != nil {
				t.Fatalf("read source schema after rejected _pragma: %v", err)
			}
			if count != 0 {
				t.Errorf("OpenSQLiteStoreReadOnly(%q) created %d injected tables, want 0", u.String(), count)
			}
		})
	}
}

func TestOpenSQLiteStoreReadOnlyResolvesSymlinkBeforeParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves parent components before following directory symlinks")
	}
	dir := t.TempDir()
	logical := filepath.Join(dir, "logical")
	physical := filepath.Join(dir, "physical")
	for _, path := range []string{logical, filepath.Join(physical, "nested")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(physical, "nested"), filepath.Join(logical, "link")); err != nil {
		t.Fatalf("create source directory symlink: %v", err)
	}
	seedReadOnlyMarkerStore(t, filepath.Join(logical, "source.db"), "logical")
	seedReadOnlyMarkerStore(t, filepath.Join(physical, "source.db"), "physical")
	sep := string(filepath.Separator)
	// Do not use filepath.Join here: cleaning link/.. would erase the case
	// under test before OpenSQLiteStoreReadOnly receives the filename.
	for _, tc := range []struct {
		name string
		path string
		cwd  string
	}{
		{name: "absolute", path: logical + sep + "link" + sep + ".." + sep + "source.db", cwd: dir},
		{name: "relative", path: "logical" + sep + "link" + sep + ".." + sep + "source.db", cwd: dir},
		{name: "logical working directory", path: ".." + sep + "source.db", cwd: filepath.Join(logical, "link")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// t.Chdir keeps the supplied logical directory in PWD. Resolving
			// a leading .. must still follow the physical working directory.
			t.Chdir(tc.cwd)
			store, err := OpenSQLiteStoreReadOnly(tc.path)
			if err != nil {
				t.Fatalf("OpenSQLiteStoreReadOnly(%q) error = %v, want success", tc.path, err)
			}
			defer func() { _ = store.Close() }()
			var marker string
			if err := store.db.QueryRow("SELECT value FROM readonly_marker").Scan(&marker); err != nil {
				t.Fatalf("OpenSQLiteStoreReadOnly(%q) read marker: %v", tc.path, err)
			}
			if marker != "physical" {
				t.Errorf("OpenSQLiteStoreReadOnly(%q) marker = %q, want physical", tc.path, marker)
			}
		})
	}
}

func TestOpenSQLiteStoreReadOnlyKeepsDotPrefixedFileName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colons are not ordinary filename characters on Windows")
	}
	dir := t.TempDir()
	seedReadOnlyMarkerStore(t, filepath.Join(dir, "file:source.db"), "original")
	t.Chdir(dir)
	// Removing ./ before encoding would reinterpret this literal filename
	// as a file: URI and open source.db instead of file:source.db.
	path := "./file:source.db"
	store, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly(%q) error = %v, want success", path, err)
	}
	defer func() { _ = store.Close() }()
	var marker string
	if err := store.db.QueryRow("SELECT value FROM readonly_marker").Scan(&marker); err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly(%q) read marker: %v", path, err)
	}
	if marker != "original" {
		t.Errorf("OpenSQLiteStoreReadOnly(%q) marker = %q, want original", path, marker)
	}
}

func TestOpenSQLiteStoreReadOnlyAcceptsURIWithModeAndCache(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "source.db")
	seedReadOnlyMarkerStore(t, path, "original")
	u, err := sqlitedsn.FileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = "mode=rwc&cache=shared&immutable=0"
	store, err := OpenSQLiteStoreReadOnly(u.String())
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly(%q) error = %v, want success", u.String(), err)
	}
	defer func() { _ = store.Close() }()
	var marker string
	if err := store.db.QueryRow("SELECT value FROM readonly_marker").Scan(&marker); err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly(%q) read marker: %v", u.String(), err)
	}
	if marker != "original" {
		t.Errorf("OpenSQLiteStoreReadOnly(%q) marker = %q, want original", u.String(), marker)
	}
	if _, err := store.db.Exec("UPDATE readonly_marker SET value = 'changed'"); err == nil {
		t.Errorf("OpenSQLiteStoreReadOnly(%q) allowed an UPDATE, want read-only error", u.String())
	}
}

func seedReadOnlyMarkerStore(t *testing.T, path, marker string) {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("seed NewSQLiteStore(%q): %v", path, err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.db.Exec("CREATE TABLE readonly_marker (value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create marker in %q: %v", path, err)
	}
	if _, err := store.db.Exec("INSERT INTO readonly_marker VALUES (?)", marker); err != nil {
		t.Fatalf("insert marker in %q: %v", path, err)
	}
}
