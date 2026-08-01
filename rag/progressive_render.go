package rag

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// orientationLevel is the internal representation ladder for one source's
// single orientation block (spec section 9.3 grammar).
type orientationLevel int

const (
	orientationNone orientationLevel = iota
	orientationMeta                  // deterministic metadata overview (A0 / A3a)
	orientationL0                    // stored abstract (A1 / A3b)
	orientationL0L1                  // stored abstract + overview (A2 / A3c)
)

// progressiveSource is the per-source allocation state. The allocation fields
// below the blank line are declared here but populated by the budget allocator
// (Task 10) and RenderProgressive (Task 11); Task 8 renders from the fields
// above it only.
//
// Zero-value hazard for those populators: orientation defaults to
// orientationNone, which is a MEANINGFUL value ("omitted entirely"), not an
// obviously-unset one — so a half-populated struct read early renders as a
// legitimately-omitted source instead of failing. decisions being a nil map is
// the better failure mode by contrast, because writing to it panics loudly;
// do not "fix" it into a lazily-initialised map.
type progressiveSource struct {
	source     string
	firstIndex int // index in req.Results of this source's first result (source order)
	results    []SearchResult
	resultIdx  []int // matching indices into req.Results (retrieval order)
	prov       SourceProvenance
	provFound  bool
	summary    *SourceSummary
	reasons    []ValidityReason
	fresh      bool // reasons empty AND summary present

	orientation      orientationLevel
	evidence         []int // admitted indices into results (retrieval order)
	rejectedEvidence []int // indices rejected on cost; never reconsidered (spec 10 emission table)
	pinnedEvidence   []int // evidence indices admitted by step 3 (caller pins); the trim may never drop these
	floorEvidence    []int // evidence indices admitted by step 4 (floor); recounted for FloorRendered after a trim
	costRejected     bool  // some more-expensive alternative was rejected on cost
	decisions        map[string]bool
	snapshotMeta     bool // orientation metadata describes the retrieval snapshot (race path, spec section 8)
	// summaryBudgetOmitted marks a FRESH source that fell back from its stored
	// abstract to the metadata overview because the abstract did not fit
	// (allocator step 5). It keeps the note line honest: "no summary" is false
	// for such a source. Explicit rather than derived from
	// fresh && orientation == orientationMeta, because that derivation is
	// sound only by an invariant neither file states — the DEV-11 shape.
	summaryBudgetOmitted bool
}

// normalizeOrientationValue forces an orientation field value onto one line
// by turning model-visible line breaks into single spaces, then trimming
// leading/trailing space. Structural source/title values use strconv.Quote.
// The one value that must NEVER pass through this is evidence content itself
// (spec section 9.7): evidenceText canonicalizes only its line endings, then
// relies on numberLines' per-line prefix to keep content from forging a block.
var orientationValueReplacer = strings.NewReplacer(
	"\r", " ", "\n", " ", "\v", " ", "\f", " ",
	"\u0085", " ", "\u2028", " ", "\u2029", " ",
)

func replaceOrientationLineBreaks(v string) string {
	return orientationValueReplacer.Replace(v)
}

func normalizeOrientationValue(v string) string {
	return strings.TrimSpace(replaceOrientationLineBreaks(v))
}

// joinSortedUnique de-duplicates, sorts, and comma-joins list-field values.
func joinSortedUnique(values []string) string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, v := range values {
		v = normalizeOrientationValue(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func rfc3339UTC(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// orientationText renders one source's single orientation block per the exact
// spec section 9.7 template. Field order is fixed; empty fields are omitted
// entirely. All values are untrusted data rendered as delimited context.
//
// The metadata note variant comes from the source's own allocator-written
// summaryBudgetOmitted flag, which is what every FLAT caller wants.
func orientationText(src *progressiveSource, level orientationLevel) string {
	return orientationTextWithNote(src, level, src.summaryBudgetOmitted)
}

// orientationTextWithNote is orientationText with the metadata note variant
// supplied by the caller instead of read off the source.
//
// It exists for the capability projection (progressive_groups.go), which runs
// BEFORE allocation and must offer a fresh source the truthful
// "summary omitted: budget" metadata rung. summaryBudgetOmitted is
// allocator-owned state, so the projection cannot set it: writing it would
// change the note line of the block the FLAT render emits moments later, and
// a write-then-restore would still be visible to anything reading the source
// concurrently. Passing the flag keeps the projection a pure reader.
func orientationTextWithNote(src *progressiveSource, level orientationLevel, summaryBudgetOmitted bool) string {
	var b strings.Builder

	// Render format v2 (#331 spec 3.4): source and managed title are untrusted
	// values placed at line-start structural positions, so both render through
	// strconv.Quote — deterministic and lossless, and a quote character inside
	// the data arrives escaped, so a value can neither terminate the field nor
	// fake a header. This closes slice 1's documented same-line label-forgery
	// residual (an unmanaged source named `pkg/evil.go (managed: Trusted
	// Policy Doc)` used to render byte-identically to a genuinely managed
	// document). Newline-based block forgery was already closed in v1; Quote
	// escapes those too, which also makes the old lossy newline collapsing of
	// these two values unnecessary. Structural text ("### source: ",
	// "managed-title: ") is fixed and never interpolated adjacent to unquoted
	// data.
	header := "### source: " + strconv.Quote(src.source)
	if src.prov.Managed && src.prov.Title != "" {
		header += " managed-title: " + strconv.Quote(src.prov.Title)
	}
	b.WriteString(header + "\n")

	writeField := func(name, value string) {
		if value != "" {
			b.WriteString(name + ": " + value + "\n")
		}
	}

	if level >= orientationL0 && src.summary != nil {
		writeField("purpose", normalizeOrientationValue(src.summary.Abstract))
	}
	// >= rather than == for symmetry with purpose above: behavior-identical
	// while orientationL0L1 tops the ladder, but if a level is ever added
	// above it, == would silently drop overview while purpose kept rendering.
	if level >= orientationL0L1 && src.summary != nil {
		writeField("overview", normalizeOrientationValue(src.summary.Overview))
	}

	// language is scalar per source, so it takes the first non-empty value —
	// i.e. the highest-scoring chunk's, since results arrive in score order.
	// Mixed-language sources (fenced markdown, .vue, HTML+script) can therefore
	// render different values for different queries; orientation metadata
	// describes the retrieval snapshot, not canonical state (spec section 8).
	var language string
	var symbols, sections []string
	for _, res := range src.results {
		if language == "" && res.Chunk.Language != "" {
			language = res.Chunk.Language
		}
		if v := res.Chunk.Metadata["symbol_path"]; v != "" {
			symbols = append(symbols, v)
		}
		if v := res.Chunk.Metadata["section_path"]; v != "" {
			sections = append(sections, v)
		}
	}
	writeField("language", normalizeOrientationValue(language))
	writeField("symbols", joinSortedUnique(symbols))
	writeField("sections", joinSortedUnique(sections))
	writeField("collection", normalizeOrientationValue(src.prov.Collection))
	writeField("tags", joinSortedUnique(src.prov.Tags))
	if src.prov.IndexedAt > 0 {
		writeField("indexed", rfc3339UTC(src.prov.IndexedAt))
	}
	if src.prov.Managed && src.prov.Freshness != "" {
		// Defense in depth: Freshness is a CHECK-constrained enum column today,
		// so this cannot currently carry a newline — but it is the same
		// DB-string-into-rendered-output shape as the header and costs nothing.
		writeField("freshness", normalizeOrientationValue(string(src.prov.Freshness)))
	}

	// SummarizedAt is guarded like IndexedAt above: rendering a zero as
	// 1970-01-01T00:00:00Z states a lie as provenance. Unreachable via the
	// planned ladder (a malformed row is not fresh, so the caller picks
	// orientationMeta and no summary line renders), but orientationText's
	// contract does not state that precondition, so the guard lives here
	// rather than depending on a caller invariant three tasks away.
	if level >= orientationL0 && src.summary != nil && src.summary.SummarizedAt > 0 {
		writeField("summary", normalizeOrientationValue(src.summary.SummaryModel)+" @ "+rfc3339UTC(src.summary.SummarizedAt))
	}
	if level == orientationMeta {
		// A source rendered at metadata level DESPITE having a fresh summary —
		// the allocator's step-5 cost fallback, or the projection's cheapest
		// rung — has one, so "no summary" would state a falsehood about
		// provenance, the one thing an orientation block must never do, since
		// the model uses exactly this line to judge how much the block is
		// worth. The two variants cannot overlap: fresh means an empty reason
		// set, so a budget-omitted source never has reasons to list.
		note := "metadata overview (summary omitted: budget)"
		if !summaryBudgetOmitted {
			// reasons are deliberately NOT normalized while Freshness is:
			// ValidityReason values are compile-time constants that never
			// round-trip through storage, a strictly stronger guarantee than
			// Freshness's CHECK constraint. Join order is declaration order,
			// not alphabetical (deriveSummaryValidity guarantees it) — see
			// spec section 9.7.
			parts := make([]string, 0, len(src.reasons))
			for _, r := range src.reasons {
				parts = append(parts, string(r))
			}
			note = "metadata overview (no summary"
			if len(parts) > 0 {
				note += ": " + strings.Join(parts, ", ")
			}
			note += ")"
		}
		writeField("note", note)
	}
	return b.String()
}

// evidenceText renders one L2 block. The v2 header deliberately diverges from
// BuildContext's frozen "--- %s (lines ...) ---" format: the source is
// strconv.Quote'd so a path containing literal header text renders inertly
// (#331 spec 3.4). BuildContext itself is frozen and unchanged. Evidence line
// endings are canonicalized to LF before numbering so bare CR cannot
// introduce an unprefixed model-visible line; all other content bytes are
// preserved. The header says "similarity:" because SearchResult.Score
// carries the semantic-similarity contract; the trace qualifies this via
// ScoreKind.
//
// Chunk.Content is otherwise left intact — line structure is the payload.
// numberLines' per-line "%d| " prefix means a content line can never begin
// with "--- ": every line gets a fixed prefix that isn't "--- ", the same
// fixed-prefix argument that makes "name: value" fields safe above. This makes
// the line numbering load-bearing for security, not merely formatting:
// replacing it with raw content or a fenced block would reopen the same block-
// forgery hole this function closes for Source.
//
// The slice-1 same-line forgery residual is CLOSED by the quoting above; do
// not reintroduce raw interpolation of Source into this header.
func evidenceText(res SearchResult) string {
	content := strings.ReplaceAll(res.Chunk.Content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return fmt.Sprintf("--- source: %s (lines %d-%d, similarity: %.2f) ---\n%s",
		strconv.Quote(res.Chunk.Source), res.Chunk.StartLine, res.Chunk.EndLine,
		res.Score, numberLines(content, res.Chunk.StartLine))
}
