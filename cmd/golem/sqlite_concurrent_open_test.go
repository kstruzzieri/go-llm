package main

import (
	"context"
	"io"
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
// journal mode "delete". The working directory is a temp dir because the
// opener prepares the raw path string, which for a file: URI is a relative
// path (#648).
func TestOpenSessionRejectsNonWAL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("#648: the opener prepares the raw file: string, which Windows rejects as a file name")
	}
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	t.Chdir(t.TempDir())
	s, _, err := openSession(t.Context(), "file:"+path+"?nolock=1", "nonwal")
	if err == nil {
		_ = s.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
}

// A connection that cannot use WAL must fail the open, not run the store with
// a rollback journal: nolock=1 turns locking off, and SQLite then reports
// journal mode "delete". The working directory is a temp dir because the
// opener prepares the raw path string, which for a file: URI is a relative
// path (#648).
func TestOpenFeedbackServiceRejectsNonWAL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("#648: the opener prepares the raw file: string, which Windows rejects as a file name")
	}
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	t.Chdir(t.TempDir())
	svc, err := openFeedbackService(t.Context(), t.TempDir(), "file:"+path+"?nolock=1", func(string) {})
	if err == nil {
		_, _ = svc.close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
}
