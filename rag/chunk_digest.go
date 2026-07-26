package rag

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// sha256Hex returns the lowercase hex SHA-256 of s. It is the evidence
// integrity digest for the retrieval/render race check (spec section 8):
// chunk IDs alone cannot be trusted because custom chunkers may reuse IDs
// for changed content.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:])
}

// ChunkContentDigestBatch returns sha256Hex of the STORED content for each
// requested chunk ID. Missing IDs are absent from the map. Bounded by the
// number of retrieved results, not corpus size. In the renderer's read
// sequence this runs LAST (spec section 8's read-order contract): the digest
// comparison is the ground truth that catches any reindex racing the earlier
// provenance and summary reads. Reversing the order reopens the race window
// — a digest read before those earlier reads could pass, and a reindex that
// then lands in the gap would go unobserved, since nothing later re-checks it.
//
// Callers must pass every chunk ID being rendered, not a budget-limited
// subset: the earlier provenance read is non-transactional and is only safe
// under the assumption that this digest covers all of it, so a partial
// chunkIDs list silently narrows race detection to just the chunks it names.
func (s *SQLiteStore) ChunkContentDigestBatch(ctx context.Context, chunkIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	query, args := inClauseQuery(`SELECT id, content FROM chunks WHERE id IN (%s)`, chunkIDs)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		// No isMissingTableErr degradation here, unlike SourceSummaryBatch and
		// attachManagedProvenance: chunks has existed since schema v1, so a
		// missing chunks table is never a legitimately-older snapshot — it is
		// unambiguous corruption and must propagate.
		return nil, fmt.Errorf("rag: chunk digest batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("rag: scan chunk digest: %w", err)
		}
		out[id] = sha256Hex(content)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate chunk digests: %w", err)
	}
	return out, nil
}
