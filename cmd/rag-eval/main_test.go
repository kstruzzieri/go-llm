package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/rageval"
)

func TestRunDefaultsToBaseline(t *testing.T) {
	out := filepath.Join(t.TempDir(), "baseline.json")
	if err := run([]string{
		"-fixtures", "../../internal/rageval/testdata/fixtures.json",
		"-out", out,
		"-no-latency",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../internal/rageval/testdata/baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("default baseline output differs from committed baseline")
	}
}

func TestRunOutlineExperiment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "outline.json")
	if err := run([]string{
		"-experiment", "outline",
		"-dimensions", "128",
		"-candidate-m", "20",
		"-out", out,
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report rageval.OutlineReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != rageval.OutlineSchemaVersion {
		t.Fatalf("schema_version = %q", report.SchemaVersion)
	}
	if report.Corpus.Dimensions != 128 || report.Corpus.CandidateLimit != 20 || report.Corpus.Samples != 5 || report.Corpus.Queries != 20 {
		t.Fatalf("corpus options = %+v", report.Corpus)
	}
	wantModes := []string{
		"full_corpus_search_multi",
		"resident_exact",
		"bounded_semantic_keyword_union",
		"outline_then_content",
		"hierarchical",
	}
	if len(report.Modes) != len(wantModes) {
		t.Fatalf("mode count = %d, want %d", len(report.Modes), len(wantModes))
	}
	for i, want := range wantModes {
		if report.Modes[i].Name != want {
			t.Fatalf("mode[%d] = %q, want %q", i, report.Modes[i].Name, want)
		}
		if len(report.Modes[i].Queries) != 20 {
			t.Fatalf("%s query count = %d, want 20", want, len(report.Modes[i].Queries))
		}
	}
}

func TestRunOutlineRequiresExplicitOutput(t *testing.T) {
	err := run([]string{
		"-experiment", "outline",
		"-dimensions", "128",
		"-candidate-m", "20",
		"-samples", "1",
	})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit -out") {
		t.Fatalf("error = %v, want explicit output requirement", err)
	}
}

func TestRunBaselineRequiresExplicitOutput(t *testing.T) {
	err := run([]string{
		"-fixtures", "../../internal/rageval/testdata/fixtures.json",
		"-no-latency",
	})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit -out") {
		t.Fatalf("error = %v, want explicit output requirement", err)
	}
}

func TestRunOutlineHonorsSamplesFlag(t *testing.T) {
	out := filepath.Join(t.TempDir(), "outline.json")
	if err := run([]string{
		"-experiment", "outline",
		"-dimensions", "16",
		"-candidate-m", "20",
		"-samples", "1",
		"-out", out,
	}); err != nil {
		t.Fatal(err)
	}
	if got := readOutlineSamples(t, out); got != 1 {
		t.Fatalf("samples = %d, want explicit 1", got)
	}
}

// TestRunOutlineWarmRunsAliasesSamples locks the deprecated -warm-runs alias:
// in outline mode it still selects the measured sample count when -samples is
// absent, so pre-split invocations keep reproducing.
func TestRunOutlineWarmRunsAliasesSamples(t *testing.T) {
	out := filepath.Join(t.TempDir(), "outline.json")
	if err := run([]string{
		"-experiment", "outline",
		"-dimensions", "16",
		"-candidate-m", "20",
		"-warm-runs", "1",
		"-out", out,
	}); err != nil {
		t.Fatal(err)
	}
	if got := readOutlineSamples(t, out); got != 1 {
		t.Fatalf("samples = %d, want alias 1", got)
	}
}

// TestRunOutlineRejectsNonPositiveSamples locks the fix for the P2 review: an
// explicit non-positive outline sample count (via -samples or the -warm-runs
// alias) must error, not be silently replaced with the default.
func TestRunOutlineRejectsNonPositiveSamples(t *testing.T) {
	for _, flagName := range []string{"-samples", "-warm-runs"} {
		out := filepath.Join(t.TempDir(), "outline.json")
		err := run([]string{
			"-experiment", "outline",
			"-dimensions", "16",
			"-candidate-m", "20",
			flagName, "0",
			"-out", out,
		})
		if err == nil || !strings.Contains(err.Error(), "sample count must be positive") {
			t.Fatalf("%s 0: error = %v, want positive-sample requirement", flagName, err)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Fatalf("%s 0: output exists after rejected run: %v", flagName, statErr)
		}
	}
}

func readOutlineSamples(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report rageval.OutlineReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report.Corpus.Samples
}

func TestRunHelpSucceeds(t *testing.T) {
	if err := run([]string{"-h"}); err != nil {
		t.Fatalf("run -h: %v", err)
	}
}

func TestRunRejectsUnknownExperiment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "unknown.json")
	err := run([]string{"-experiment", "unknown", "-out", out})
	if err == nil || !strings.Contains(err.Error(), `unknown experiment "unknown"`) {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("output exists after rejected experiment: %v", statErr)
	}
}

func TestProgressiveExperimentRequiresOut(t *testing.T) {
	err := run([]string{"-experiment", "progressive"})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit -out") {
		t.Fatalf("error = %v, want explicit output requirement", err)
	}
}

func TestRunProgressiveExperiment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "progressive.json")
	if err := run([]string{
		"-experiment", "progressive",
		"-dimensions", "16",
		"-out", out,
	}); err != nil {
		t.Fatal(err)
	}
	var report rageval.ProgressiveReport
	if err := json.Unmarshal(mustReadRAGEvalFile(t, out), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != rageval.ProgressiveSchemaVersion ||
		report.Corpus.Dimensions != 16 || report.Corpus.SelectedK != 10 {
		t.Fatalf("progressive report options = %+v", report)
	}
}

func TestMixedExperimentRequiresOut(t *testing.T) {
	err := run([]string{"-experiment", "mixed"})
	if err == nil || !strings.Contains(err.Error(), "requires an explicit -out") {
		t.Fatalf("error = %v, want explicit output requirement", err)
	}
}

// TestRunMixedExperiment mirrors TestRunDefaultsToBaseline for the mixed
// experiment: the runner's output must equal the committed baseline exactly,
// so `-experiment mixed -out <baseline path>` IS the regeneration command.
func TestRunMixedExperiment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "mixed.json")
	if err := run([]string{"-experiment", "mixed", "-out", out}); err != nil {
		t.Fatal(err)
	}
	got := mustReadRAGEvalFile(t, out)
	var report rageval.MixedReport
	if err := json.Unmarshal(got, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != rageval.MixedSchemaVersion {
		t.Fatalf("schema_version = %q", report.SchemaVersion)
	}
	if len(report.Cases) != 5 || !report.Summary.AllDeterministic {
		t.Fatalf("report summary = %+v with %d cases", report.Summary, len(report.Cases))
	}
	want, err := os.ReadFile("../../internal/rageval/testdata/mixed-baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("mixed runner output differs from committed mixed-baseline.json")
	}
}

// TestRunUnknownExperimentListsMixed pins that the unknown-experiment error
// advertises the mixed experiment alongside the pre-existing ones.
func TestRunUnknownExperimentListsMixed(t *testing.T) {
	err := run([]string{"-experiment", "nope", "-out", filepath.Join(t.TempDir(), "x.json")})
	if err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("error = %v, want mention of mixed", err)
	}
}

func mustReadRAGEvalFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
