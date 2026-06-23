package projectcontext

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCappedReadsRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte("hello context"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, truncated, found, err := readCapped(p, defaultMaxBytes)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if !found {
		t.Fatal("readCapped: want found=true")
	}
	if truncated {
		t.Fatal("readCapped: want truncated=false")
	}
	if content != "hello context" {
		t.Fatalf("readCapped: content=%q", content)
	}
}

func TestReadCappedMissingFileNotFound(t *testing.T) {
	_, _, found, err := readCapped(filepath.Join(t.TempDir(), "nope.md"), defaultMaxBytes)
	if err != nil {
		t.Fatalf("readCapped: want nil error for missing file, got %v", err)
	}
	if found {
		t.Fatal("readCapped: want found=false for missing file")
	}
}
