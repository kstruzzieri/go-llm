package main

import (
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/completion"
)

// TestFixtureExpectationsMatchDetector validates that each bundled fixture's
// expected "context" string actually matches what completion.AnalyzeCursor
// produces on that fixture's prefix/suffix. Runs without Ollama and keeps
// the testdata honest when the detector evolves.
func TestFixtureExpectationsMatchDetector(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.txt")
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no testdata fixtures found")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := LoadFixture(path)
			if err != nil {
				t.Fatalf("LoadFixture: %v", err)
			}
			if f.Expect == nil || f.Expect.Context == "" {
				t.Skip("no context expectation to validate")
			}

			// Derive the language from the fixture's FilePath the same way the
			// Provider would, then run the detector.
			lang := languageFromFilePath(f.FilePath)
			analysis := completion.AnalyzeCursor(f.Prefix, f.Suffix, lang)
			if got := analysis.Context.String(); got != f.Expect.Context {
				t.Errorf("detector context=%s, fixture expects %s — fixture and detector have drifted",
					got, f.Expect.Context)
			}
		})
	}
}

// languageFromFilePath mirrors the extension-based detection the completion
// package uses internally. Kept minimal — only the languages our fixtures
// exercise need to map here.
func languageFromFilePath(filePath string) string {
	switch filepath.Ext(filePath) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	default:
		return ""
	}
}
