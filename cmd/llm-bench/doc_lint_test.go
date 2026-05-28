package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docPath returns the absolute path of a docs/llm/<name> file relative
// to this test's package directory (cmd/llm-bench/). Tests run with cwd
// = package dir, so docs/llm lives two levels up.
func docPath(name string) string {
	return filepath.Join("..", "..", "docs", "llm", name)
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(docPath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestAnalysisMDHasHistoricalBanner(t *testing.T) {
	body := readDoc(t, "analysis.md")
	if !strings.HasPrefix(body, "> **Status — 2026-05-25**:") {
		first := body
		if len(first) > 80 {
			first = first[:80]
		}
		t.Fatalf("analysis.md must start with the historical banner; first 80 chars: %q", first)
	}
	if !strings.Contains(body, "[harness-results.md](harness-results.md)") {
		t.Errorf("analysis.md banner must link to harness-results.md")
	}
	if !strings.Contains(body, "[benchmarks/](benchmarks/)") {
		t.Errorf("analysis.md banner must link to benchmarks/")
	}
}

func TestHarnessResultsHasAcceptanceGate(t *testing.T) {
	body := readDoc(t, "harness-results.md")
	for _, must := range []string{
		"## Acceptance gate",
		"`dirty: no`",
		"Calibration PASS",
		"Judge cache hit-rate",
		"`ToolArgsValid` is `computed=true`",
		"manifest hash",
		"reviewed by the repo owner",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("harness-results.md missing required acceptance criterion fragment %q", must)
		}
	}
}

func TestRecommendationCitationOnceFirstRunAccepted(t *testing.T) {
	// Conditional check — runs only after the first accepted-run PR
	// replaces the "No accepted runs yet" placeholder in
	// harness-results.md. While scaffolding-only, this is a no-op.
	results := readDoc(t, "harness-results.md")
	if strings.Contains(results, "No accepted runs yet") {
		t.Skip("scaffolding state: no accepted run yet — recommendation lint deferred")
	}
	rec := readDoc(t, "recommendation.md")
	if !strings.Contains(rec, "[harness-results.md](harness-results.md)") {
		t.Errorf("recommendation.md must cite harness-results.md once a run is accepted")
	}
	// Once an accepted run exists, the "April 2026 analysis" framing
	// must only appear inside historical contexts, not as a current
	// citation.
	if strings.Count(rec, "April 2026 analysis") > 0 {
		// Tolerate inside a quote/blockquote, but reject as a top-level claim.
		// The cheapest reliable check: every occurrence must be on a line
		// that also contains "historical" or starts with ">".
		lines := strings.Split(rec, "\n")
		for _, line := range lines {
			if strings.Contains(line, "April 2026 analysis") &&
				!strings.Contains(line, "historical") &&
				!strings.HasPrefix(strings.TrimSpace(line), ">") {
				t.Errorf("recommendation.md still cites April 2026 analysis as current evidence: %q", line)
			}
		}
	}
}

func TestBenchmarksTemplateExists(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "llm", "benchmarks", "TEMPLATE.md"))
	if err != nil {
		t.Fatalf("read TEMPLATE.md: %v", err)
	}
	for _, must := range []string{
		"## Provenance",
		"## Calibration",
		"## Results",
		"## Conclusion",
		"## Caveats",
		"AnswerQuality (mean / p25 / p50 / p75 / p90)",
		"ToolArgsValid (computed=N)",
	} {
		if !strings.Contains(string(body), must) {
			t.Errorf("TEMPLATE.md missing required section/column fragment %q", must)
		}
	}
}
