package rag

import (
	"strings"
	"testing"
)

// orientationFixture builds the per-source input the orientation renderer
// consumes: retrieval-snapshot chunks plus provenance plus optional summary.
func orientationFixture() *progressiveSource {
	return &progressiveSource{
		source: "pkg/a.go",
		results: []SearchResult{{
			Chunk: Chunk{
				ID: "id1", Content: "func A() {\n\treturn\n}\n", Source: "pkg/a.go",
				StartLine: 10, EndLine: 12, Language: "go",
				Metadata: map[string]string{"symbol_path": "A"},
			},
			Score: 0.87,
		}},
		prov:      SourceProvenance{Source: "pkg/a.go", ContentHash: "hash1", VectorSpaceID: "vs1", IndexedAt: 1700000000},
		provFound: true,
	}
}

func TestOrientationTextMetadataOverview(t *testing.T) {
	src := orientationFixture()
	src.reasons = []ValidityReason{ReasonMissing}

	got := orientationText(src, orientationMeta)
	want := "### pkg/a.go\n" +
		"language: go\n" +
		"symbols: A\n" +
		"indexed: 2023-11-14T22:13:20Z\n" +
		"note: metadata overview (no summary: missing)\n"
	if got != want {
		t.Fatalf("metadata overview mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestOrientationTextFreshSummaryL0AndL1(t *testing.T) {
	src := orientationFixture()
	row := SourceSummary{
		Source: "pkg/a.go", ContentHash: "hash1", VectorSpaceID: "vs1",
		Abstract: "Handles A.", Overview: "Defines A; returns immediately.",
		SummaryModel: "qwen3:8b", FormatVersion: SourceSummaryFormatVersion,
		SummarizedAt: 1700000000,
	}
	src.summary = &row

	gotL0 := orientationText(src, orientationL0)
	wantL0 := "### pkg/a.go\n" +
		"purpose: Handles A.\n" +
		"language: go\n" +
		"symbols: A\n" +
		"indexed: 2023-11-14T22:13:20Z\n" +
		"summary: qwen3:8b @ 2023-11-14T22:13:20Z\n"
	if gotL0 != wantL0 {
		t.Fatalf("L0 mismatch:\n got:\n%s\nwant:\n%s", gotL0, wantL0)
	}

	gotL1 := orientationText(src, orientationL0L1)
	wantL1 := "### pkg/a.go\n" +
		"purpose: Handles A.\n" +
		"overview: Defines A; returns immediately.\n" +
		"language: go\n" +
		"symbols: A\n" +
		"indexed: 2023-11-14T22:13:20Z\n" +
		"summary: qwen3:8b @ 2023-11-14T22:13:20Z\n"
	if gotL1 != wantL1 {
		t.Fatalf("L0+L1 mismatch:\n got:\n%s\nwant:\n%s", gotL1, wantL1)
	}
}

func TestOrientationTextManagedFieldsAndNormalization(t *testing.T) {
	src := orientationFixture()
	src.prov.Managed = true
	src.prov.Title = "My Doc"
	src.prov.Collection = "notes"
	src.prov.Tags = []string{"zeta", "alpha", "zeta"} // dedup + sort
	src.prov.Freshness = DocumentFreshnessFresh
	src.reasons = []ValidityReason{ReasonStaleContent, ReasonEvidenceMismatch}
	// Multi-line values collapse to one line (orientation fields only).
	src.results[0].Chunk.Metadata = map[string]string{"section_path": "Intro > Setup"}
	src.results[0].Chunk.Language = "markdown"

	got := orientationText(src, orientationMeta)
	want := "### pkg/a.go (managed: My Doc)\n" +
		"language: markdown\n" +
		"sections: Intro > Setup\n" +
		"collection: notes\n" +
		"tags: alpha, zeta\n" +
		"indexed: 2023-11-14T22:13:20Z\n" +
		"freshness: fresh\n" +
		"note: metadata overview (no summary: stale_content, evidence_mismatch)\n"
	if got != want {
		t.Fatalf("managed overview mismatch:\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestOrientationDelimiterContainingValue(t *testing.T) {
	// The format has no escaping: a value containing ", " renders verbatim.
	// The field prefix up to the first ": " is fixed, so nothing a value
	// contains can terminate the line early (spec section 9.7).
	src := orientationFixture()
	src.reasons = []ValidityReason{ReasonMissing}
	src.results[0].Chunk.Metadata = map[string]string{"symbol_path": "A, B: tricky"}
	got := orientationText(src, orientationMeta)
	if !strings.Contains(got, "symbols: A, B: tricky\n") {
		t.Fatalf("delimiter-containing value must render verbatim:\n%s", got)
	}
}

func TestOrientationValueNormalizationOneLine(t *testing.T) {
	src := orientationFixture()
	row := SourceSummary{
		Source: "pkg/a.go", ContentHash: "hash1", VectorSpaceID: "vs1",
		Abstract: "  line one\r\nline two  ", Overview: "o",
		SummaryModel: "m", FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}
	src.summary = &row
	got := orientationText(src, orientationL0)
	if !strings.Contains(got, "purpose: line one  line two\n") {
		t.Fatalf("CR/LF must collapse to spaces with trim, got:\n%s", got)
	}
}

// TestOrientationUnpinnedGuards pins the three conditionals no other case
// exercises: the managed-only header parenthetical, the managed-only
// freshness field, and the zero-IndexedAt suppression. IndexedAt comes from
// MAX(indexed_at) over a NOT NULL DEFAULT 0 column, so zero is reachable and
// rendering it would present 1970-01-01 as real provenance.
func TestOrientationUnpinnedGuards(t *testing.T) {
	const baseWant = "### pkg/a.go\n" +
		"language: go\n" +
		"symbols: A\n" +
		"indexed: 2023-11-14T22:13:20Z\n" +
		"note: metadata overview (no summary: missing)\n"

	tests := []struct {
		name   string
		mutate func(*progressiveSource)
		want   string
	}{
		{
			name:   "title ignored when not managed",
			mutate: func(src *progressiveSource) { src.prov.Title = "Should Not Appear" },
			want:   baseWant,
		},
		{
			name:   "freshness ignored when not managed",
			mutate: func(src *progressiveSource) { src.prov.Freshness = DocumentFreshnessStale },
			want:   baseWant,
		},
		{
			name:   "zero indexed_at omits the field",
			mutate: func(src *progressiveSource) { src.prov.IndexedAt = 0 },
			want: "### pkg/a.go\n" +
				"language: go\n" +
				"symbols: A\n" +
				"note: metadata overview (no summary: missing)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := orientationFixture()
			src.reasons = []ValidityReason{ReasonMissing}
			tt.mutate(src)
			if got := orientationText(src, orientationMeta); got != tt.want {
				t.Fatalf("mismatch:\n got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}
