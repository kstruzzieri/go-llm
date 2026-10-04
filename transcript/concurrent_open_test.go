package transcript

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != transcriptFileMode {
		t.Fatalf("db mode = %v, want %v", got, transcriptFileMode)
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
