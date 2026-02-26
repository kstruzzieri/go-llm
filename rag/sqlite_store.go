package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	_ "modernc.org/sqlite"
)

// SQLiteStore is a VectorStore backed by SQLite with brute-force cosine similarity.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a vector store backed by SQLite.
// Use ":memory:" for dbPath to create an in-memory database.
func NewSQLiteStore(dbPath string) (VectorStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("rag: open sqlite: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("rag: set WAL mode: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS chunks (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		source TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}',
		embedding TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source);
	`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("rag: create schema: %w", err)
	}
	return nil
}

// Store saves chunks with their embeddings to SQLite.
func (s *SQLiteStore) Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error {
	if len(chunks) != len(embeddings) {
		return fmt.Errorf("rag: store: chunks/embeddings length mismatch (%d vs %d)", len(chunks), len(embeddings))
	}
	if len(chunks) == 0 {
		return nil
	}
	// Validate all embeddings have the same dimension
	dim := len(embeddings[0])
	if dim == 0 {
		return fmt.Errorf("rag: store: embedding dimension is 0")
	}
	for i, emb := range embeddings {
		if len(emb) != dim {
			return fmt.Errorf("rag: store: embedding dimension mismatch at index %d (expected %d, got %d)", i, dim, len(emb))
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rag: begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("rag: prepare insert: %w", err)
	}
	defer stmt.Close()

	for i, chunk := range chunks {
		metaJSON, err := json.Marshal(chunk.Metadata)
		if err != nil {
			return fmt.Errorf("rag: marshal metadata: %w", err)
		}
		embJSON, err := json.Marshal(embeddings[i])
		if err != nil {
			return fmt.Errorf("rag: marshal embedding: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, chunk.ID, chunk.Content, chunk.Source,
			chunk.StartLine, chunk.EndLine, chunk.Language, string(metaJSON), string(embJSON)); err != nil {
			return fmt.Errorf("rag: insert chunk %q: %w", chunk.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: commit: %w", err)
	}
	return nil
}

// Search finds the top-k most similar chunks using brute-force cosine similarity.
func (s *SQLiteStore) Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language, metadata, embedding FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("rag: query chunks: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var chunk Chunk
		var metaJSON, embJSON string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language, &metaJSON, &embJSON); err != nil {
			return nil, fmt.Errorf("rag: scan chunk: %w", err)
		}

		chunk.Metadata = make(map[string]string)
		json.Unmarshal([]byte(metaJSON), &chunk.Metadata)

		var embedding []float64
		if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
			return nil, fmt.Errorf("rag: unmarshal embedding: %w", err)
		}

		score := cosineSimilarity(queryEmbedding, embedding)
		results = append(results, SearchResult{
			Chunk:    chunk,
			Score:    score,
			Distance: 1 - score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate chunks: %w", err)
	}

	// Sort by score descending
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results, nil
}

// DeleteBySource removes all chunks with the given source path.
func (s *SQLiteStore) DeleteBySource(ctx context.Context, source string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chunks WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: delete by source %q: %w", source, err)
	}
	return nil
}

// Stats returns index statistics.
func (s *SQLiteStore) Stats(ctx context.Context) (StoreStats, error) {
	var stats StoreStats

	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&stats.TotalChunks)
	if err != nil {
		return stats, fmt.Errorf("rag: count chunks: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT source) FROM chunks`).Scan(&stats.TotalSources)
	if err != nil {
		return stats, fmt.Errorf("rag: count sources: %w", err)
	}

	// Get embedding dimension from first row
	var embJSON string
	err = s.db.QueryRowContext(ctx, `SELECT embedding FROM chunks LIMIT 1`).Scan(&embJSON)
	if err == nil {
		var emb []float64
		if json.Unmarshal([]byte(embJSON), &emb) == nil {
			stats.EmbeddingDim = len(emb)
		}
	}

	// Get database page count and page size for storage estimate
	var pageCount, pageSize int64
	if s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount) == nil {
		if s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize) == nil {
			stats.StorageBytes = pageCount * pageSize
		}
	}

	return stats, nil
}

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// cosineSimilarity computes cosine similarity between two vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += a[i] * b[i]
		magA += a[i] * a[i]
		magB += b[i] * b[i]
	}
	mag := math.Sqrt(magA) * math.Sqrt(magB)
	if mag == 0 {
		return 0
	}
	return dot / mag
}

// detectLanguageFromPath is available in chunker_code.go as detectLanguage.
