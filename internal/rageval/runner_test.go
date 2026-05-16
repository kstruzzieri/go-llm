package rageval

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

const fixturePath = "testdata/fixtures.json"
const baselinePath = "testdata/baseline.json"

func TestFixtureValidate(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if len(fixture.Queries) != 20 {
		t.Fatalf("queries = %d, want 20", len(fixture.Queries))
	}
	categories := map[string]int{}
	for _, query := range fixture.Queries {
		categories[query.Category]++
	}
	wantCategories := []string{"architecture", "code_review", "compare", "cross_file_trace", "single_hop"}
	for _, category := range wantCategories {
		if categories[category] != 4 {
			t.Fatalf("category %q count = %d, want 4", category, categories[category])
		}
	}
}

func TestRunFixtureWithoutLiveModel(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	report, err := Run(context.Background(), fixture, RunOptions{MeasureLatency: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Modes) != 2 {
		t.Fatalf("modes = %d, want 2", len(report.Modes))
	}
	for _, mode := range report.Modes {
		if len(mode.Queries) != len(fixture.Queries) {
			t.Fatalf("mode %q queries = %d, want %d", mode.Name, len(mode.Queries), len(fixture.Queries))
		}
		if mode.Summary.RecallAt5 <= 0 {
			t.Fatalf("mode %q recall@5 = %f, want > 0", mode.Name, mode.Summary.RecallAt5)
		}
	}
}

func TestBaselineReportShape(t *testing.T) {
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if report.Corpus.Queries != 20 {
		t.Fatalf("baseline queries = %d, want 20", report.Corpus.Queries)
	}
	if report.Thresholds.Status != "pending_owner_values_before_95" {
		t.Fatalf("threshold status = %q", report.Thresholds.Status)
	}
	if len(report.Modes) != 2 {
		t.Fatalf("baseline modes = %d, want 2", len(report.Modes))
	}
}
