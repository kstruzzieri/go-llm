package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SourceSummaryFormatVersion is the current stored representation version.
// Bump it whenever the summarize prompt, output contract, or rendering of
// stored text changes in a way that makes existing rows non-comparable.
const SourceSummaryFormatVersion = 1

// SourceSummary is one persisted source_summaries row: an atomic L0/L1 pair
// for a single indexed source or managed document (#189 slice 1).
type SourceSummary struct {
	Source        string
	ContentHash   string
	VectorSpaceID string
	Abstract      string // L0
	Overview      string // L1
	SummaryModel  string
	FormatVersion int
	SummarizedAt  int64
}

// normalizeSourceSummary returns a copy of row with its identity/provenance
// fields (Source, ContentHash, VectorSpaceID, SummaryModel) trimmed to their
// canonical form. Source is the primary key and the join key against
// chunks.source / managed_documents.source, so an untrimmed value would
// silently orphan the row from every downstream reader and from delete.
// Abstract and Overview are free text, deliberately left untouched: the
// renderers normalize display text themselves, and trimming here would be a
// second, competing normalization layer.
func normalizeSourceSummary(row SourceSummary) SourceSummary {
	row.Source = strings.TrimSpace(row.Source)
	row.ContentHash = strings.TrimSpace(row.ContentHash)
	row.VectorSpaceID = strings.TrimSpace(row.VectorSpaceID)
	row.SummaryModel = strings.TrimSpace(row.SummaryModel)
	return row
}

// validateSourceSummaryWrite enforces the write contract: no blank fields, the
// current format version, a positive timestamp. A summary must name the vector
// space it was generated against — blank provenance is a validity reason on
// the read side, never a legal stored value. This is deliberately stricter
// than the read-side summaryRowMalformed check, which rejects only an
// above-current format_version: a below-current row is stale, not malformed,
// so it must remain writable-rejected but readable-interpretable.
func validateSourceSummaryWrite(row SourceSummary) error {
	switch {
	case strings.TrimSpace(row.Source) == "":
		return fmt.Errorf("rag: upsert source summary: blank source")
	case strings.TrimSpace(row.ContentHash) == "":
		return fmt.Errorf("rag: upsert source summary %q: blank content_hash", row.Source)
	case strings.TrimSpace(row.VectorSpaceID) == "":
		return fmt.Errorf("rag: upsert source summary %q: blank vector_space_id", row.Source)
	case strings.TrimSpace(row.Abstract) == "":
		return fmt.Errorf("rag: upsert source summary %q: blank abstract", row.Source)
	case strings.TrimSpace(row.Overview) == "":
		return fmt.Errorf("rag: upsert source summary %q: blank overview", row.Source)
	case strings.TrimSpace(row.SummaryModel) == "":
		return fmt.Errorf("rag: upsert source summary %q: blank summary_model", row.Source)
	case row.FormatVersion != SourceSummaryFormatVersion:
		return fmt.Errorf("rag: upsert source summary %q: format_version %d, want %d",
			row.Source, row.FormatVersion, SourceSummaryFormatVersion)
	case row.SummarizedAt <= 0:
		return fmt.Errorf("rag: upsert source summary %q: non-positive summarized_at %d",
			row.Source, row.SummarizedAt)
	}
	return nil
}

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

// UpsertSourceSummary writes one L0/L1 pair atomically, replacing any existing
// row for the source.
func (s *SQLiteStore) UpsertSourceSummary(ctx context.Context, row SourceSummary) error {
	row = normalizeSourceSummary(row)
	if err := validateSourceSummaryWrite(row); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO source_summaries
		  (source, content_hash, vector_space_id, abstract, overview, summary_model, format_version, summarized_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET
		  content_hash    = excluded.content_hash,
		  vector_space_id = excluded.vector_space_id,
		  abstract        = excluded.abstract,
		  overview        = excluded.overview,
		  summary_model   = excluded.summary_model,
		  format_version  = excluded.format_version,
		  summarized_at   = excluded.summarized_at`,
		row.Source, row.ContentHash, row.VectorSpaceID, row.Abstract, row.Overview,
		row.SummaryModel, row.FormatVersion, row.SummarizedAt)
	if err != nil {
		return fmt.Errorf("rag: upsert source summary %q: %w", row.Source, err)
	}
	return nil
}

// DeleteSourceSummary removes the summary row for source. Deleting an absent
// row is a no-op.
func (s *SQLiteStore) DeleteSourceSummary(ctx context.Context, source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("rag: delete source summary: blank source")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM source_summaries WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: delete source summary %q: %w", source, err)
	}
	return nil
}

// SourceSummaryBatch loads summary rows for the given sources. Sources with no
// row are absent from the map. Rows are returned as stored; interpretation
// (malformed / stale) happens in deriveSummaryValidity.
//
// sources are expected already canonical (untrimmed as-is): they originate
// from chunks.source, and the returned map is keyed by the DB-returned
// source, so trimming the query input here would let a padded caller name
// query successfully while being unable to look up its own result.
func (s *SQLiteStore) SourceSummaryBatch(ctx context.Context, sources []string) (map[string]SourceSummary, error) {
	out := make(map[string]SourceSummary, len(sources))
	if len(sources) == 0 {
		return out, nil
	}
	query, args := inClauseQuery(`
		SELECT source, content_hash, vector_space_id, abstract, overview,
		       summary_model, format_version, summarized_at
		FROM source_summaries WHERE source IN (%s)`, sources)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		if s.immutable && isMissingTableErr(err, "source_summaries") {
			// A v7 database opened via OpenSQLiteStoreReadOnly cannot migrate:
			// the feature degrades to summary-missing. On a WRITABLE store the
			// table is guaranteed by migration, so a missing table there is
			// corruption and propagates (spec section 13).
			return out, nil
		}
		return nil, fmt.Errorf("rag: source summary batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r SourceSummary
		if err := rows.Scan(&r.Source, &r.ContentHash, &r.VectorSpaceID, &r.Abstract,
			&r.Overview, &r.SummaryModel, &r.FormatVersion, &r.SummarizedAt); err != nil {
			return nil, fmt.Errorf("rag: scan source summary: %w", err)
		}
		out[r.Source] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate source summaries: %w", err)
	}
	return out, nil
}

// SourceProvenance is the per-source data the progressive renderer needs that
// SearchResult does not carry (current content hash, vector space, managed
// registry metadata). Blank ContentHash/VectorSpaceID mean "unknown" and are
// validity reasons, never treated as matching values (design rule D6).
type SourceProvenance struct {
	Source        string
	ContentHash   string
	VectorSpaceID string
	IndexedAt     int64 // MAX over the source's chunks; display-only
	Mixed         bool  // MIN != MAX on signature or vector space
	Managed       bool  // resolved via managed_documents, never the prefix
	Title         string
	Collection    string
	Tags          []string
	Freshness     DocumentFreshness
}

// SourceProvenanceBatch loads provenance for the given sources in two bounded
// queries: a GROUP BY over chunks (MIN/MAX uniformity check, following the
// probe at rag/sqlite_store.go:671) and a managed_documents ownership join.
// When a field is mixed across a source it is left blank rather than picking
// an arbitrary row, so it cascades into the matching unknown_* validity
// reason alongside mixed_provenance.
func (s *SQLiteStore) SourceProvenanceBatch(ctx context.Context, sources []string) (map[string]SourceProvenance, error) {
	out := make(map[string]SourceProvenance, len(sources))
	if len(sources) == 0 {
		return out, nil
	}
	query, args := inClauseQuery(`
		SELECT source,
		       MIN(source_content_hash), MAX(source_content_hash),
		       MIN(vector_space_id), MAX(vector_space_id),
		       MAX(indexed_at)
		FROM chunks WHERE source IN (%s) GROUP BY source`, sources)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rag: source provenance batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p SourceProvenance
		var minSig, maxSig, minVS, maxVS string
		if err := rows.Scan(&p.Source, &minSig, &maxSig, &minVS, &maxVS, &p.IndexedAt); err != nil {
			return nil, fmt.Errorf("rag: scan source provenance: %w", err)
		}
		if minSig != maxSig {
			p.Mixed = true // content hash left blank
		} else if sig, ok := parseSourceSignature(minSig); ok {
			p.ContentHash = sig.ContentHash
		} // unparseable/blank signature: ContentHash stays "" (unknown)
		if minVS != maxVS {
			p.Mixed = true // vector space left blank
		} else {
			p.VectorSpaceID = minVS // may be "" (unknown, e.g. legacy or plain Store writes)
		}
		out[p.Source] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate source provenance: %w", err)
	}

	if err := s.attachManagedProvenance(ctx, sources, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachManagedProvenance overlays managed_documents registry metadata onto
// already-loaded provenance rows. A missing managed_documents table (pre-v6
// database opened read-only) degrades to "nothing is managed".
func (s *SQLiteStore) attachManagedProvenance(ctx context.Context, sources []string, out map[string]SourceProvenance) error {
	query, args := inClauseQuery(`
		SELECT source, title, collection, tags, freshness
		FROM managed_documents WHERE source IN (%s)`, sources)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		if s.immutable && isMissingTableErr(err, "managed_documents") {
			// Pre-v6 database opened read-only: nothing is managed. A missing
			// registry on a writable store is corruption and propagates.
			return nil
		}
		return fmt.Errorf("rag: managed provenance batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source, title, collection, tagsJSON, freshness string
		if err := rows.Scan(&source, &title, &collection, &tagsJSON, &freshness); err != nil {
			return fmt.Errorf("rag: scan managed provenance: %w", err)
		}
		p, ok := out[source]
		if !ok {
			continue // registry row without chunks: nothing to render for it
		}
		p.Managed = true
		p.Title = title
		p.Collection = collection
		p.Freshness = DocumentFreshness(freshness)
		var tags []string
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			// Corruption propagates; the spec's degradation paths are
			// enumerated in section 13 and this is not one of them.
			return fmt.Errorf("rag: decode managed provenance tags for %q: %w", source, err)
		}
		p.Tags = tags
		out[source] = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rag: iterate managed provenance: %w", err)
	}
	return nil
}

// inClauseQuery formats query (which must contain exactly one %s) with the
// right number of placeholders and returns the matching args slice. query
// must not contain any other printf verb: a stray one (e.g. a literal "%s" in
// an embedded strftime format) is not caught here and corrupts the SQL
// silently rather than erroring — fmt substitutes "%!s(MISSING)" for it,
// SQLite accepts the malformed literal without complaint, and the query
// simply returns zero rows with a nil error.
func inClauseQuery(query string, values []string) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return fmt.Sprintf(query, placeholders), args
}

// isMissingTableErr reports whether err is SQLite's "no such table" for the
// named table — the only error class the summary read path degrades on.
func isMissingTableErr(err error, table string) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: "+table)
}
