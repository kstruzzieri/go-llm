package rageval

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestProgressiveExperimentDeterministicAndCandidateEqual(t *testing.T) {
	ctx := context.Background()
	r1, err := RunProgressiveExperiment(ctx, ProgressiveOptions{})
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if r1.SchemaVersion != ProgressiveSchemaVersion {
		t.Fatalf("schema = %q, want %q", r1.SchemaVersion, ProgressiveSchemaVersion)
	}
	if len(r1.Queries) == 0 {
		t.Fatal("no queries")
	}
	sawL1 := false
	for _, q := range r1.Queries {
		// The #246 guard: representation may never prune selection.
		if !q.CandidateSetsEqual {
			t.Fatalf("query %s: candidate sets differ between arms", q.ID)
		}
		if !reflect.DeepEqual(q.Flat.CandidateIDs, q.Progressive.CandidateIDs) {
			t.Fatalf("query %s: recorded IDs differ", q.ID)
		}
		if q.Flat.ContextTokens <= 0 {
			t.Fatalf("query %s: flat arm rendered nothing", q.ID)
		}
		if q.Progressive.RenderFormatVersion != 2 {
			t.Fatalf("query %s: progressive arm format %d, want 2", q.ID, q.Progressive.RenderFormatVersion)
		}
		sawL1 = sawL1 || q.Progressive.SourcesAtL1 > 0
	}
	if !sawL1 {
		t.Fatal("fresh-summary path never rendered an L1 source")
	}
	if r1.Summary.TotalMetadataFallback == 0 {
		t.Fatal("metadata fallback path never exercised")
	}
	// Byte determinism across runs.
	r2, err := RunProgressiveExperiment(ctx, ProgressiveOptions{})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if string(b1) != string(b2) {
		t.Fatal("experiment is not byte-deterministic across runs")
	}
}
