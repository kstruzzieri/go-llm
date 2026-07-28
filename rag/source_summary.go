package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SourceSummaryFormatVersion is the current stored representation version.
// Bump it whenever the summarize prompt, output contract, or rendering of
// stored text changes in a way that makes existing rows non-comparable.
const SourceSummaryFormatVersion = 1

const upsertSourceSummarySQL = `
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
	  summarized_at   = excluded.summarized_at`

// SourceSummary is one persisted source_summaries row: an atomic L0/L1 pair
// for a single indexed source or managed document (#189 slice 1).
type SourceSummary struct {
	Source string
	// ContentHash and VectorSpaceID are the provenance this summary was
	// generated against. They must EQUAL what SourceProvenanceBatch currently
	// reports for Source — see UpsertSourceSummary for why anything else is a
	// silent failure rather than an error.
	ContentHash   string
	VectorSpaceID string
	Abstract      string // L0
	Overview      string // L1
	SummaryModel  string
	FormatVersion int
	SummarizedAt  int64
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

// UpsertSourceSummary writes one L0/L1 pair atomically, replacing any existing
// row for the source.
//
// SourceProvenanceBatch is this method's MANDATORY companion, not an
// incidental export. Write validation only rejects blank fields — it cannot
// check that row.ContentHash and row.VectorSpaceID describe the source as
// currently indexed. Supply anything else and the write succeeds, no error is
// returned anywhere, and every later render derives stale_content or
// stale_vector_space and falls back to the deterministic metadata overview:
// the summary is stored but permanently unreadable.
//
// So callers must read the source's provenance first and copy those values:
//
//	prov, err := store.SourceProvenanceBatch(ctx, []string{source})
//	// ... row.ContentHash = prov[source].ContentHash
//	// ... row.VectorSpaceID = prov[source].VectorSpaceID
//
// Do NOT derive ContentHash from GetSourceHash: it returns the raw
// source_signature JSON, while the value compared at render time is the
// content_hash INSIDE that document, and the parser for it is unexported.
// SourceProvenanceBatch is the only supported way to obtain it.
//
// A blank prov.ContentHash or prov.VectorSpaceID means the current side is
// unknown (mixed chunks, or an unparseable signature); there is no correct
// value to store in that case, so defer the summary rather than guess.
//
// Source, ContentHash, and VectorSpaceID are stored byte-for-byte: they are
// exact join/comparison values returned by SourceProvenanceBatch. Whitespace
// around a nonblank value is therefore significant, not canonicalized here.
func (s *SQLiteStore) UpsertSourceSummary(ctx context.Context, row SourceSummary) error {
	row.SummaryModel = strings.TrimSpace(row.SummaryModel)
	if err := validateSourceSummaryWrite(row); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, upsertSourceSummarySQL,
		row.Source, row.ContentHash, row.VectorSpaceID, row.Abstract, row.Overview,
		row.SummaryModel, row.FormatVersion, row.SummarizedAt)
	if err != nil {
		return fmt.Errorf("rag: upsert source summary %q: %w", row.Source, err)
	}
	return nil
}

// DeleteSourceSummary removes the summary row for source. Deleting an absent
// row is a no-op.
//
// This runs on s.db, its own connection, not any caller-supplied transaction
// — do NOT call it from inside a transaction that also deletes the source's
// chunks/registry row: the two statements would not commit or roll back
// together. The three lifecycle deletion sites (DeleteBySource and
// replaceSourceTx in this file, DeleteDocument in rag/managed.go) issue
// `DELETE FROM source_summaries WHERE source = ?` directly on their own tx
// instead, precisely to keep the summary delete atomic with the rest of the
// removal (#189 spec section 12).
func (s *SQLiteStore) DeleteSourceSummary(ctx context.Context, source string) error {
	if strings.TrimSpace(source) == "" {
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
	err := forEachSourceBatch(sources, func(batch []string) error {
		query, args := inClauseQuery(`
			SELECT source, content_hash, vector_space_id, abstract, overview,
			       summary_model, format_version, summarized_at
			FROM source_summaries WHERE source IN (%s)`, batch)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			if s.immutable && isMissingTableErr(err, "source_summaries") {
				// A v7 database opened via OpenSQLiteStoreReadOnly cannot
				// migrate: the feature degrades to summary-missing. On a
				// WRITABLE store the table is guaranteed by migration, so a
				// missing table there is corruption and propagates (spec
				// section 13). The table cannot appear between batches, so
				// skipping this one skips them all.
				return errSummaryTableAbsent
			}
			return fmt.Errorf("rag: source summary batch: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r SourceSummary
			if err := rows.Scan(&r.Source, &r.ContentHash, &r.VectorSpaceID, &r.Abstract,
				&r.Overview, &r.SummaryModel, &r.FormatVersion, &r.SummarizedAt); err != nil {
				return fmt.Errorf("rag: scan source summary: %w", err)
			}
			out[r.Source] = r
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("rag: iterate source summaries: %w", err)
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSummaryTableAbsent) {
		return nil, err
	}
	return out, nil
}

// errSummaryTableAbsent unwinds SourceSummaryBatch's per-batch loop when a
// read-only pre-v8 database has no source_summaries table. It is a control
// signal, never returned to callers: the degraded result is an empty map and a
// nil error, exactly as before batching was introduced.
var errSummaryTableAbsent = errors.New("rag: source_summaries table absent (read-only degrade)")

// maxSourcesPerBatch bounds how many sources one IN (...) read binds at a time.
//
// SQLite's SQLITE_MAX_VARIABLE_NUMBER is 32766 in the bundled modernc build,
// and exceeding it fails the whole statement with "too many SQL variables"
// rather than degrading. These batch readers were introduced for
// retrieval-result-sized inputs (a handful of sources), but
// GenerateSourceSummaries passes every source in the index, so a large enough
// workspace would hit the ceiling. Chunking here rather than at that one call
// site keeps the limit handled for every caller, present and future.
const maxSourcesPerBatch = 8000

// forEachSourceBatch calls fn with successive slices of sources, each no longer
// than maxSourcesPerBatch. sources is never copied; fn must not retain its
// argument beyond the call. An empty sources calls fn zero times.
func forEachSourceBatch(sources []string, fn func([]string) error) error {
	for start := 0; start < len(sources); start += maxSourcesPerBatch {
		end := start + maxSourcesPerBatch
		if end > len(sources) {
			end = len(sources)
		}
		if err := fn(sources[start:end]); err != nil {
			return err
		}
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
