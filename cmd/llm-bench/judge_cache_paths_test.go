package main

import (
	"bytes"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
	"modernc.org/sqlite"
)

func judgeCacheFileURI(t *testing.T, path string) string {
	t.Helper()
	u, err := sqlitedsn.FileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	return u.String()
}

func TestOpenJudgeCache_URIPathCreatesOnlyTargetParent(t *testing.T) {
	wd := t.TempDir()
	t.Chdir(wd)
	path := filepath.Join(t.TempDir(), "missing", "nested", "judge %#.db")
	c, err := openJudgeCache(judgeCacheFileURI(t, path))
	if err != nil {
		t.Fatal(err)
	}
	cleanupJudgeCache(t, c)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not at URI target: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil || len(entries) != 0 {
		t.Fatalf("URI open created working-directory entries: %v, err=%v", entries, err)
	}
}

func TestOpenJudgeCache_MissingParentAfterSymlinkDotDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SQLite resolves Windows paths lexically")
	}
	for _, form := range []string{"path", "URI"} {
		t.Run(form, func(t *testing.T) {
			t.Chdir(t.TempDir())
			root, target, input := makeSymlinkDotDotPath(t, "missing/judge.db")
			if form == "URI" {
				u, err := sqlitedsn.FileURLPreservingPath(input)
				if err != nil {
					t.Fatal(err)
				}
				input = u.String()
			}
			c, err := openJudgeCache(input)
			if err != nil {
				t.Fatal(err)
			}
			cleanupJudgeCache(t, c)
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("cache not at symlink-resolved target: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lexically cleaned parent was created: %v", err)
			}
		})
	}
}

func TestResolveJudgeCachePath_KeepsCallerBusyTimeoutOnce(t *testing.T) {
	input := judgeCacheFileURI(t, filepath.Join(t.TempDir(), "judge.db")) + "?_pragma=busy_timeout(17)&_pragma=foreign_keys(1)"
	dsn, _, err := resolveJudgeCachePath(input)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query()["_pragma"]; !slices.Equal(got, []string{"busy_timeout(17)", "foreign_keys(1)"}) {
		t.Fatalf("connection pragmas = %v, want one caller timeout and foreign_keys", got)
	}
	c, err := openJudgeCache(input)
	if err != nil {
		t.Fatal(err)
	}
	cleanupJudgeCache(t, c)
	c.db.SetMaxIdleConns(0)
	if got := judgeCacheQuery(t, c.db, "PRAGMA busy_timeout"); got != "17" {
		t.Fatalf("replacement busy_timeout = %s, want 17", got)
	}
	if got := judgeCacheQuery(t, c.db, "PRAGMA foreign_keys"); got != "1" {
		t.Fatalf("replacement foreign_keys = %s, want 1", got)
	}
}

func TestOpenJudgeCache_CurrentSchemaWhileWriterHeld(t *testing.T) {
	c, path := newTestCache(t)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE judge_cache_schema_version SET description=description WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ro", "rw"} {
		t.Run(mode, func(t *testing.T) {
			other, err := openJudgeCache(judgeCacheFileURI(t, path) + "?mode=" + mode + "&_pragma=busy_timeout(0)")
			if err != nil {
				t.Fatalf("current-schema open with held writer, mode=%s: %v", mode, err)
			}
			cleanupJudgeCache(t, other)
			if got := judgeCacheQuery(t, other.db, judgeCacheVersionsQuery); got != "1,2" {
				t.Fatalf("schema versions = %q, want 1,2", got)
			}
		})
	}
}

func TestOpenJudgeCache_RejectsMalformedURIWithoutModifyingDB(t *testing.T) {
	for _, query := range []string{"mode=ro;ignored=1", "mode=ro&bad=%zz", "mode=ro&mode=rwc", "vfs=unix", "mode=ro%00ignored"} {
		t.Run(query, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := filepath.Join(t.TempDir(), "judge.db")
			seedJudgeCacheV1(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c, err := openJudgeCache(judgeCacheFileURI(t, path) + "?" + query)
			if c != nil {
				if closeErr := c.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if !errors.Is(err, errJudgeCacheOpen) {
				t.Errorf("openJudgeCache(%q) = %v, want errJudgeCacheOpen", query, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("invalid URI modified the existing database")
			}
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 0 {
				t.Errorf("invalid URI created entries: %v, err=%v", entries, err)
			}
		})
	}
}

func TestOpenJudgeCache_NoDirectoriesForMemoryOrRejectedURI(t *testing.T) {
	for _, query := range []string{"mode=memory", "mode=ro", "mode=rw", "mode=ro;ignored=1"} {
		t.Run(query, func(t *testing.T) {
			t.Chdir(t.TempDir())
			c, err := openJudgeCache("file:missing/nested/judge.db?" + query)
			if c != nil {
				cleanupJudgeCache(t, c)
			}
			if query == "mode=memory" && err != nil {
				t.Fatalf("open memory URI: %v", err)
			}
			if query != "mode=memory" && !errors.Is(err, errJudgeCacheOpen) {
				t.Errorf("openJudgeCache(%q) = %v, want errJudgeCacheOpen", query, err)
			}
			entries, err := os.ReadDir(".")
			if err != nil || len(entries) != 0 {
				t.Fatalf("non-creating URI created entries: %v, err=%v", entries, err)
			}
		})
	}
}

func TestOpenJudgeCache_RelativeURIReplacementKeepsDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	c, err := openJudgeCache("file:judge.db")
	if err != nil {
		t.Fatal(err)
	}
	cleanupJudgeCache(t, c)
	c.db.SetMaxIdleConns(0)
	t.Chdir(t.TempDir())
	if got := judgeCacheQuery(t, c.db, judgeCacheVersionsQuery); got != "1,2" {
		t.Fatalf("replacement schema versions = %q, want 1,2", got)
	}
	if _, err := os.Stat("judge.db"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement created a different database: %v", err)
	}
}

func TestOpenJudgeCache_PreservesUnderlyingErrors(t *testing.T) {
	t.Run("filesystem", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := openJudgeCache(filepath.Join(parent, "judge.db"))
		var pathErr *os.PathError
		if !errors.Is(err, errJudgeCacheOpen) || !errors.As(err, &pathErr) {
			t.Fatalf("open error = %v, want open sentinel and PathError", err)
		}
	})
	t.Run("migration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "judge.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE judge_cache (wrong TEXT)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = openJudgeCache(path)
		var sqliteErr *sqlite.Error
		if !errors.Is(err, errJudgeCacheMigrate) || !errors.As(err, &sqliteErr) {
			t.Fatalf("migration error = %v, want migrate sentinel and SQLite error", err)
		}
	})
}
