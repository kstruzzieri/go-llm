package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/projectcontext"
)

// agentsBudgetPathLen is the length of the absolute AGENTS.md path the budget
// test assumes. The rendered header carries the document's path, so the test
// renders under a fixed path of this length rather than the checkout's real
// one: the result is the same on every machine, and a checkout whose AGENTS.md
// path is at most this long keeps the whole file.
const agentsBudgetPathLen = 256

// TestRepoAGENTSFitsWorstCaseBudget keeps this repository's own AGENTS.md whole
// when Golem works on go-llm. Project context shares projectContextMaxBytes with
// a Git snapshot of up to gitContextMaxBytes, and an over-budget document keeps
// its prefix, so growth past the worst case silently drops the last sections.
func TestRepoAGENTSFitsWorstCaseBudget(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	const name = "/AGENTS.md"
	path := "/" + strings.Repeat("p", agentsBudgetPathLen-1-len(name)) + name
	doc := projectcontext.Document{Source: "workspace", Path: path, Content: string(b)}
	budget := projectContextMaxBytes - gitContextMaxBytes
	_, _, statuses := renderProjectContext(promptfence.New(), []projectcontext.Document{doc}, budget)
	if statuses[0] != projectContextRetained {
		t.Fatalf("AGENTS.md (%d bytes) is %s under a %d-character path when the Git snapshot uses its full %d bytes; trim it to fit the %d-byte remainder (header included)",
			len(b), statuses[0], len(path), gitContextMaxBytes, budget)
	}
}
