//go:build !linux && !darwin

package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildOutputScreenOtherPlatform(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=other-0001-abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := buildOutputScreen(t.Context(), ws)
	if err != nil || s == nil {
		t.Fatalf("other: build failed (error: %t)", err != nil)
	}
	var buf bytes.Buffer
	w := s.stream(&buf)
	_, _ = w.Write([]byte("hello\n"))
	w.finish()
	if buf.Len() != 0 {
		t.Fatalf("other: output delivered (len %d)", buf.Len())
	}
	want := "credential screen: output withheld; credential files could not be read completely (no hardened workspace walk on this platform)"
	if notes := s.notes(); len(notes) != 1 || notes[0] != want {
		t.Fatalf("other: notes differ (%d notes)", len(notes))
	}
}
