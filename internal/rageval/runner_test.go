package rageval

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
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

func TestFixtureValidateRejectsDuplicateQueryText(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	fixture.Queries[1].Query = fixture.Queries[0].Query
	err = fixture.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate query text, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate query text") {
		t.Fatalf("expected error to mention duplicate query text, got: %v", err)
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

func TestRunModeHonorsZeroWarmRuns(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}

	calls := 0
	report, err := runMode(context.Background(), "test", fixture, RunOptions{WarmRuns: 0, MeasureLatency: false}, func(_ context.Context, query QueryFixture, _ int) ([]rag.SearchResult, error) {
		calls++
		return []rag.SearchResult{
			{
				Chunk: rag.Chunk{
					ID:      query.ExpectedIDs[0],
					Content: "fixture result",
				},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("runMode: %v", err)
	}
	if calls != len(fixture.Queries) {
		t.Fatalf("retrieve calls = %d, want %d cold calls only", calls, len(fixture.Queries))
	}
	for _, query := range report.Queries {
		if query.WarmLatencyMS.Count != 0 {
			t.Fatalf("query %q warm latency count = %d, want 0", query.ID, query.WarmLatencyMS.Count)
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
