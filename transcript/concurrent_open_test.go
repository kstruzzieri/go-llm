package transcript

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestOpenConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		s, err := Open(ctx, path)
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}

// Another connection's last close unlinks the WAL sidecars; when that lands
// between a sidecar's Stat and its Chmod, the open must treat it as absent.
func TestChmodTranscriptDBFilesSkipsSidecarRemovedAfterStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		// Chmod, not the create mode: a strict umask would pre-secure the fixture.
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chmod := func(p string, mode os.FileMode) error {
		if p != path {
			_ = os.Remove(p)
		}
		return os.Chmod(p, mode)
	}
	if err := chmodTranscriptDBFilesWith(path, chmod); err != nil {
		t.Fatalf("chmodTranscriptDBFilesWith = %v, want nil for sidecars removed after Stat", err)
	}
	// File mode bits are not portable on Windows.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != transcriptFileMode {
			t.Fatalf("db mode = %v, want %v", got, transcriptFileMode)
		}
	}
}

// Only a vanished file is tolerated: any other chmod error must fail the
// open, or a transcript could stay readable by others.
func TestChmodTranscriptDBFilesReportsChmodError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	chmod := func(p string, _ os.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: p, Err: fs.ErrPermission}
	}
	if err := chmodTranscriptDBFilesWith(path, chmod); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("chmodTranscriptDBFilesWith = %v, want an fs.ErrPermission error", err)
	}
}

// A directory where a sidecar belongs is an error, not something to chmod.
func TestChmodTranscriptDBFilesRejectsSidecarDirectory(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path+suffix, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := chmodTranscriptDBFiles(path); err == nil {
				t.Fatalf("chmodTranscriptDBFiles = nil with a directory at %s, want an error", suffix)
			}
		})
	}
}

// A connection that cannot use WAL must fail the open, not run the store with
// a rollback journal: nolock=1 turns locking off, and SQLite then reports
// journal mode "delete". The working directory is a temp dir because the
// opener prepares the raw path string, which for a file: URI is a relative
// path (#648).
func TestOpenRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	t.Chdir(t.TempDir())
	s, err := Open(t.Context(), "file:"+path+"?nolock=1")
	if err == nil {
		_ = s.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
}
