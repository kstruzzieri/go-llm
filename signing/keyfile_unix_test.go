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

func TestReadKeyFileDoesNotTreatNonENOENTLstatErrorAsAbsence(t *testing.T) {
	dir := secureKeyDir(t)
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, _, err := openKeyRoot(filepath.Join(dir, "unused.pem"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	// "plain" is a regular file, so lstat of a child path fails with ENOTDIR,
	// not ENOENT. Only ENOENT may be read as first-use absence; anything else
	// must surface, or a transient error would mint a replacement identity.
	raw, existed, err := readKeyFile(root, "plain/hmac.pem", nil)
	if err == nil || errors.Is(err, fs.ErrNotExist) || existed || raw != nil {
		t.Fatalf("ENOTDIR lstat = raw %v, existed %v, err %v; want a surfaced non-absence error", raw, existed, err)
	}
}

func TestKeyFileTempChmodRestoresOwnerBitsUnderStrictUmask(t *testing.T) {
	dir := secureKeyDir(t) // created before the umask change
	oldMask := syscall.Umask(0o277)
	t.Cleanup(func() { syscall.Umask(oldMask) })
	path := filepath.Join(dir, "hmac.pem")
	if _, _, err := LoadOrCreateHMAC(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode under umask 0277 = %04o, want 0600 (fchmod on the temp must restore owner write)", got)
	}
}

func TestKeyDirectoryCreationNeverChmodsThroughThePath(t *testing.T) {
	// os.Chmod(dir) after os.Mkdir follows a symlink an ancestor-writer can
	// swap in between the two calls, which is an arbitrary-path chmod as this
	// UID. The package therefore relies on Mkdir's mode alone; under a umask
	// that strips owner bits the directory is unusable and creation fails
	// closed rather than being "repaired" through the path.
	base := t.TempDir()
	oldMask := syscall.Umask(0o277)
	t.Cleanup(func() { syscall.Umask(oldMask) })
	keyDir := filepath.Join(base, "keys")
	_, created, err := LoadOrCreateHMAC(filepath.Join(keyDir, "hmac.pem"))
	fi, statErr := os.Stat(keyDir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	// The mode is the direct evidence: a path chmod would have made it 0700.
	if got := fi.Mode().Perm(); got != 0o500 {
		t.Fatalf("key directory mode = %04o, want 0500 (Mkdir 0700 under umask 0277, no path chmod)", got)
	}
	if os.Geteuid() == 0 {
		// root bypasses directory permission bits, so creation succeeds there;
		// the no-chmod property above is what matters.
		if err != nil || !created {
			t.Fatalf("as root = created %v, err %v; want a created key in the 0500 directory", created, err)
		}
		return
	}
	if err == nil || created {
		t.Fatalf("owner-stripping umask = created %v, err %v; want fail-closed error", created, err)
	}
}
