package memory

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

func fileURI(name string) string { return "file:" + name }

// TestOpenHardenedDBHardensTheFileSQLiteOpens: OpenHardenedDB backs the CLI
// memory store, capability probes, checkpoints and Golem's thread stores; the
// 0600 promise must cover the file SQLite opens for every input form (#648).
func TestOpenHardenedDBHardensTheFileSQLiteOpens(t *testing.T) {
	sqlitetest.RunHardenedPaths(t, sqlitetest.HardenedPathsOptions{
		URI:      fileURI,
		MemoryOK: true,
		Open: func(ctx context.Context, input string) (io.Closer, error) {
			return OpenHardenedDB(ctx, input)
		},
	})
}

// TestOpenRecordStoreHardensTheFileSQLiteOpens: the record store keeps its
// signing keys beside the database, so an input naming no file is refused
// unless the caller says where the keys live (#648).
func TestOpenRecordStoreHardensTheFileSQLiteOpens(t *testing.T) {
	sqlitetest.RunHardenedPaths(t, sqlitetest.HardenedPathsOptions{
		URI:      fileURI,
		MemoryOK: false,
		Open: func(ctx context.Context, input string) (io.Closer, error) {
			return OpenRecordStore(ctx, input, RecordStoreConfig{})
		},
	})
}

// TestOpenRecordStoreKeysBesideTheFileSQLiteOpens: the default key directory
// is derived from the database SQLite opens, so a file: URI no longer puts the
// private key under a directory named after the raw URI (#648).
func TestOpenRecordStoreKeysBesideTheFileSQLiteOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	cwd := t.TempDir()
	t.Chdir(cwd)
	rt, err := OpenRecordStore(context.Background(), fileURI(filepath.ToSlash(path)), RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rt.Close() }()
	if _, err := os.Stat(path + ".keys/current.pem"); err != nil {
		t.Fatalf("signing key not beside the database: %v", err)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory after the open: %v, %v; want it empty", entries, err)
	}
}

// TestOpenRecordStoreRefusesLegacyKeyDir: before #648 a file:-URI record
// store kept its keys in a directory named after the raw URI, relative to
// the working directory. Reopening such a store must not create a second
// identity beside the database: it fails and names the move to make.
func TestOpenRecordStoreRefusesLegacyKeyDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	cwd := t.TempDir()
	t.Chdir(cwd)
	input := fileURI(filepath.ToSlash(path))
	legacy := input + ".keys"
	if err := os.MkdirAll(filepath.Join(legacy, "trusted"), 0o700); err != nil {
		t.Skipf("cannot create the legacy key directory name here: %v", err)
	}
	_, err := OpenRecordStore(context.Background(), input, RecordStoreConfig{})
	if err == nil || !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), path+".keys") {
		t.Fatalf("OpenRecordStore = %v, want an error naming %q and %q", err, legacy, path+".keys")
	}
	if _, err := os.Stat(path + ".keys"); !os.IsNotExist(err) {
		t.Fatalf("a new key directory was created beside the database: %v", err)
	}
}

// TestOpenHardenedDBRejectsNonWAL: the doc's promise, previously untested for
// this opener (#648 investigation).
func TestOpenHardenedDBRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	cwd := t.TempDir()
	t.Chdir(cwd)
	db, err := OpenHardenedDB(t.Context(), fileURI(path)+"?nolock=1")
	if err == nil {
		_ = db.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory after the open: %v, %v; want it empty", entries, err)
	}
}

// TestPrepareAndSecureResolveTheirInput: callers outside this package
// (Golem's session and checkpoint stores, MCP) prepare and secure by the path
// they opened with, so both helpers must act on the file SQLite opens for it.
func TestPrepareAndSecureResolveTheirInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not portable on Windows")
	}
	path := filepath.Join(t.TempDir(), "m.db")
	cwd := t.TempDir()
	t.Chdir(cwd)
	input := fileURI(filepath.ToSlash(path))
	if err := PrepareDBFile(input); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != dbFileMode {
		t.Fatalf("PrepareDBFile(%q): %s = %v, %v; want it created 0600", input, path, info, err)
	}
	for _, p := range []string{path, path + "-wal"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SecureDBFiles(input); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != dbFileMode {
			t.Fatalf("SecureDBFiles(%q): %s = %v, %v; want 0600", input, p, info, err)
		}
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory after prepare and secure: %v, %v; want it empty", entries, err)
	}
}
