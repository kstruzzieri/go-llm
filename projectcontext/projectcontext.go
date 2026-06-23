package projectcontext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// defaultMaxBytes caps a single project-context file so a huge file cannot blow
// the consumer's context window.
const defaultMaxBytes = 64 * 1024

// defaultFilename is the conventional project-context filename.
const defaultFilename = "AGENTS.md"

// Document is one discovered project-context file.
type Document struct {
	Source    string // provenance label: "global" or "workspace"
	Path      string // absolute path it was read from
	Content   string // file bytes, possibly truncated to the loader's cap
	Truncated bool   // true if Content was capped
}

// Loader discovers and reads project-context files in deterministic order.
// Zero value is usable: it finds nothing. Set WorkspaceRoot and/or GlobalDir to
// enable discovery. Exported fields keep construction and testing trivial.
type Loader struct {
	// WorkspaceRoot is the directory whose root-level file is the most specific
	// (highest-precedence) document. Empty disables the workspace document.
	WorkspaceRoot string
	// GlobalDir is the directory whose file is the least specific
	// (lowest-precedence) document. Empty disables the global document.
	GlobalDir string
	// Filenames is the ordered candidate filename list; the first that exists in
	// a location wins for that location. Empty defaults to {"AGENTS.md"}.
	// Entries must be simple base names: empty, absolute, NUL-containing, "."/"..",
	// and path-separator-containing entries are ignored.
	Filenames []string
	// MaxBytes caps a single file. <= 0 defaults to 64 KiB.
	MaxBytes int
}

// Load returns documents in low→high precedence order (global first, workspace
// last). Missing files are skipped (not an error). Returns nil, nil when nothing
// is found or nothing is configured.
func (l *Loader) Load(ctx context.Context) ([]Document, error) {
	maxBytes := l.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	names := candidateNames(l.Filenames)
	if len(names) == 0 {
		return nil, nil
	}

	var docs []Document
	// Order is significant: global (least specific) first, workspace last.
	locations := []struct {
		source string
		dir    string
	}{
		{"global", l.GlobalDir},
		{"workspace", l.WorkspaceRoot},
	}
	for _, loc := range locations {
		if loc.dir == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		doc, found, err := loadOne(loc.source, loc.dir, names, maxBytes)
		if err != nil {
			return nil, err
		}
		if found {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

func candidateNames(in []string) []string {
	if len(in) == 0 {
		in = []string{defaultFilename}
	}
	out := make([]string, 0, len(in))
	for _, name := range in {
		if name == "" || name == "." || name == ".." || filepath.IsAbs(name) ||
			strings.IndexByte(name, 0) >= 0 || strings.ContainsAny(name, `/\`) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func canonicalDir(dir string) (string, bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	fi, err := os.Lstat(canon)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !fi.IsDir() {
		return "", false, nil
	}
	return canon, true, nil
}

// loadOne reads the first existing candidate filename in dir, returning that
// Document. found=false means no candidate existed (or all were unsafe/absent).
func loadOne(source, dir string, names []string, maxBytes int) (Document, bool, error) {
	root, ok, err := canonicalDir(dir)
	if err != nil || !ok {
		return Document{}, false, err
	}
	for _, name := range names {
		path := filepath.Join(root, name)
		content, truncated, found, err := readCapped(path, maxBytes)
		if err != nil {
			return Document{}, false, err
		}
		if found {
			return Document{
				Source:    source,
				Path:      path,
				Content:   content,
				Truncated: truncated,
			}, true, nil
		}
	}
	return Document{}, false, nil
}
