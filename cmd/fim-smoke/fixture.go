// Command fim-smoke runs a FIM completion against a local Ollama instance
// to smoke-test the completion provider end-to-end. It supports an
// eyeball-inspection mode that prints the budget trace and output, and a
// -check mode that asserts structural invariants against sidecar
// expectations without requiring exact text matches.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cursorMarker is the sentinel in fixture source files that splits prefix
// from suffix. It must appear exactly once per fixture.
const cursorMarker = "<CURSOR>"

// Fixture represents a single FIM smoke-test scenario.
type Fixture struct {
	Path     string  // absolute path to the .txt file
	Prefix   string  // source before the cursor marker
	Suffix   string  // source after the cursor marker
	FilePath string  // synthetic file path used for language detection
	Expect   *Expect // nil when no sidecar .expect.json present
}

// Expect carries the structural invariants a fixture is expected to satisfy.
// Fields with zero values are treated as "no assertion for this field".
type Expect struct {
	// FilePath overrides the fixture-name-derived file path.
	FilePath string `json:"file_path,omitempty"`
	// Context is the expected CursorContext string (e.g. "function_body").
	Context string `json:"context,omitempty"`
	// Shape lists acceptable CompletionShape strings; empty means any.
	Shape []string `json:"shape,omitempty"`
	// MinPrefixPct / MaxPrefixPct bound the adaptive prefix-budget percentage.
	MinPrefixPct int `json:"min_prefix_pct,omitempty"`
	MaxPrefixPct int `json:"max_prefix_pct,omitempty"`
	// MinTokens asserts the completion returned at least this many tokens.
	MinTokens int `json:"min_tokens,omitempty"`
	// NoStopLeak asserts no effective stop token appears anywhere in the output.
	NoStopLeak bool `json:"no_stop_leak,omitempty"`
}

// LoadFixture reads a fixture and optional sidecar from disk.
func LoadFixture(path string) (*Fixture, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}
	prefix, suffix, err := splitCursor(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	f := &Fixture{
		Path:     abs,
		Prefix:   prefix,
		Suffix:   suffix,
		FilePath: deriveFilePath(abs),
	}

	sidecar := sidecarPath(abs)
	if data, err := os.ReadFile(sidecar); err == nil {
		var e Expect
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("%s: parse sidecar: %w", sidecar, err)
		}
		f.Expect = &e
		if e.FilePath != "" {
			f.FilePath = e.FilePath
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: read sidecar: %w", sidecar, err)
	}

	return f, nil
}

// splitCursor splits source on the cursor marker. Returns an error when the
// marker is missing or present more than once.
func splitCursor(source string) (prefix, suffix string, err error) {
	idx := strings.Index(source, cursorMarker)
	if idx < 0 {
		return "", "", fmt.Errorf("fixture missing %s marker", cursorMarker)
	}
	rest := source[idx+len(cursorMarker):]
	if strings.Contains(rest, cursorMarker) {
		return "", "", fmt.Errorf("fixture contains multiple %s markers", cursorMarker)
	}
	return source[:idx], rest, nil
}

// deriveFilePath converts a fixture path like ".../go_between_decls.txt" into
// a synthetic source path like "between_decls.go" that the language detector
// can recognize. The convention is <lang>_<anything>.txt → <anything>.<ext>.
func deriveFilePath(fixturePath string) string {
	name := strings.TrimSuffix(filepath.Base(fixturePath), ".txt")
	underscore := strings.Index(name, "_")
	if underscore < 0 {
		return name
	}
	lang := name[:underscore]
	rest := name[underscore+1:]
	ext, ok := langToExt[lang]
	if !ok {
		return name
	}
	return rest + ext
}

// langToExt maps fixture-name language prefixes to the file extension the
// completion package's language detector recognizes.
var langToExt = map[string]string{
	"go":         ".go",
	"python":     ".py",
	"typescript": ".ts",
	"javascript": ".js",
	"rust":       ".rs",
	"java":       ".java",
	"c":          ".c",
	"cpp":        ".cpp",
	"ruby":       ".rb",
	"yaml":       ".yaml",
	"json":       ".json",
	"sql":        ".sql",
}

// sidecarPath returns the expected sidecar path for a fixture:
// "foo.txt" → "foo.expect.json".
func sidecarPath(fixturePath string) string {
	return strings.TrimSuffix(fixturePath, ".txt") + ".expect.json"
}
