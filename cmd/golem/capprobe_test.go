package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

func TestCapProbeStorePath_UsesDataDir(t *testing.T) {
	base := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	p, err := capProbeStorePath(getenv)
	if err != nil {
		t.Fatalf("capProbeStorePath: %v", err)
	}
	if want := filepath.Join(base, "golem", "fingerprints.db"); p != want {
		t.Errorf("path = %q, want %q", p, want)
	}
}

func TestOpenCapProbeStore_CreatesHardenedFile(t *testing.T) {
	base := t.TempDir()
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	h, warn := openCapProbeStore(context.Background(), getenv, root)
	if h == nil || h.store == nil || h.db == nil {
		t.Fatalf("want non-nil handle, got %#v (warn=%q)", h, warn)
	}
	defer func() { _ = h.db.Close() }()
	if warn != "" {
		t.Errorf("unexpected warn: %q", warn)
	}
	dbPath := filepath.Join(base, "golem", "fingerprints.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("cap-probe DB not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cap-probe DB mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("cap-probe DB dir not created: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("cap-probe DB dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
	assertDBFilesSecured(t, dbPath)
}

// assertDBFilesSecured checks the DB file and any present -wal/-shm sidecars
// are 0600. Missing sidecars are fine (WAL may not have spilled yet).
func assertDBFilesSecured(t *testing.T, dbPath string) {
	t.Helper()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("stat %q: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%q mode = %o, want 0600", p, info.Mode().Perm())
		}
	}
}

func TestOpenCapProbeStore_SaveCapProbeSecuresSidecars(t *testing.T) {
	// SaveCapProbe runs mid-session (route-time probes). A WAL checkpoint can
	// recreate a -wal/-shm sidecar at the umask, not 0600. The store decorator
	// must re-chmod after every write.
	base := t.TempDir()
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	ctx := context.Background()
	h, warn := openCapProbeStore(ctx, getenv, root)
	if h == nil || h.store == nil {
		t.Fatalf("want non-nil handle, got %#v (warn=%q)", h, warn)
	}
	defer func() { _ = h.db.Close() }()

	dbPath := filepath.Join(base, "golem", "fingerprints.db")
	// Loosen the sidecars before the write to prove the decorator re-secures
	// them: if it did not run, they would stay 0666 after SaveCapProbe.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(dbPath+suffix, 0o666); err != nil && !os.IsNotExist(err) {
			t.Fatalf("loosen %s: %v", suffix, err)
		}
	}
	row := fingerprint.CapProbe{
		BackendID:    "lc",
		ModelName:    "m",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeYes,
		ModelDigest:  "d",
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     time.Now(),
	}
	if err := h.store.SaveCapProbe(ctx, row); err != nil {
		t.Fatalf("SaveCapProbe: %v", err)
	}
	assertDBFilesSecured(t, dbPath)
}

func TestOpenCapProbeStore_FallsBackToMemoryOnFailure(t *testing.T) {
	// XDG_DATA_HOME points at a regular file, so creating <base>/golem fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return blocker
		}
		return ""
	}
	ctx := context.Background()
	h, warn := openCapProbeStore(ctx, getenv, t.TempDir())
	if h == nil || h.store == nil {
		t.Fatalf("want in-memory fallback store, got nil (warn=%q)", warn)
	}
	defer func() { _ = h.db.Close() }()
	if warn == "" {
		t.Errorf("want a degradation warning")
	}
	// The fallback store must actually work for this run: save + read back.
	row := fingerprint.CapProbe{
		BackendID:    "lc",
		ModelName:    "m",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeYes,
		ModelDigest:  "d",
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     time.Now(),
	}
	if err := h.store.SaveCapProbe(ctx, row); err != nil {
		t.Fatalf("SaveCapProbe on fallback store: %v", err)
	}
	got, err := h.store.GetCapProbe(ctx, "lc", "m", "tool_call")
	if err != nil {
		t.Fatalf("GetCapProbe on fallback store: %v", err)
	}
	if got == nil || got.State != fingerprint.CapProbeYes {
		t.Fatalf("GetCapProbe = %#v, want yes row", got)
	}
}
