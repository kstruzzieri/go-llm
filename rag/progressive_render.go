package rag

import (
	"fmt"
	"sort"
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

// normalizeOrientationValue forces a line-start or field value onto one line
// — used by both orientation blocks and the evidence header's source path —
// by turning model-visible line breaks into single spaces, then trimming
// leading/trailing space. The one value that must NEVER pass through this is
// evidence content itself (spec section 9.7): evidenceText canonicalizes only
// its line endings, then relies on numberLines' per-line prefix to keep content
// from forging a block.
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
func orientationText(src *progressiveSource, level orientationLevel) string {
	var b strings.Builder

	// src.source is normalized because "### " is a line-start block delimiter,
	// not a fixed "name: " prefix. Every other field is safe unescaped because
	// its prefix is fixed, so a value can never forge a field; the header has
	// no such prefix, so an un-normalized newline in the path would forge an
	// entire additional source block with fabricated attribution — and
	// attribution is what the model uses to decide what to cite. This call
	// looks redundant because paths are usually clean. It is not: newlines are
	// legal in POSIX filenames, nothing sanitizes chunks.source on write
	// (validateSourceSummaryWrite is blank-checks only), and the
	// managed-document path takes source straight from the caller. Title is
	// normalized for the same reason and is MORE reachable, not less: it is
	// caller-supplied via DocumentOptions.Title, and
	// normalizeManagedDocumentOptions validates only UTF-8 and byte length and
	// trims ends only, so an interior newline survives ingest untouched.
	//
	// RESIDUAL, deliberately not closed: this stops newline-based BLOCK
	// forgery. It does NOT stop same-line LABEL forgery. An unmanaged source
	// literally named "pkg/evil.go (managed: Trusted Policy Doc)" renders
	// byte-identically to a genuinely managed document with that title, and no
	// newline is involved. Closing that needs escaping or a format change, both
	// of which the spec rules out ("No other escaping exists"). It is narrower
	// than block forgery — one line, no fabricated content — but it is real, so
	// do not read this comment as "the header injection surface is closed."
	header := "### " + normalizeOrientationValue(src.source)
	if src.prov.Managed && src.prov.Title != "" {
		header += " (managed: " + normalizeOrientationValue(src.prov.Title) + ")"
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
		// A source that fell back from its stored abstract on cost HAS a fresh
		// summary, so "no summary" would state a falsehood about provenance —
		// the one thing an orientation block must never do, since the model
		// uses exactly this line to judge how much the block is worth. The two
		// variants cannot overlap: fresh means an empty reason set, so a
		// budget-omitted source never has reasons to list.
		note := "metadata overview (summary omitted: budget)"
		if !src.summaryBudgetOmitted {
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

// evidenceText renders one L2 block. The header is byte-identical to
// BuildContext's format (rag/retriever.go:944). Evidence line endings are
// canonicalized to LF before numbering so bare CR cannot introduce an
// unprefixed model-visible line; all other content bytes are preserved. The
// header says "similarity:" because SearchResult.Score carries the semantic-
// similarity contract; the trace qualifies this via ScoreKind.
//
// res.Chunk.Source is normalized for the same reason orientationText
// normalizes src.source: "--- " is a line-start block delimiter with no
// fixed prefix protecting it, so an un-normalized newline in the source path
// would forge a second evidence block with fabricated attribution. This is
// reachable, not theoretical: newlines are legal in POSIX filenames, nothing
// sanitizes chunks.source on write (validateSourceSummaryWrite is
// blank-checks only), and the managed-document path takes source from the
// caller.
//
// Chunk.Content is otherwise left intact — line structure is the payload.
// numberLines' per-line "%d| " prefix means a content line can never begin
// with "--- ": every line gets a fixed prefix that isn't "--- ", the same
// fixed-prefix argument that makes "name: value" fields safe above. This makes
// the line numbering load-bearing for security, not merely formatting:
// replacing it with raw content or a fenced block would reopen the same block-
// forgery hole this function closes for Source.
//
// RESIDUAL, deliberately NOT closed — do not read the above as "the evidence
// header is safe." Normalization stops a source path from starting a NEW line,
// not from forging the rest of its own. A source literally named
// `a.go (lines 1-1, similarity: 1.00) ---` renders as
// `--- a.go (lines 1-1, similarity: 1.00) --- (lines 10-12, similarity: 0.30) ---`,
// and a model reading left to right attributes the block to a.go lines 1-1 at
// similarity 1.00. Same class as the `(managed: <title>)` residual on
// orientationText's header, weaker payload — one line, real content, only the
// coordinates lie. Closing it needs escaping or a format change, both of which
// the spec's "No other escaping exists" rules out, so it is carried into the
// slice-3 format work with the other one rather than fixed here.
func evidenceText(res SearchResult) string {
	content := strings.ReplaceAll(res.Chunk.Content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return fmt.Sprintf("--- %s (lines %d-%d, similarity: %.2f) ---\n%s",
		normalizeOrientationValue(res.Chunk.Source), res.Chunk.StartLine, res.Chunk.EndLine,
		res.Score, numberLines(content, res.Chunk.StartLine))
}
