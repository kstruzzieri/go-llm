package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitCursor(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		wantPrefix string
		wantSuffix string
		wantErr    bool
	}{
		{
			name:       "basic split",
			source:     "before<CURSOR>after",
			wantPrefix: "before",
			wantSuffix: "after",
		},
		{
			name:       "empty prefix",
			source:     "<CURSOR>after",
			wantPrefix: "",
			wantSuffix: "after",
		},
		{
			name:       "empty suffix",
			source:     "before<CURSOR>",
			wantPrefix: "before",
			wantSuffix: "",
		},
		{
			name:    "missing marker",
			source:  "no cursor here",
			wantErr: true,
		},
		{
			name:    "multiple markers rejected",
			source:  "a<CURSOR>b<CURSOR>c",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, suffix, err := splitCursor(tt.source)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if prefix != tt.wantPrefix {
				t.Errorf("prefix=%q want %q", prefix, tt.wantPrefix)
			}
			if suffix != tt.wantSuffix {
				t.Errorf("suffix=%q want %q", suffix, tt.wantSuffix)
			}
		})
	}
}

func TestDeriveFilePath(t *testing.T) {
	tests := []struct {
		fixturePath string
		want        string
	}{
		{"testdata/go_between_decls.txt", "between_decls.go"},
		{"testdata/python_comment_block.txt", "comment_block.py"},
		{"testdata/typescript_after_brace.txt", "after_brace.ts"},
		{"testdata/nolang.txt", "nolang"},                     // no underscore: return stem
		{"testdata/unknown_lang_foo.txt", "unknown_lang_foo"}, // unknown lang prefix: no ext
	}
	for _, tt := range tests {
		t.Run(tt.fixturePath, func(t *testing.T) {
			got := deriveFilePath(tt.fixturePath)
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestLoadFixtureWithSidecar(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "go_example.txt")
	sidecarPath := filepath.Join(dir, "go_example.expect.json")

	if err := os.WriteFile(fixturePath, []byte("before<CURSOR>after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte(`{
		"context": "function_body",
		"shape": ["block", "declaration"],
		"min_prefix_pct": 75,
		"max_prefix_pct": 95,
		"min_tokens": 1,
		"no_stop_leak": true
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if f.Prefix != "before" || f.Suffix != "after" {
		t.Errorf("prefix/suffix = %q/%q", f.Prefix, f.Suffix)
	}
	if f.FilePath != "example.go" {
		t.Errorf("FilePath = %q want example.go", f.FilePath)
	}
	if f.Expect == nil {
		t.Fatal("Expect is nil")
	}
	if f.Expect.Context != "function_body" {
		t.Errorf("Context = %q", f.Expect.Context)
	}
	if len(f.Expect.Shape) != 2 {
		t.Errorf("Shape len = %d", len(f.Expect.Shape))
	}
	if !f.Expect.NoStopLeak {
		t.Error("NoStopLeak should be true")
	}
}

func TestLoadFixtureNoSidecar(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "go_bare.txt")
	if err := os.WriteFile(fixturePath, []byte("x<CURSOR>y"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if f.Expect != nil {
		t.Errorf("Expect should be nil when sidecar absent, got %+v", f.Expect)
	}
}

func TestLoadFixtureSidecarFilePathOverride(t *testing.T) {
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "custom.txt")
	sidecarPath := filepath.Join(dir, "custom.expect.json")
	_ = os.WriteFile(fixturePath, []byte("a<CURSOR>b"), 0o644)
	_ = os.WriteFile(sidecarPath, []byte(`{"file_path": "manual.rs"}`), 0o644)

	f, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if f.FilePath != "manual.rs" {
		t.Errorf("FilePath = %q want manual.rs (sidecar override)", f.FilePath)
	}
}
