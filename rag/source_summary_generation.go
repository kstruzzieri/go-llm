package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSourceSummaryStale means the source identity changed after generation
// started, so the generated text was not published.
var ErrSourceSummaryStale = errors.New("rag: source changed during summary generation")

// SourceSummaryInput is the indexed source snapshot supplied to a summary
// model. Chunks are ordered by start line and come from the store, not the
// current filesystem.
type SourceSummaryInput struct {
	Source string
	Chunks []Chunk
}

// GeneratedSourceSummary is one model-produced L0/L1 pair plus the actual
// provider/model identity that served it.
type GeneratedSourceSummary struct {
	Abstract string
	Overview string
	Model    string
}

// SourceSummaryGenerator generates one L0/L1 pair from indexed chunks.
type SourceSummaryGenerator func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error)

// GenerateSourceSummaries refreshes missing or stale source summaries.
// Sources with unknown/mixed provenance or a newer unreadable format are
// skipped. Each publish uses a transaction-local compare-and-swap against the
// exact ContentHash and VectorSpaceID returned by SourceProvenanceBatch.
// Per-source failures are joined and returned after the remaining eligible
// sources run; context cancellation stops immediately.
func (s *SQLiteStore) GenerateSourceSummaries(ctx context.Context, generate SourceSummaryGenerator) error {
	if generate == nil {
		return fmt.Errorf("rag: generate source summaries: generator is required")
	}
	sources, err := s.ListSources(ctx)
	if err != nil {
		return err
	}
	provenance, err := s.SourceProvenanceBatch(ctx, sources)
	if err != nil {
		return err
	}
	summaries, err := s.SourceSummaryBatch(ctx, sources)
	if err != nil {
		return err
	}

	var generationErr error
	var attempted, failed int
	for _, source := range sources {
		prov, ok := provenance[source]
		if !ok || prov.Mixed || strings.TrimSpace(prov.Source) == "" ||
			strings.TrimSpace(prov.ContentHash) == "" || strings.TrimSpace(prov.VectorSpaceID) == "" {
			continue
		}
		if current, ok := summaries[source]; ok {
			if current.FormatVersion > SourceSummaryFormatVersion {
				continue
			}
			if len(deriveSummaryValidity(&current, prov, true, true)) == 0 {
				continue
			}
		}

		attempted++
		if err := s.generateSourceSummary(ctx, prov, generate); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			failed++
			generationErr = errors.Join(generationErr, err)
		}
	}
	if generationErr == nil {
		return nil
	}
	// The count leads the message because callers surface this through a
	// first-line-only warning: errors.Join renders one error per line, so
	// without a leading tally a single failure and five hundred of them print
	// identically. Wrapping with %w keeps errors.Is traversal into the join.
	return fmt.Errorf("rag: generate source summaries: %d of %d sources failed: %w",
		failed, attempted, generationErr)
}

func (s *SQLiteStore) generateSourceSummary(ctx context.Context, prov SourceProvenance, generate SourceSummaryGenerator) error {
	chunks, snapshot, err := s.sourceSummarySnapshot(ctx, prov.Source)
	if err != nil {
		return err
	}
	if len(chunks) == 0 || snapshot.Mixed || snapshot.Source != prov.Source ||
		snapshot.ContentHash != prov.ContentHash || snapshot.VectorSpaceID != prov.VectorSpaceID {
		return fmt.Errorf("%w: %q changed before generation", ErrSourceSummaryStale, prov.Source)
	}
	generated, err := generate(ctx, SourceSummaryInput{Source: prov.Source, Chunks: chunks})
	if err != nil {
		return fmt.Errorf("rag: generate source summary %q: %w", prov.Source, err)
	}
	return s.upsertSourceSummaryIfCurrent(ctx, SourceSummary{
		Source:        prov.Source,
		ContentHash:   prov.ContentHash,
		VectorSpaceID: prov.VectorSpaceID,
		Abstract:      generated.Abstract,
		Overview:      generated.Overview,
		SummaryModel:  generated.Model,
		FormatVersion: SourceSummaryFormatVersion,
		SummarizedAt:  time.Now().Unix(),
	})
}

// sourceSummarySnapshot reads chunks and the identity stored on those exact
// rows in one SQLite statement, preventing a reindex between the model input
// and the identity captured for it.
func (s *SQLiteStore) sourceSummarySnapshot(ctx context.Context, source string) ([]Chunk, SourceProvenance, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, content, source, start_line, end_line, language, metadata,
		       stable_key, source_content_hash, vector_space_id
		  FROM chunks
		 WHERE source = ?
		 ORDER BY start_line, rowid`, source)
	if err != nil {
		return nil, SourceProvenance{}, fmt.Errorf("rag: load source summary input %q: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	var chunks []Chunk
	var firstSignature, firstVectorSpace string
	var signatureMixed, vectorSpaceMixed bool
	for rows.Next() {
		var chunk Chunk
		var metadataJSON, signature, vectorSpace string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source, &chunk.StartLine,
			&chunk.EndLine, &chunk.Language, &metadataJSON, &chunk.StableKey,
			&signature, &vectorSpace); err != nil {
			return nil, SourceProvenance{}, fmt.Errorf("rag: scan source summary input %q: %w", source, err)
		}
		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metadataJSON), &chunk.Metadata); err != nil {
			return nil, SourceProvenance{}, fmt.Errorf("rag: unmarshal source summary chunk %q metadata: %w", chunk.ID, err)
		}
		if len(chunks) == 0 {
			firstSignature, firstVectorSpace = signature, vectorSpace
		} else {
			signatureMixed = signatureMixed || signature != firstSignature
			vectorSpaceMixed = vectorSpaceMixed || vectorSpace != firstVectorSpace
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, SourceProvenance{}, fmt.Errorf("rag: iterate source summary input %q: %w", source, err)
	}
	snapshot := SourceProvenance{Source: source, Mixed: signatureMixed || vectorSpaceMixed}
	if !signatureMixed {
		if signature, ok := parseSourceSignature(firstSignature); ok {
			snapshot.ContentHash = signature.ContentHash
		}
	}
	if !vectorSpaceMixed {
		snapshot.VectorSpaceID = firstVectorSpace
	}
	return chunks, snapshot, nil
}

func (s *SQLiteStore) upsertSourceSummaryIfCurrent(ctx context.Context, row SourceSummary) error {
	row.SummaryModel = strings.TrimSpace(row.SummaryModel)
	if err := validateSourceSummaryWrite(row); err != nil {
		return err
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("rag: upsert current source summary %q: begin: %w", row.Source, err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	var minSignature, maxSignature, minVectorSpace, maxVectorSpace string
	var storedFormatVersion int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MIN(source_content_hash), ''), COALESCE(MAX(source_content_hash), ''),
		       COALESCE(MIN(vector_space_id), ''), COALESCE(MAX(vector_space_id), ''),
		       COALESCE((SELECT format_version FROM source_summaries WHERE source = ?), 0)
		  FROM chunks
		 WHERE source = ?`, row.Source, row.Source).Scan(
		&count, &minSignature, &maxSignature, &minVectorSpace, &maxVectorSpace, &storedFormatVersion,
	); err != nil {
		return fmt.Errorf("rag: upsert current source summary %q: read provenance: %w", row.Source, err)
	}
	if storedFormatVersion > SourceSummaryFormatVersion {
		return fmt.Errorf("rag: upsert current source summary %q: stored format_version %d is newer than %d",
			row.Source, storedFormatVersion, SourceSummaryFormatVersion)
	}
	signature, validSignature := parseSourceSignature(minSignature)
	if count == 0 || minSignature != maxSignature || minVectorSpace != maxVectorSpace ||
		!validSignature || signature.ContentHash != row.ContentHash || minVectorSpace != row.VectorSpaceID {
		return fmt.Errorf("%w: %q", ErrSourceSummaryStale, row.Source)
	}
	if _, err := tx.ExecContext(ctx, upsertSourceSummarySQL,
		row.Source, row.ContentHash, row.VectorSpaceID, row.Abstract, row.Overview,
		row.SummaryModel, row.FormatVersion, row.SummarizedAt); err != nil {
		return fmt.Errorf("rag: upsert current source summary %q: %w", row.Source, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: upsert current source summary %q: commit: %w", row.Source, err)
	}
	return nil
}
