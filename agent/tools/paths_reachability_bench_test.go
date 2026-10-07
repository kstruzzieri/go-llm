//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func benchWrite(b *testing.B, path, body string) {
	b.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkReachabilityReadDepth5 measures one point read five components
// deep with 20 siblings per level (#613 budget: <= +25% versus the base).
func BenchmarkReachabilityReadDepth5(b *testing.B) {
	root := b.TempDir()
	rel := filepath.Join("a", "b", "c", "d", "file.txt")
	benchWrite(b, filepath.Join(root, rel), "x\n")
	for _, dir := range []string{"", "a", "a/b", "a/b/c", "a/b/c/d"} {
		for i := 0; i < 20; i++ {
			benchWrite(b, filepath.Join(root, dir, fmt.Sprintf("sib%02d", i)), "s")
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := ws.readAll(rel); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReachabilitySearch2000 measures a no-match search over 2000 files
// three levels deep (#613 budget: <= +50% versus the base on Darwin).
func BenchmarkReachabilitySearch2000(b *testing.B) {
	root := b.TempDir()
	body := strings.Repeat("package x // filler line\n", 40)
	for a := 0; a < 10; a++ {
		for c := 0; c < 10; c++ {
			for f := 0; f < 20; f++ {
				benchWrite(b, filepath.Join(root, fmt.Sprintf("a%d", a), fmt.Sprintf("b%d", c), fmt.Sprintf("f%d.go", f)), body)
			}
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		b.Fatal(err)
	}
	s := NewSearch(ws)
	raw := json.RawMessage(`{"pattern":"NOMATCHXYZ"}`)
	for b.Loop() {
		if res, err := s.Invoke(context.Background(), raw); err != nil || res.IsError || res.Content != "no matches" || res.Truncated {
			b.Fatalf("search: %+v, %v", res, err)
		}
	}
}

// BenchmarkReachabilitySearchWide exposes repeated enumeration of one large
// directory. Compare its scaling before/after; a fixed-size timing alone does
// not establish algorithmic complexity.
func BenchmarkReachabilitySearchWide(b *testing.B) {
	for _, files := range []int{1000, 2000, 4000} {
		b.Run(fmt.Sprintf("files_%d", files), func(b *testing.B) {
			root := b.TempDir()
			for i := 0; i < files; i++ {
				benchWrite(b, filepath.Join(root, fmt.Sprintf("f%04d.go", i)), "package x\n")
			}
			ws, err := NewWorkspace(root)
			if err != nil {
				b.Fatal(err)
			}
			s := NewSearch(ws)
			raw := json.RawMessage(`{"pattern":"NOMATCHXYZ"}`)
			for b.Loop() {
				if res, err := s.Invoke(context.Background(), raw); err != nil || res.IsError || res.Content != "no matches" || res.Truncated {
					b.Fatalf("search: %+v, %v", res, err)
				}
			}
		})
	}
}
