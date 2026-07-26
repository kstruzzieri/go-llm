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
// provenance and summary reads.
func (s *SQLiteStore) ChunkContentDigestBatch(ctx context.Context, chunkIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return out, nil
	}
	query, args := inClauseQuery(`SELECT id, content FROM chunks WHERE id IN (%s)`, chunkIDs)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
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
