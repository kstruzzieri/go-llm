package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

func TestOpenSessionConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		s, _, err := openSession(ctx, path, "concurrent")
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}

func TestOpenFeedbackServiceConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		svc, err := openFeedbackService(ctx, filepath.Dir(path), path, func(string) {})
		if err != nil {
			return nil, err
		}
		return closeFunc(func() error { _, err := svc.close(); return err }), nil
	})
}

// A connection that cannot use WAL must fail the open, not run the store with
// a rollback journal: nolock=1 turns locking off, and SQLite then reports
// journal mode "delete". The working directory is empty so a decoy named after
// the raw URI would show (#648).
func TestOpenSessionRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	cwd := t.TempDir()
	t.Chdir(cwd)
	s, _, err := openSession(t.Context(), "file:"+path+"?nolock=1", "nonwal")
	if err == nil {
		_ = s.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
	assertEmptyDir(t, cwd)
}

// TestOpenFeedbackServiceRejectsNonWAL is TestOpenSessionRejectsNonWAL for
// the feedback writer.
func TestOpenFeedbackServiceRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	cwd := t.TempDir()
	t.Chdir(cwd)
	svc, err := openFeedbackService(t.Context(), t.TempDir(), "file:"+path+"?nolock=1", func(string) {})
	if err == nil {
		_, _ = svc.close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
	assertEmptyDir(t, cwd)
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("%s after the open: %v, %v; want it empty", dir, entries, err)
	}
}

func golemFileURI(name string) string { return "file:" + name }

// TestOpenSessionHardensTheFileSQLiteOpens: session transcripts are private;
// the 0600 promise covers the file SQLite opens for every input form (#648).
func TestOpenSessionHardensTheFileSQLiteOpens(t *testing.T) {
	sqlitetest.RunHardenedPaths(t, sqlitetest.HardenedPathsOptions{
		URI:      golemFileURI,
		MemoryOK: true,
		Open: func(ctx context.Context, input string) (io.Closer, error) {
			s, _, err := openSession(ctx, input, "hardened")
			if err != nil {
				return nil, err
			}
			return s, nil
		},
	})
}

// TestOpenFeedbackServiceHardensTheFileSQLiteOpens: the feedback writer and
// its file-based weight reader must share the file SQLite opens; an input
// naming no file has nothing for the reader to open and is refused (#648).
func TestOpenFeedbackServiceHardensTheFileSQLiteOpens(t *testing.T) {
	sqlitetest.RunHardenedPaths(t, sqlitetest.HardenedPathsOptions{
		URI:      golemFileURI,
		MemoryOK: false,
		Open: func(ctx context.Context, input string) (io.Closer, error) {
			svc, err := openFeedbackService(ctx, t.TempDir(), input, func(string) {})
			if err != nil {
				return nil, err
			}
			return closeFunc(func() error { _, err := svc.close(); return err }), nil
		},
	})
}

// TestFeedbackGuardChecksTheFileSQLiteOpens: -feedback-db must stay outside the
// workspace for the file SQLite opens, not for the raw input: a file: URI read
// relative to a working directory elsewhere, or a URI whose ".." follows a
// symlink into the workspace, is refused before the opener runs (#648).
func TestFeedbackGuardChecksTheFileSQLiteOpens(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T) string{
		"absolute URI inside": func(t *testing.T) string {
			return golemFileURI(filepath.ToSlash(filepath.Join(root, "x.db")))
		},
		"URI dotdot through a symlink into the workspace": func(t *testing.T) string {
			if runtime.GOOS == "windows" {
				t.Skip("Windows SQLite resolves .. lexically")
			}
			if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(outside, "link")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return golemFileURI(filepath.ToSlash(filepath.Join(outside, "link")) + "/../x.db")
		},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			db := input(t)
			t.Chdir(outside)
			called := false
			opener := func(context.Context, string, string, func(string)) (*feedbackService, error) {
				called = true
				return nil, nil
			}
			svc, warning := openConfiguredFeedback(t.Context(), true, root, db, func(string) string { return "" }, func(string) {}, opener)
			if called || svc != nil {
				t.Fatalf("openConfiguredFeedback(%q) reached the opener; want the workspace guard to refuse it", db)
			}
			if !strings.Contains(warning, "behavioral feedback disabled") {
				t.Fatalf("warning = %q, want feedback disabled by the guard", warning)
			}
		})
	}
}

// TestChmodIndexDBFilesFollowsTheFileSQLiteOpens: when the index path is a
// symlink, SQLite opens the target and names its sidecars after it, so those
// are the files to secure (#648).
func TestChmodIndexDBFilesFollowsTheFileSQLiteOpens(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not portable on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.db")
	for _, p := range []string{target, target + "-wal", target + "-shm"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "index.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := chmodIndexDBFiles(link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{target, target + "-wal", target + "-shm"} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s = %v, %v; want 0600", p, info, err)
		}
	}
}
