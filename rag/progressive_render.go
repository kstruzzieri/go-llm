package rag

import (
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

// progressiveSource is the per-source allocation state.
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
	costRejected     bool  // some more-expensive alternative was rejected on cost
	decisions        map[string]bool
	snapshotMeta     bool // orientation metadata describes the retrieval snapshot (race path, spec section 8)
}

// normalizeOrientationValue forces one orientation field value onto one line:
// CR and LF become single spaces, then leading/trailing space is trimmed.
// Evidence content is NEVER passed through this (spec section 9.7).
var orientationValueReplacer = strings.NewReplacer("\r", " ", "\n", " ")

func normalizeOrientationValue(v string) string {
	return strings.TrimSpace(orientationValueReplacer.Replace(v))
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

	header := "### " + src.source
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
	if level == orientationL0L1 && src.summary != nil {
		writeField("overview", normalizeOrientationValue(src.summary.Overview))
	}

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
		writeField("freshness", string(src.prov.Freshness))
	}

	if level >= orientationL0 && src.summary != nil {
		writeField("summary", normalizeOrientationValue(src.summary.SummaryModel)+" @ "+rfc3339UTC(src.summary.SummarizedAt))
	}
	if level == orientationMeta {
		parts := make([]string, 0, len(src.reasons))
		for _, r := range src.reasons {
			parts = append(parts, string(r))
		}
		note := "metadata overview (no summary"
		if len(parts) > 0 {
			note += ": " + strings.Join(parts, ", ")
		}
		note += ")"
		writeField("note", note)
	}
	return b.String()
}
