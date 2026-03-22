package rag

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrationFreshDB(t *testing.T) {
	store := newTestStore(t)

	// Verify rag_schema_version exists with version 2.
	var maxVersion int
	err := store.db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query rag_schema_version: %v", err)
	}
	if maxVersion != 2 {
		t.Errorf("max schema version = %d, want 2", maxVersion)
	}

	// Verify chunks_fts virtual table exists.
	var ftsCount int
	err = store.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks_fts'`,
	).Scan(&ftsCount)
	if err != nil {
		t.Fatalf("query chunks_fts existence: %v", err)
	}
	if ftsCount != 1 {
		t.Error("chunks_fts virtual table does not exist")
	}

	// Verify indexed_at column exists by inserting and querying it.
	_, err = store.db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at)
		 VALUES ('test', 'content', 'src.go', 1, 1, 'go', '{}', '[]', 12345)`,
	)
	if err != nil {
		t.Fatalf("insert with indexed_at: %v", err)
	}

	var indexedAt int64
	err = store.db.QueryRow(`SELECT indexed_at FROM chunks WHERE id = 'test'`).Scan(&indexedAt)
	if err != nil {
		t.Fatalf("query indexed_at: %v", err)
	}
	if indexedAt != 12345 {
		t.Errorf("indexed_at = %d, want 12345", indexedAt)
	}
}

func TestMigrationExistingDB(t *testing.T) {
	// Simulate a pre-migration database: create old schema manually.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// Create the old schema (no rag_schema_version, no indexed_at, no FTS5).
	oldSchema := `
	CREATE TABLE chunks (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		source TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}',
		embedding TEXT NOT NULL
	);
	CREATE INDEX idx_chunks_source_line ON chunks(source, start_line);
	CREATE INDEX idx_chunks_lang_source_line ON chunks(language, source, start_line);
	`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}

	// Insert test data without indexed_at column.
	_, err = db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding)
		 VALUES ('c1', 'hello world', 'main.go', 1, 5, 'go', '{}', '[0.1, 0.2, 0.3]')`,
	)
	if err != nil {
		t.Fatalf("insert test data: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding)
		 VALUES ('c2', 'goodbye world', 'util.go', 1, 3, 'go', '{}', '[0.4, 0.5, 0.6]')`,
	)
	if err != nil {
		t.Fatalf("insert test data: %v", err)
	}

	// Run migrations on the pre-existing database.
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	// Verify version upgraded to 2.
	var maxVersion int
	err = db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if maxVersion != 2 {
		t.Errorf("max schema version = %d, want 2", maxVersion)
	}

	// Verify indexed_at was backfilled (should be non-zero).
	rows, err := db.Query(`SELECT id, indexed_at FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatalf("query indexed_at: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var indexedAt int64
		if err := rows.Scan(&id, &indexedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if indexedAt == 0 {
			t.Errorf("chunk %q indexed_at not backfilled (still 0)", id)
		}
	}

	// Verify FTS5 works with a MATCH query.
	var matchCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'hello'`).Scan(&matchCount)
	if err != nil {
		t.Fatalf("FTS5 MATCH query: %v", err)
	}
	if matchCount != 1 {
		t.Errorf("FTS5 MATCH 'hello' returned %d rows, want 1", matchCount)
	}
}

func TestIndexedAtWrittenOnStore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	before := time.Now().Unix()

	chunks := []Chunk{
		{ID: "c1", Content: "hello", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "world", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	after := time.Now().Unix()

	rows, err := store.db.QueryContext(ctx, `SELECT id, indexed_at FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatalf("query indexed_at: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var indexedAt int64
		if err := rows.Scan(&id, &indexedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if indexedAt < before || indexedAt > after {
			t.Errorf("chunk %q indexed_at = %d, want between %d and %d", id, indexedAt, before, after)
		}
	}
}

func TestIndexedAtWrittenOnReplace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store initial chunks.
	initial := []Chunk{
		{ID: "c1", Content: "original", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, initial, [][]float64{{1.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Small delay so timestamps differ.
	before := time.Now().Unix()

	replacement := []Chunk{
		{ID: "c2", Content: "replacement", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.ReplaceSource(ctx, "a.go", replacement, [][]float64{{0.5}}); err != nil {
		t.Fatalf("ReplaceSource() error: %v", err)
	}

	after := time.Now().Unix()

	var indexedAt int64
	err := store.db.QueryRowContext(ctx, `SELECT indexed_at FROM chunks WHERE id = 'c2'`).Scan(&indexedAt)
	if err != nil {
		t.Fatalf("query indexed_at: %v", err)
	}
	if indexedAt < before || indexedAt > after {
		t.Errorf("replaced chunk indexed_at = %d, want between %d and %d", indexedAt, before, after)
	}
}

func TestDBMethod(t *testing.T) {
	store := newTestStore(t)
	db := store.DB()
	if db == nil {
		t.Fatal("DB() returned nil")
	}

	// Verify the returned *sql.DB can execute queries.
	var result int
	if err := db.QueryRow(`SELECT 1`).Scan(&result); err != nil {
		t.Fatalf("DB().QueryRow() error: %v", err)
	}
	if result != 1 {
		t.Errorf("DB() query returned %d, want 1", result)
	}
}

func TestFTS5SyncOnInsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "the quick brown fox jumps", Source: "animals.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "lazy dog sleeps", Source: "animals.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Verify FTS5 contains the inserted content.
	tests := []struct {
		query string
		want  int
	}{
		{"fox", 1},
		{"dog", 1},
		{"quick", 1},
		{"nonexistent", 0},
		{`"animals"`, 2}, // source column is also indexed (quoted to avoid FTS5 syntax issues with dots)
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			var count int
			err := store.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, tt.query,
			).Scan(&count)
			if err != nil {
				t.Fatalf("FTS5 MATCH %q: %v", tt.query, err)
			}
			if count != tt.want {
				t.Errorf("FTS5 MATCH %q returned %d rows, want %d", tt.query, count, tt.want)
			}
		})
	}
}

func TestFTS5SyncOnDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "alpha bravo", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "charlie delta", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Verify both are in FTS5 before deletion.
	var count int
	err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("pre-delete FTS5 query: %v", err)
	}
	if count != 1 {
		t.Fatalf("pre-delete: expected 1 match for 'alpha', got %d", count)
	}

	// Delete source a.go.
	if err := store.DeleteBySource(ctx, "a.go"); err != nil {
		t.Fatalf("DeleteBySource() error: %v", err)
	}

	// FTS5 should no longer match 'alpha'.
	err = store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("post-delete FTS5 query: %v", err)
	}
	if count != 0 {
		t.Errorf("post-delete: FTS5 MATCH 'alpha' returned %d rows, want 0", count)
	}

	// 'charlie' from b.go should still be there.
	err = store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'charlie'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("post-delete FTS5 query for charlie: %v", err)
	}
	if count != 1 {
		t.Errorf("post-delete: FTS5 MATCH 'charlie' returned %d rows, want 1", count)
	}
}
