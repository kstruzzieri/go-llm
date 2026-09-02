//go:build unix

package signing

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestKeyFileModesOnCreate(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "keys", "agent.pem")
	if _, _, err := LoadOrCreateEd25519(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode = %04o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("key dir mode = %04o, want 0700", got)
	}
}

func TestKeyFileRejectsLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "agent.pem")
	if _, _, err := LoadOrCreateEd25519(path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []fs.FileMode{0o644, 0o640, 0o604, 0o660, 0o606} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreateEd25519(path); !errors.Is(err, ErrInsecureKeyFile) {
			t.Errorf("mode %04o: err = %v, want ErrInsecureKeyFile", mode, err)
		}
	}
	for _, mode := range []fs.FileMode{0o600, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreateEd25519(path); err != nil {
			t.Errorf("mode %04o: unexpected error %v", mode, err)
		}
	}
}

func TestKeyDirectoryRejectsLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateHMAC(filepath.Join(dir, "hmac.pem")); !errors.Is(err, ErrInsecureKeyDirectory) {
		t.Fatalf("loose key directory: err = %v, want ErrInsecureKeyDirectory", err)
	}
}

type fileInfoWithStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (f fileInfoWithStat) Sys() any { return f.stat }

func TestOwnerAndModeOKRejectsForeignOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("file info has no syscall.Stat_t")
	}
	foreign := *stat
	foreign.Uid++
	if ownerAndModeOK(fileInfoWithStat{FileInfo: info, stat: &foreign}) {
		t.Fatal("foreign-owned 0600 file accepted")
	}
}
