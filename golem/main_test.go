package golem_test

import (
	"os"
	"path/filepath"
	"testing"
)

// testDataHome is the data directory TestMain gave this process.
var testDataHome string

// TestMain keeps every test off the developer's data directory: Runtime opens
// its session store under XDG_DATA_HOME (else ~/.local/share), so a test that
// does not set it would create, or migrate, the real golem/sessions.db.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "golem-test-data-*")
	if err != nil {
		panic(err)
	}
	testDataHome = dir
	if err := os.Setenv("XDG_DATA_HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestDataHomeIsUnderTempDir pins TestMain's isolation. The path must be
// absolute too: the data dir resolver ignores a relative XDG_DATA_HOME and
// falls back to HOME.
func TestDataHomeIsUnderTempDir(t *testing.T) {
	got := os.Getenv("XDG_DATA_HOME")
	rel, err := filepath.Rel(os.TempDir(), got)
	if got == "" || got != testDataHome || !filepath.IsAbs(got) || err != nil || !filepath.IsLocal(rel) || rel == "." {
		t.Fatalf("XDG_DATA_HOME = %q, TestMain chose %q, temp dir %q", got, testDataHome, os.TempDir())
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("data home is not a directory: %v", err)
	}
}
