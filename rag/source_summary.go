package rag

import (
	"context"
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

// validateSourceSummaryWrite enforces the write contract: no blank fields, the
// current format version, a positive timestamp. A summary must name the vector
// space it was generated against — blank provenance is a validity reason on
// the read side, never a legal stored value.
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
func (s *SQLiteStore) UpsertSourceSummary(ctx context.Context, row SourceSummary) error {
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

// inClauseQuery formats query (which must contain exactly one %s) with the
// right number of placeholders and returns the matching args slice.
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
