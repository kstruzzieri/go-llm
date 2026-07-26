package rag

import (
	"reflect"
	"testing"
)

func TestDeriveSummaryValidity(t *testing.T) {
	fresh := validSummary() // ContentHash "hash1", VectorSpaceID "vs1", current format
	prov := SourceProvenance{Source: "pkg/a.go", ContentHash: "hash1", VectorSpaceID: "vs1"}

	older := fresh
	older.FormatVersion = SourceSummaryFormatVersion - 1

	newer := fresh
	newer.FormatVersion = SourceSummaryFormatVersion + 1

	blankRow := fresh
	blankRow.Abstract = ""

	tests := []struct {
		name       string
		row        *SourceSummary
		prov       SourceProvenance
		provFound  bool
		evidenceOK bool
		want       []ValidityReason
	}{
		{"fresh", &fresh, prov, true, true, nil},
		{"missing row", nil, prov, true, true,
			[]ValidityReason{ReasonMissing}},
		// missing is mutually exclusive with ROW-BEARING reasons only (spec
		// section 14); unknown_* are current-side, so a missing row whose
		// current side is also blank emits all three.
		{"missing row with unknown current side", nil, SourceProvenance{}, true, true,
			[]ValidityReason{ReasonMissing, ReasonUnknownContentHash, ReasonUnknownVectorSpace}},
		{"malformed short-circuits comparison", &blankRow,
			SourceProvenance{ContentHash: "other", VectorSpaceID: "othervs"}, true, true,
			[]ValidityReason{ReasonMalformedRow}},
		{"format above current is malformed", &newer, prov, true, true,
			[]ValidityReason{ReasonMalformedRow}},
		{"format below current is stale", &older, prov, true, true,
			[]ValidityReason{ReasonStaleFormat}},
		{"content drift", &fresh,
			SourceProvenance{ContentHash: "changed", VectorSpaceID: "vs1"}, true, true,
			[]ValidityReason{ReasonStaleContent}},
		{"vector space drift", &fresh,
			SourceProvenance{ContentHash: "hash1", VectorSpaceID: "vs2"}, true, true,
			[]ValidityReason{ReasonStaleVectorSpace}},
		{"both drift preserved in declaration order", &fresh,
			SourceProvenance{ContentHash: "changed", VectorSpaceID: "vs2"}, true, true,
			[]ValidityReason{ReasonStaleContent, ReasonStaleVectorSpace}},
		{"blank current hash is unknown, never a match", &fresh,
			SourceProvenance{ContentHash: "", VectorSpaceID: "vs1"}, true, true,
			[]ValidityReason{ReasonUnknownContentHash}},
		{"blank current vector space is unknown", &fresh,
			SourceProvenance{ContentHash: "hash1", VectorSpaceID: ""}, true, true,
			[]ValidityReason{ReasonUnknownVectorSpace}},
		{"mixed provenance", &fresh,
			SourceProvenance{ContentHash: "", VectorSpaceID: "vs1", Mixed: true}, true, true,
			[]ValidityReason{ReasonUnknownContentHash, ReasonMixedProvenance}},
		{"evidence mismatch", &fresh, prov, true, false,
			[]ValidityReason{ReasonEvidenceMismatch}},
		{"no provenance at all", &fresh, SourceProvenance{}, false, true,
			[]ValidityReason{ReasonUnknownContentHash, ReasonUnknownVectorSpace}},
		// provFound=false means the store could not supply provenance, so the
		// current side is unknown no matter what the struct holds. Fields that
		// happen to MATCH the row are what pins the !provFound term: drop it
		// and this case yields a bogus "fresh".
		{"provenance not found outranks fields that look valid", &fresh,
			SourceProvenance{ContentHash: "hash1", VectorSpaceID: "vs1"}, false, true,
			[]ValidityReason{ReasonUnknownContentHash, ReasonUnknownVectorSpace}},
		// stale_* and unknown_* are per-field mutually exclusive (a stale
		// comparison needs a non-blank current value), so this five-reason
		// set is the maximal compatible one for a well-formed row.
		{"maximal compatible set", &older,
			SourceProvenance{ContentHash: "", VectorSpaceID: "", Mixed: true}, true, false,
			[]ValidityReason{ReasonStaleFormat, ReasonUnknownContentHash,
				ReasonUnknownVectorSpace, ReasonMixedProvenance, ReasonEvidenceMismatch}},
		// malformed_row short-circuits row comparison (no stale_*/unknown_*)
		// but source- and evidence-scoped reasons still apply.
		{"malformed with mixed and evidence", &blankRow,
			SourceProvenance{ContentHash: "", VectorSpaceID: "", Mixed: true}, true, false,
			[]ValidityReason{ReasonMalformedRow, ReasonMixedProvenance, ReasonEvidenceMismatch}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveSummaryValidity(tt.row, tt.prov, tt.provFound, tt.evidenceOK)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("deriveSummaryValidity = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReasonOrderIsCompleteAndSorts guards the byte-stable emission contract
// from both sides.
//
// Completeness: sortReasons ranks by reasonOrder[reason], and a Go map returns
// 0 for an absent key, so a reason declared without an order entry would
// silently sort ahead of ReasonMissing instead of failing. Go cannot enumerate
// constants reflectively, so the nine are listed explicitly: adding a tenth
// without a reasonOrder entry must break this test.
//
// Sorting: deriveSummaryValidity happens to append in declaration order today,
// which makes its sortReasons call a no-op that no table case can falsify. The
// reversed-input assertion below pins the ordering contract directly, so the
// sort cannot be deleted — and reasonOrder's VALUES cannot be permuted —
// without going red.
func TestReasonOrderIsCompleteAndSorts(t *testing.T) {
	declared := []ValidityReason{
		ReasonMissing, ReasonMalformedRow, ReasonStaleContent,
		ReasonStaleVectorSpace, ReasonStaleFormat, ReasonUnknownContentHash,
		ReasonUnknownVectorSpace, ReasonMixedProvenance, ReasonEvidenceMismatch,
	}
	if len(reasonOrder) != len(declared) {
		t.Fatalf("reasonOrder has %d entries, want %d: a reason was added or removed without updating the order map",
			len(reasonOrder), len(declared))
	}
	seen := make(map[int]ValidityReason, len(declared))
	for _, r := range declared {
		idx, ok := reasonOrder[r]
		if !ok {
			t.Errorf("reason %q absent from reasonOrder: it would sort to 0, ahead of %q", r, ReasonMissing)
			continue
		}
		if dup, clash := seen[idx]; clash {
			t.Errorf("reasons %q and %q share reasonOrder index %d", dup, r, idx)
		}
		seen[idx] = r
	}

	shuffled := make([]ValidityReason, len(declared))
	for i, r := range declared {
		shuffled[len(declared)-1-i] = r
	}
	if got := sortReasons(shuffled); !reflect.DeepEqual(got, declared) {
		t.Fatalf("sortReasons(reversed) = %v, want declaration order %v", got, declared)
	}
}

func TestSummaryRowMalformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SourceSummary)
		want   bool
	}{
		{"valid", func(s *SourceSummary) {}, false},
		// Whitespace-only pins two properties at once: the clause must exist
		// (delete it and this goes red) and must use TrimSpace, not == ""
		// (drop the TrimSpace and this goes red too). Exact "" only pins the
		// former, which is why "empty abstract" below is kept alongside it.
		{"whitespace-only abstract", func(s *SourceSummary) { s.Abstract = "   " }, true},
		{"empty abstract", func(s *SourceSummary) { s.Abstract = "" }, true},
		{"whitespace-only overview", func(s *SourceSummary) { s.Overview = "   " }, true},
		{"whitespace-only model", func(s *SourceSummary) { s.SummaryModel = "   " }, true},
		{"whitespace-only content hash", func(s *SourceSummary) { s.ContentHash = "   " }, true},
		{"whitespace-only vector space", func(s *SourceSummary) { s.VectorSpaceID = "   " }, true},
		{"zero timestamp", func(s *SourceSummary) { s.SummarizedAt = 0 }, true},
		// Above-current format: written by a newer build — cannot be interpreted.
		{"format above current", func(s *SourceSummary) { s.FormatVersion = SourceSummaryFormatVersion + 1 }, true},
		// Below-current format is stale, NOT malformed (spec section 5 matrix).
		{"format below current", func(s *SourceSummary) { s.FormatVersion = SourceSummaryFormatVersion - 1 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := validSummary()
			tt.mutate(&row)
			if got := summaryRowMalformed(row); got != tt.want {
				t.Fatalf("summaryRowMalformed = %v, want %v", got, tt.want)
			}
		})
	}
}
