package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/projectcontext"
)

// agentsBudgetPathLen is the length in bytes of the absolute AGENTS.md path the
// budget test assumes. The rendered header carries the document's path, so the
// test renders under a fixed ASCII path of this length rather than the
// checkout's real one, and gives the same result for the same AGENTS.md bytes
// wherever the checkout lives. A real path that renders longer (deeper, or
// with multi-byte or escaped characters) leaves less room than the test
// checks.
const agentsBudgetPathLen = 256

// TestRepoAGENTSFitsWorstCaseBudget keeps this repository's own AGENTS.md whole
// when Golem works on go-llm. Project context shares projectContextMaxBytes with
// a Git snapshot of up to gitContextMaxBytes, and an over-budget document keeps
// its prefix, so growth past the worst case silently drops the last sections.
// The worst case also has a global AGENTS.md too large to fit: the workspace
// document is placed first, and the notice that the global one was omitted
// still takes bytes from the budget.
func TestRepoAGENTSFitsWorstCaseBudget(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	const name = "/AGENTS.md"
	path := "/" + strings.Repeat("p", agentsBudgetPathLen-1-len(name)) + name
	docs := []projectcontext.Document{
		{Source: "global", Path: "/home/user/.config/golem/AGENTS.md", Content: strings.Repeat("g", projectContextMaxBytes)},
		{Source: "workspace", Path: path, Content: string(b)},
	}
	budget := projectContextMaxBytes - gitContextMaxBytes
	_, _, statuses := renderProjectContext(promptfence.New(), docs, budget)
	if statuses[0] != projectContextOmitted {
		t.Fatalf("the oversized global document is %s, not omitted; the test no longer covers the omission notice's cost", statuses[0])
	}
	if statuses[1] != projectContextRetained {
		t.Fatalf("AGENTS.md (%d bytes) is %s under a %d-byte ASCII path when the Git snapshot uses its full %d bytes and a global AGENTS.md is omitted; trim it to fit the %d-byte remainder (header and omission notice included)",
			len(b), statuses[1], len(path), gitContextMaxBytes, budget)
	}
}
