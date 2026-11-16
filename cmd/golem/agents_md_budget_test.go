package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/projectcontext"
)

// TestRepoAGENTSFitsWorstCaseBudget keeps this repository's own AGENTS.md whole
// when Golem works on go-llm. Project context shares projectContextMaxBytes with
// a Git snapshot of up to gitContextMaxBytes, and an over-budget document keeps
// its prefix, so growth past the worst case silently drops the last sections.
func TestRepoAGENTSFitsWorstCaseBudget(t *testing.T) {
	t.Parallel()
	path, err := filepath.Abs(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := projectcontext.Document{Source: "workspace", Path: path, Content: string(b)}
	budget := projectContextMaxBytes - gitContextMaxBytes
	_, _, statuses := renderProjectContext(promptfence.New(), []projectcontext.Document{doc}, budget)
	if statuses[0] != projectContextRetained {
		t.Fatalf("AGENTS.md (%d bytes) is %s when the Git snapshot uses its full %d bytes; trim it to fit the %d-byte remainder (header included)",
			len(b), statuses[0], gitContextMaxBytes, budget)
	}
}
