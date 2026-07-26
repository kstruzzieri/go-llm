package rag

import (
	"sort"
	"strings"
)

// This file is the read-side DECISION layer for stored source summaries: pure
// interpretation with no database access. Storage I/O lives in
// source_summary.go. summaryRowMalformed belongs here rather than beside the
// storage API because it is read-side interpretation whose only caller is
// deriveSummaryValidity, and its write/read asymmetry pairs with the validity
// matrix below rather than with the write contract. Same split as
// chunk_digest.go and source_provenance.go.

// summaryRowMalformed reports whether a stored row cannot be interpreted at
// all. Malformed means the row bypassed write validation (blank required
// fields, non-positive timestamp) or was written by a NEWER build
// (format_version above current). A below-current format_version is stale,
// not malformed: the row is interpretable, it just no longer applies.
// Malformed short-circuits all freshness comparison.
func summaryRowMalformed(row SourceSummary) bool {
	return strings.TrimSpace(row.ContentHash) == "" ||
		strings.TrimSpace(row.VectorSpaceID) == "" ||
		strings.TrimSpace(row.Abstract) == "" ||
		strings.TrimSpace(row.Overview) == "" ||
		strings.TrimSpace(row.SummaryModel) == "" ||
		row.SummarizedAt <= 0 ||
		row.FormatVersion > SourceSummaryFormatVersion
}

// ValidityReason names one cause a stored summary cannot be served fresh.
// Validity is a SET of reasons so co-occurring drift preserves every cause;
// a summary is fresh iff the set is empty. Reasons are emitted in the
// declaration order below so traces are byte-stable.
type ValidityReason string

const (
	ReasonMissing            ValidityReason = "missing"              // no row
	ReasonMalformedRow       ValidityReason = "malformed_row"        // row fails read validation; short-circuits comparison
	ReasonStaleContent       ValidityReason = "stale_content"        // content_hash mismatch
	ReasonStaleVectorSpace   ValidityReason = "stale_vector_space"   // vector_space_id mismatch
	ReasonStaleFormat        ValidityReason = "stale_format"         // format_version BELOW constant (above => malformed_row)
	ReasonUnknownContentHash ValidityReason = "unknown_content_hash" // CURRENT signature unparseable/absent
	ReasonUnknownVectorSpace ValidityReason = "unknown_vector_space" // CURRENT vector space blank
	ReasonMixedProvenance    ValidityReason = "mixed_provenance"     // MIN != MAX across the source
	ReasonEvidenceMismatch   ValidityReason = "evidence_mismatch"    // retrieved chunk digest no longer matches the index
)

// deriveSummaryValidity computes the reason set for one source at render time.
// row is nil when no summary row exists. provFound is false when the store
// could not supply provenance at all (non-SQLite store); prov is then zeroed
// on entry, so the current side is wholly unknown regardless of what the
// caller passed. evidenceOK is false when any retrieved chunk's stored digest
// is missing or differs (spec section 8). Blank values never compare equal
// (design rule D6): a blank CURRENT value is unknown_*, while a blank STORED
// value is malformed_row (it bypassed write validation).
//
// Reasons have four SCOPES, and the structure below mirrors them — the
// interleaving is load-bearing, not incidental:
//
//	row-bearing  missing, malformed_row, stale_* — the switch, mutually exclusive
//	current-side unknown_*                       — why the current side could not be compared
//	source       mixed_provenance                — the source's chunks disagree
//	evidence     evidence_mismatch               — retrieved chunks no longer match the index
//
// Only ROW-BEARING reasons exclude each other, which is why the three trailing
// blocks sit outside the switch and can co-occur with anything it emitted.
// Flattening this into one chain of ifs would let stale_*/unknown_* accompany
// malformed_row, which the spec section 5 matrix forbids.
func deriveSummaryValidity(row *SourceSummary, prov SourceProvenance, provFound, evidenceOK bool) []ValidityReason {
	if !provFound {
		// The store could not supply provenance at all (a non-*SQLiteStore
		// VectorStore). The current side is then wholly unknown, so no stale_*
		// comparison and no mixed signal is meaningful — ignore prov entirely
		// rather than trusting fields the store never populated. This keeps
		// stale_* and unknown_* per-field mutually exclusive by construction
		// (spec section 5) instead of by caller convention. prov is a value
		// parameter, so this rebinds only the local copy.
		prov = SourceProvenance{}
	}
	var reasons []ValidityReason
	malformed := row != nil && summaryRowMalformed(*row)
	switch {
	case row == nil:
		reasons = append(reasons, ReasonMissing)
	case malformed:
		// Malformed short-circuits the row comparison entirely: no stale_*
		// and no unknown_* accompany it (spec section 5 matrix). Reasons
		// about the SOURCE (mixed) and the EVIDENCE (mismatch) still apply
		// below — they do not describe the row.
		reasons = append(reasons, ReasonMalformedRow)
	default:
		if prov.ContentHash != "" && row.ContentHash != prov.ContentHash {
			reasons = append(reasons, ReasonStaleContent)
		}
		if prov.VectorSpaceID != "" && row.VectorSpaceID != prov.VectorSpaceID {
			reasons = append(reasons, ReasonStaleVectorSpace)
		}
		if row.FormatVersion < SourceSummaryFormatVersion {
			reasons = append(reasons, ReasonStaleFormat)
		}
	}
	if !malformed {
		// Current-side unknowns: why the current side could not be compared.
		// Blankness is the only test needed — the !provFound zeroing above is
		// the single place that case is handled, so a second provFound term
		// here would be dead logic. A missing row still reaches this block
		// (malformed is false when row is nil) and correctly reports an
		// unknown current side: missing is exclusive with row-bearing reasons
		// only.
		if prov.ContentHash == "" {
			reasons = append(reasons, ReasonUnknownContentHash)
		}
		if prov.VectorSpaceID == "" {
			reasons = append(reasons, ReasonUnknownVectorSpace)
		}
	}
	if prov.Mixed {
		reasons = append(reasons, ReasonMixedProvenance)
	}
	if !evidenceOK {
		reasons = append(reasons, ReasonEvidenceMismatch)
	}
	return sortReasons(reasons)
}

// reasonOrder is the fixed declaration order for byte-stable trace emission.
var reasonOrder = map[ValidityReason]int{
	ReasonMissing: 0, ReasonMalformedRow: 1, ReasonStaleContent: 2,
	ReasonStaleVectorSpace: 3, ReasonStaleFormat: 4, ReasonUnknownContentHash: 5,
	ReasonUnknownVectorSpace: 6, ReasonMixedProvenance: 7, ReasonEvidenceMismatch: 8,
}

func sortReasons(reasons []ValidityReason) []ValidityReason {
	sort.Slice(reasons, func(i, j int) bool {
		return reasonOrder[reasons[i]] < reasonOrder[reasons[j]]
	})
	return reasons
}
