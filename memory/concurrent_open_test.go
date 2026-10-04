package memory

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestOpenHardenedDBConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		return OpenHardenedDB(ctx, path)
	})
}

// Another connection's last close unlinks the WAL sidecars; when that lands
// between a sidecar's Stat and its Chmod, securing must treat it as absent.
func TestSecureDBFilesSkipsSidecarRemovedAfterStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
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
	if err := secureDBFilesWith(path, chmod); err != nil {
		t.Fatalf("secureDBFilesWith = %v, want nil for sidecars removed after Stat", err)
	}
	// File mode bits are not portable on Windows.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != dbFileMode {
			t.Fatalf("db mode = %v, want %v", got, os.FileMode(dbFileMode))
		}
	}
}

// A sidecar that vanishes must not stop the others from being secured: with
// only -wal removed after its Stat, a loose -shm must still end up 0600.
func TestSecureDBFilesKeepsGoingAfterVanishedSidecar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not portable on Windows")
	}
	path := filepath.Join(t.TempDir(), "m.db")
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chmod := func(p string, mode os.FileMode) error {
		if p == path+"-wal" {
			_ = os.Remove(p)
		}
		return os.Chmod(p, mode)
	}
	if err := secureDBFilesWith(path, chmod); err != nil {
		t.Fatalf("secureDBFilesWith = %v, want nil", err)
	}
	info, err := os.Stat(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != dbFileMode {
		t.Fatalf("-shm mode = %v, want %v", got, os.FileMode(dbFileMode))
	}
}

// Only a vanished file is tolerated: any other chmod error must fail, or the
// database could stay readable by others.
func TestSecureDBFilesReportsChmodError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	chmod := func(p string, _ os.FileMode) error {
		return &fs.PathError{Op: "chmod", Path: p, Err: fs.ErrPermission}
	}
	if err := secureDBFilesWith(path, chmod); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("secureDBFilesWith = %v, want an fs.ErrPermission error", err)
	}
}

// A directory where a sidecar belongs is an error, not something to chmod.
func TestSecureDBFilesRejectsSidecarDirectory(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path+suffix, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := SecureDBFiles(path); err == nil {
				t.Fatalf("SecureDBFiles = nil with a directory at %s, want an error", suffix)
			}
		})
	}
}
