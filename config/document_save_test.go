package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishReplaceWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := publishReplace(p, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp residue: %v", entries)
	}
}

// Create-only publication: existing target survives untouched.
func TestPublishNewRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishNew(p, []byte("clobber"))
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != "original" {
		t.Fatal("existing target was modified")
	}
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) != 1 {
		t.Fatalf("temp residue after refusal: %v", entries)
	}
}

// Post-rename dir-sync failure = published but durability uncertain: typed
// error, bytes live.
func TestPublishDurabilityUncertain(t *testing.T) {
	orig := syncDir
	syncDir = func(string) error { return errors.New("injected dir-sync failure") }
	t.Cleanup(func() { syncDir = orig })

	p := filepath.Join(t.TempDir(), "out.json")
	err := publishReplace(p, []byte("data\n"))
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v, want ErrDurabilityUncertain", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != "data\n" {
		t.Fatal("bytes were not published despite post-rename phase")
	}
}
