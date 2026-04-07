package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Compile-time interface satisfaction checks.
var (
	_ VectorStore       = (*SQLiteStore)(nil)
	_ Exportable        = (*SQLiteStore)(nil)
	_ sourceChunkLoader = (*SQLiteStore)(nil)
	_ sourceHashChecker = (*SQLiteStore)(nil)
)

// SQLiteStore is a VectorStore backed by SQLite with brute-force cosine similarity.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a vector store backed by SQLite.
// Use ":memory:" for dbPath to create an in-memory database (for testing).
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("rag: open sqlite: %w", err)
	}

	if dbPath == ":memory:" {
		// In-memory databases: constrain to exactly 1 connection.
		// With database/sql's connection pool, multiple connections to :memory:
		// each create a separate database, causing missing schema/data.
		db.SetMaxOpenConns(1)
	} else {
		// File-backed databases: enable WAL mode for better concurrent read performance.
		// WAL is not meaningful for :memory: databases.
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("rag: set WAL mode: %w", err)
		}
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

// DB returns the underlying *sql.DB for packages that need shared access
// to the workspace database (e.g., conversation/ and feedback/ creating
// their own tables).
func (s *SQLiteStore) DB() *sql.DB {
	return s.db
}

func validateStoreInputs(chunks []Chunk, embeddings [][]float64) error {
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
	return nil
}

func (s *SQLiteStore) insertChunksTx(ctx context.Context, tx *sql.Tx, chunks []Chunk, embeddings [][]float64) error {
	// Use ON CONFLICT ... DO UPDATE instead of INSERT OR REPLACE.
	// INSERT OR REPLACE deletes the old row and inserts a new one, but
	// SQLite does not fire DELETE triggers for rows removed by REPLACE
	// conflict resolution (unless recursive_triggers is enabled). This
	// leaves stale entries in the FTS5 index. ON CONFLICT DO UPDATE
	// modifies the row in place (preserving its rowid) and fires the
	// AFTER UPDATE trigger, which correctly maintains FTS5.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
				content = excluded.content,
				source = excluded.source,
				start_line = excluded.start_line,
				end_line = excluded.end_line,
				language = excluded.language,
				metadata = excluded.metadata,
				embedding = excluded.embedding,
				indexed_at = excluded.indexed_at,
				stable_key = excluded.stable_key`)
	if err != nil {
		return fmt.Errorf("rag: prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().Unix()
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
			chunk.StartLine, chunk.EndLine, chunk.Language, string(metaJSON), string(embJSON), now, chunk.StableKey); err != nil {
			return fmt.Errorf("rag: insert chunk %q: %w", chunk.ID, err)
		}
	}
	return nil
}

// Store saves chunks with their embeddings to SQLite.
func (s *SQLiteStore) Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error {
	if err := validateStoreInputs(chunks, embeddings); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rag: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.insertChunksTx(ctx, tx, chunks, embeddings); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: commit: %w", err)
	}
	return nil
}

// ReplaceSource atomically replaces all chunks for a source path.
// If insertion fails, the delete is rolled back and existing data is preserved.
func (s *SQLiteStore) ReplaceSource(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64) error {
	if err := validateStoreInputs(chunks, embeddings); err != nil {
		return fmt.Errorf("rag: replace source %q: %w", source, err)
	}
	for i, chunk := range chunks {
		if chunk.Source != source {
			return fmt.Errorf("rag: replace source %q: chunk %d has source %q", source, i, chunk.Source)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rag: replace source %q: begin transaction: %w", source, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: replace source %q: delete existing chunks: %w", source, err)
	}

	if len(chunks) > 0 {
		if err := s.insertChunksTx(ctx, tx, chunks, embeddings); err != nil {
			return fmt.Errorf("rag: replace source %q: %w", source, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: replace source %q: commit: %w", source, err)
	}
	return nil
}

// Search finds the top-k most similar chunks using brute-force cosine similarity.
func (s *SQLiteStore) Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language, metadata, embedding, stable_key FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("rag: query chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SearchResult
	for rows.Next() {
		var chunk Chunk
		var metaJSON, embJSON string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language, &metaJSON, &embJSON, &chunk.StableKey); err != nil {
			return nil, fmt.Errorf("rag: scan chunk: %w", err)
		}

		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metaJSON), &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
		}

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

// GetBySource returns all chunks for a given source path, ordered by start_line,
// with their embeddings. This supports incremental indexing by allowing comparison
// of existing chunks against newly generated ones.
func (s *SQLiteStore) GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language,
		        metadata, embedding, stable_key
		 FROM chunks WHERE source = ?
		 ORDER BY start_line`, source)
	if err != nil {
		return nil, fmt.Errorf("rag: get by source %q: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	var results []ChunkWithEmbedding
	for rows.Next() {
		var chunk Chunk
		var metaJSON, embJSON string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language,
			&metaJSON, &embJSON, &chunk.StableKey); err != nil {
			return nil, fmt.Errorf("rag: scan chunk for source %q: %w", source, err)
		}

		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metaJSON), &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
		}

		var embedding []float64
		if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
			return nil, fmt.Errorf("rag: unmarshal embedding for chunk %q: %w", chunk.ID, err)
		}

		results = append(results, ChunkWithEmbedding{
			Chunk:     chunk,
			Embedding: embedding,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate chunks for source %q: %w", source, err)
	}
	return results, nil
}

// GetSourceHash returns the stored content hash for a source path, or empty
// string if the source has no chunks or no hash is stored. This enables fast
// file-level change detection during incremental indexing.
func (s *SQLiteStore) GetSourceHash(ctx context.Context, source string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT source_content_hash FROM chunks WHERE source = ? LIMIT 1`,
		source).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rag: get source hash for %q: %w", source, err)
	}
	return hash, nil
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

// ExportChunks implements Exportable by streaming all matching chunks from SQLite.
// The returned iterator yields one ExportedChunk at a time, with cleanup handled
// automatically when iteration stops.
func (s *SQLiteStore) ExportChunks(ctx context.Context, filter *ExportFilter) (iter.Seq2[ExportedChunk, error], error) {
	query, args := buildExportQuery(filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rag: export chunks: %w", err)
	}
	return func(yield func(ExportedChunk, error) bool) {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				yield(ExportedChunk{}, err)
				return
			}
			chunk, embedding, err := scanExportRow(rows)
			if err != nil {
				yield(ExportedChunk{}, err)
				return
			}
			if !yield(ExportedChunk{Chunk: chunk, Embedding: embedding}, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(ExportedChunk{}, fmt.Errorf("rag: iterate export chunks: %w", err))
		}
	}, nil
}

// buildExportQuery constructs the SELECT with optional WHERE clauses for export filtering.
func buildExportQuery(filter *ExportFilter) (string, []any) {
	base := `SELECT id, content, source, start_line, end_line, language, embedding, stable_key FROM chunks`
	var conditions []string
	var args []any

	if filter != nil {
		if filter.SourcePattern != "" {
			conditions = append(conditions, "source GLOB ?")
			args = append(args, filter.SourcePattern)
		}
		if filter.Language != "" {
			conditions = append(conditions, "language = ?")
			args = append(args, filter.Language)
		}
	}

	if len(conditions) > 0 {
		base += " WHERE " + strings.Join(conditions, " AND ")
	}
	base += " ORDER BY source, start_line"
	return base, args
}

// scanExportRow scans a row from the export query into a Chunk and embedding.
// Metadata is intentionally not selected — it's excluded from the Parquet schema.
func scanExportRow(rows *sql.Rows) (Chunk, []float64, error) {
	var chunk Chunk
	var embJSON string
	if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
		&chunk.StartLine, &chunk.EndLine, &chunk.Language, &embJSON, &chunk.StableKey); err != nil {
		return Chunk{}, nil, fmt.Errorf("rag: scan export row: %w", err)
	}

	var embedding []float64
	if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
		return Chunk{}, nil, fmt.Errorf("rag: unmarshal export embedding: %w", err)
	}

	return chunk, embedding, nil
}
