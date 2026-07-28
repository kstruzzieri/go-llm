package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

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
	if err := forEachSourceBatch(sources, func(batch []string) error {
		query, args := inClauseQuery(`
			SELECT source,
			       COALESCE(MIN(source_content_hash), ''), COALESCE(MAX(source_content_hash), ''),
			       COALESCE(MIN(vector_space_id), ''), COALESCE(MAX(vector_space_id), ''),
			       MAX(indexed_at)
			FROM chunks WHERE source IN (%s) GROUP BY source`, batch)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("rag: source provenance batch: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p SourceProvenance
			var minSig, maxSig, minVS, maxVS string
			if err := rows.Scan(&p.Source, &minSig, &maxSig, &minVS, &maxVS, &p.IndexedAt); err != nil {
				return fmt.Errorf("rag: scan source provenance: %w", err)
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
			return fmt.Errorf("rag: iterate source provenance: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
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
	err := forEachSourceBatch(sources, func(batch []string) error {
		return s.attachManagedProvenanceBatch(ctx, batch, out)
	})
	if errors.Is(err, errManagedTableAbsent) {
		return nil
	}
	return err
}

// errManagedTableAbsent unwinds attachManagedProvenance's per-batch loop when a
// read-only pre-v6 database has no managed_documents table. Control signal
// only; the degraded result is "nothing is managed" and a nil error. The table
// cannot appear between batches, so skipping one skips them all.
var errManagedTableAbsent = errors.New("rag: managed_documents table absent (read-only degrade)")

func (s *SQLiteStore) attachManagedProvenanceBatch(ctx context.Context, sources []string, out map[string]SourceProvenance) error {
	query, args := inClauseQuery(`
		SELECT source, title, collection, tags, freshness
		FROM managed_documents WHERE source IN (%s)`, sources)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		if s.immutable && isMissingTableErr(err, "managed_documents") {
			// Pre-v6 database opened read-only: nothing is managed. A missing
			// registry on a writable store is corruption and propagates.
			return errManagedTableAbsent
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
			// Degrade, don't propagate. Managed/Title/Collection/Freshness
			// above already scanned successfully, so the source genuinely
			// IS managed — setting Managed=false here would assert
			// something false. The closer precedent is the retrieval-path
			// validity predicate at rag/retriever.go:702, which treats
			// malformed tags as "not valid" and moves on, not the
			// authoritative document read at rag/managed.go:733-734, which
			// propagates: that call sits on the read/write path for one
			// document, this one sits on the render path for a whole batch.
			// One corrupt tags value must not fail progressive rendering for
			// every OTHER healthy source in the batch — the entire point of
			// this feature is per-source degradation with reasons, and
			// "managed with no tags" is already representable.
			tags = nil
		}
		if tags == nil {
			// rag/managed.go:736-738 normalizes the same column the same
			// way: a nil slice reads fine through len()/range in Go, but
			// serializes as JSON null instead of [], which the eventual
			// ProgressiveTrace marshaling (#189 Task 11) would surface.
			tags = []string{}
		}
		p.Tags = tags
		out[source] = p
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rag: iterate managed provenance: %w", err)
	}
	return nil
}
