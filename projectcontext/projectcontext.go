package projectcontext

import "context"

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
	return nil, nil
}
