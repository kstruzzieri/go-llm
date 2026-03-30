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

	// Verify rag_schema_version exists with version 3.
	var maxVersion int
	err := store.db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query rag_schema_version: %v", err)
	}
	if maxVersion != 3 {
		t.Errorf("max schema version = %d, want 3", maxVersion)
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

	// Verify version upgraded to 3.
	var maxVersion int
	err = db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if maxVersion != 3 {
		t.Errorf("max schema version = %d, want 3", maxVersion)
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

func TestFTS5SyncOnReplaceSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store initial chunks.
	chunks := []Chunk{
		{ID: "c1", Content: "alpha bravo", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1.0, 0.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Verify FTS5 has "alpha".
	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count); err != nil {
		t.Fatalf("pre-replace FTS5 query: %v", err)
	}
	if count != 1 {
		t.Fatalf("pre-replace: expected 1 match for 'alpha', got %d", count)
	}

	// Replace with different content.
	replacement := []Chunk{
		{ID: "c2", Content: "charlie delta", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.ReplaceSource(ctx, "a.go", replacement, [][]float64{{0.0, 1.0}}); err != nil {
		t.Fatalf("ReplaceSource() error: %v", err)
	}

	// FTS5 should no longer match "alpha" (old content removed).
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count); err != nil {
		t.Fatalf("post-replace FTS5 query: %v", err)
	}
	if count != 0 {
		t.Errorf("post-replace: FTS5 MATCH 'alpha' returned %d rows, want 0", count)
	}

	// FTS5 should match "charlie" (new content).
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'charlie'`,
	).Scan(&count); err != nil {
		t.Fatalf("post-replace FTS5 query for charlie: %v", err)
	}
	if count != 1 {
		t.Errorf("post-replace: FTS5 MATCH 'charlie' returned %d rows, want 1", count)
	}
}

func TestMigrationIdempotency(t *testing.T) {
	// Create a fresh database and run migrations.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}

	// Running migrations again should be a no-op.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}

	// Verify version is still 3 (not duplicated).
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rag_schema_version`).Scan(&count); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 version records (v1, v2, v3), got %d", count)
	}
}

func TestFTS5UpsertConsistency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert a chunk with content "original alpha".
	chunks := []Chunk{
		{ID: "c1", Content: "original alpha", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1.0, 0.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Verify FTS5 finds "alpha".
	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count); err != nil {
		t.Fatalf("pre-upsert FTS5 query: %v", err)
	}
	if count != 1 {
		t.Fatalf("pre-upsert: expected 1 match for 'alpha', got %d", count)
	}

	// Upsert the same chunk ID with different content.
	updated := []Chunk{
		{ID: "c1", Content: "updated bravo", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, updated, [][]float64{{0.0, 1.0}}); err != nil {
		t.Fatalf("Store() upsert error: %v", err)
	}

	// FTS5 must NOT find "alpha" (old content removed).
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'`,
	).Scan(&count); err != nil {
		t.Fatalf("post-upsert FTS5 query for old term: %v", err)
	}
	if count != 0 {
		t.Errorf("post-upsert: FTS5 MATCH 'alpha' returned %d rows, want 0 (stale terms)", count)
	}

	// FTS5 must find "bravo" (new content indexed).
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH 'bravo'`,
	).Scan(&count); err != nil {
		t.Fatalf("post-upsert FTS5 query for new term: %v", err)
	}
	if count != 1 {
		t.Errorf("post-upsert: FTS5 MATCH 'bravo' returned %d rows, want 1", count)
	}

	// Verify content-sync reads don't fail (no "missing row from content table").
	rows, err := store.db.QueryContext(ctx,
		`SELECT rowid, content, source FROM chunks_fts WHERE chunks_fts MATCH 'bravo'`)
	if err != nil {
		t.Fatalf("content-sync read: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var content, source string
		if err := rows.Scan(&rowid, &content, &source); err != nil {
			t.Fatalf("content-sync scan: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("content-sync iterate: %v", err)
	}
}

func TestMigrationHalfAppliedV2(t *testing.T) {
	// Simulate a database where v2 DDL was applied but version was
	// not recorded (crash between tx.Commit and recordVersion in the
	// original non-atomic code path).
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// Create v2-shaped schema: chunks table with indexed_at + FTS5.
	v2Schema := `
	CREATE TABLE chunks (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		source TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}',
		embedding TEXT NOT NULL,
		indexed_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_chunks_source_line ON chunks(source, start_line);
	CREATE INDEX idx_chunks_lang_source_line ON chunks(language, source, start_line);

	CREATE VIRTUAL TABLE chunks_fts USING fts5(
		content, source,
		content=chunks, content_rowid=rowid
	);
	CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
		INSERT INTO chunks_fts(rowid, content, source)
		VALUES (new.rowid, new.content, new.source);
	END;
	CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN
		INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
		VALUES ('delete', old.rowid, old.content, old.source);
	END;
	CREATE TRIGGER chunks_au AFTER UPDATE ON chunks BEGIN
		INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
		VALUES ('delete', old.rowid, old.content, old.source);
		INSERT INTO chunks_fts(rowid, content, source)
		VALUES (new.rowid, new.content, new.source);
	END;
	`
	if _, err := db.Exec(v2Schema); err != nil {
		t.Fatalf("create v2 schema: %v", err)
	}

	// NO rag_schema_version table — simulates crash window.

	// runMigrations must succeed without "duplicate column name" error.
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() on half-applied v2: %v", err)
	}

	// Verify version recorded as 3 (v2 detected, then v3 applied).
	var maxVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if maxVersion != 3 {
		t.Errorf("max version = %d, want 3", maxVersion)
	}
}

func TestMigrationHalfAppliedV2WithV1Recorded(t *testing.T) {
	// Simulate the gap identified in review: v2-shaped schema but only
	// version 1 is recorded (crash between first and second recordVersion
	// in the previous non-atomic repair code).
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// Create v2-shaped schema.
	v2Schema := `
	CREATE TABLE chunks (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		source TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}',
		embedding TEXT NOT NULL,
		indexed_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_chunks_source_line ON chunks(source, start_line);
	CREATE INDEX idx_chunks_lang_source_line ON chunks(language, source, start_line);
	CREATE VIRTUAL TABLE chunks_fts USING fts5(
		content, source,
		content=chunks, content_rowid=rowid
	);
	CREATE TRIGGER chunks_ai AFTER INSERT ON chunks BEGIN
		INSERT INTO chunks_fts(rowid, content, source)
		VALUES (new.rowid, new.content, new.source);
	END;
	CREATE TRIGGER chunks_ad AFTER DELETE ON chunks BEGIN
		INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
		VALUES ('delete', old.rowid, old.content, old.source);
	END;
	CREATE TRIGGER chunks_au AFTER UPDATE ON chunks BEGIN
		INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
		VALUES ('delete', old.rowid, old.content, old.source);
		INSERT INTO chunks_fts(rowid, content, source)
		VALUES (new.rowid, new.content, new.source);
	END;

	-- Version table exists but only v1 is recorded (simulates partial repair crash).
	CREATE TABLE rag_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	);
	INSERT INTO rag_schema_version (version, description, applied_at) VALUES (1, 'baseline', 1000);
	`
	if _, err := db.Exec(v2Schema); err != nil {
		t.Fatalf("create v2 schema with v1 recorded: %v", err)
	}

	// runMigrations must succeed without "duplicate column name" error.
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() on v2 schema with v1 recorded: %v", err)
	}

	// Verify version is now 3 (v2 detected, then v3 applied).
	var maxVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if maxVersion != 3 {
		t.Errorf("max version = %d, want 3", maxVersion)
	}
}
