package rag

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrationFreshDB(t *testing.T) {
	store := newTestStore(t)

	// Verify rag_schema_version exists with the latest applied version.
	var maxVersion int
	err := store.db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query rag_schema_version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max schema version = %d, want 7", maxVersion)
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

func TestMigrationFreshDBAddsManagedDocumentRevision(t *testing.T) {
	store := newTestStore(t)
	var columns int
	if err := store.db.QueryRow(`
		SELECT COUNT(*)
		  FROM pragma_table_info('managed_documents')
		 WHERE name = 'revision' AND type = 'INTEGER' AND "notnull" = 1 AND dflt_value = '1'`,
	).Scan(&columns); err != nil {
		t.Fatalf("query revision column: %v", err)
	}
	if columns != 1 {
		t.Fatalf("managed revision columns = %d, want INTEGER NOT NULL DEFAULT 1", columns)
	}
}

func TestMigrationV7BackfillsManagedDocumentRevision(t *testing.T) {
	db := openV4DB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV5(tx); err != nil {
		t.Fatal(err)
	}
	if err := migrateV6(tx); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{5, 6} {
		if _, err := tx.Exec(
			`INSERT INTO rag_schema_version (version, description, applied_at) VALUES (?, 'fixture', 1000)`,
			version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO managed_documents (
			id, source, title, kind, mime_type, content_hash, source_signature,
			collection, tags, state, freshness, created_at, updated_at
		) VALUES ('doc', 'managed:doc', 'title', 'text', 'text/plain', 'hash',
		          'signature', '', '[]', 'indexed', 'fresh', 1000, 1000)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}
	var revision int64
	if err := db.QueryRow(`SELECT revision FROM managed_documents WHERE id = 'doc'`).Scan(&revision); err != nil {
		t.Fatalf("query migrated revision: %v", err)
	}
	if revision != 1 {
		t.Fatalf("migrated revision = %d, want 1", revision)
	}
}

func TestNewSQLiteStoreConcurrentFreshMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("seed WAL mode: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	const openers = 32
	start := make(chan struct{})
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := NewSQLiteStore(path)
			if err == nil {
				err = store.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent NewSQLiteStore() error: %v", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if version != migrations[len(migrations)-1].version {
		t.Fatalf("schema version = %d, want %d", version, migrations[len(migrations)-1].version)
	}
	var managedTable int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'managed_documents'`).Scan(&managedTable); err != nil {
		t.Fatalf("query managed_documents: %v", err)
	}
	if managedTable != 1 {
		t.Fatal("managed_documents table missing after concurrent migration")
	}
}

func TestMigrationExistingDB(t *testing.T) {
	// Simulate a pre-migration database: create old schema manually.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

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

	// Verify version upgraded to the latest applied version.
	var maxVersion int
	err = db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max schema version = %d, want 7", maxVersion)
	}

	// Verify indexed_at was backfilled (should be non-zero).
	rows, err := db.Query(`SELECT id, indexed_at FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatalf("query indexed_at: %v", err)
	}
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()

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
	defer func() { _ = db.Close() }()

	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}

	// Running migrations again should be a no-op.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}

	// Verify version count is unchanged after re-run (no duplicates).
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rag_schema_version`).Scan(&count); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if count != 7 {
		t.Errorf("expected 7 version records (v1..v7), got %d", count)
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
	defer func() { _ = rows.Close() }()
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

func TestMigrationV4SourceContentHash(t *testing.T) {
	store := newTestStore(t)

	// Verify max schema version is the latest applied version.
	var maxVersion int
	err := store.db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query rag_schema_version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max schema version = %d, want 7", maxVersion)
	}

	// Verify source_content_hash column exists by inserting and querying it.
	_, err = store.db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash)
		 VALUES ('test-v4', 'content', 'src.go', 1, 1, 'go', '{}', '[]', 12345, 'key1', 'abc123')`,
	)
	if err != nil {
		t.Fatalf("insert with source_content_hash: %v", err)
	}

	var hash string
	err = store.db.QueryRow(`SELECT source_content_hash FROM chunks WHERE id = 'test-v4'`).Scan(&hash)
	if err != nil {
		t.Fatalf("query source_content_hash: %v", err)
	}
	if hash != "abc123" {
		t.Errorf("source_content_hash = %q, want %q", hash, "abc123")
	}
}

func TestMigrationV4ExistingDB(t *testing.T) {
	// Simulate a V3-shaped database: chunks table with stable_key but NOT source_content_hash.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	// Create V3-shaped schema with stable_key, indexed_at, FTS5, and triggers.
	v3Schema := `
	CREATE TABLE chunks (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		source TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		language TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}',
		embedding TEXT NOT NULL,
		indexed_at INTEGER NOT NULL DEFAULT 0,
		stable_key TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_chunks_source_line ON chunks(source, start_line);
	CREATE INDEX idx_chunks_lang_source_line ON chunks(language, source, start_line);
	CREATE INDEX idx_chunks_stable_key ON chunks(stable_key);

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

	CREATE TABLE rag_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	);
	INSERT INTO rag_schema_version (version, description, applied_at) VALUES (1, 'baseline schema', 1000);
	INSERT INTO rag_schema_version (version, description, applied_at) VALUES (2, 'add indexed_at, FTS5 index, and sync triggers', 1000);
	INSERT INTO rag_schema_version (version, description, applied_at) VALUES (3, 'add stable_key column for chunk identity', 1000);
	`
	if _, err := db.Exec(v3Schema); err != nil {
		t.Fatalf("create v3 schema: %v", err)
	}

	// Insert a row without source_content_hash column.
	_, err = db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key)
		 VALUES ('c1', 'hello world', 'main.go', 1, 5, 'go', '{}', '[0.1, 0.2]', 12345, 'key1')`,
	)
	if err != nil {
		t.Fatalf("insert test data: %v", err)
	}

	// Run migrations on the V3 database.
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	// Verify version upgraded to the latest applied version.
	var maxVersion int
	err = db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion)
	if err != nil {
		t.Fatalf("query schema version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max schema version = %d, want 7", maxVersion)
	}

	// Verify existing row has empty default hash.
	var hash string
	err = db.QueryRow(`SELECT source_content_hash FROM chunks WHERE id = 'c1'`).Scan(&hash)
	if err != nil {
		t.Fatalf("query source_content_hash: %v", err)
	}
	if hash != "" {
		t.Errorf("existing row source_content_hash = %q, want empty string", hash)
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
	defer func() { _ = db.Close() }()

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

	// Verify version recorded at latest (v2 detected, then v3+ applied).
	var maxVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max version = %d, want 7", maxVersion)
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
	defer func() { _ = db.Close() }()

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

	// Verify version is now at latest (v2 detected, then v3+ applied).
	var maxVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max version = %d, want 7", maxVersion)
	}
}

// v4Schema is the chunks-table shape after migrations 1-4. Used by the v5
// migration tests to set up a pre-v5 fixture.
const v4Schema = `
CREATE TABLE chunks (
	id TEXT PRIMARY KEY,
	content TEXT NOT NULL,
	source TEXT NOT NULL,
	start_line INTEGER NOT NULL,
	end_line INTEGER NOT NULL,
	language TEXT NOT NULL DEFAULT '',
	metadata TEXT NOT NULL DEFAULT '{}',
	embedding TEXT NOT NULL,
	indexed_at INTEGER NOT NULL DEFAULT 0,
	stable_key TEXT NOT NULL DEFAULT '',
	source_content_hash TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_chunks_source_line ON chunks(source, start_line);
CREATE INDEX idx_chunks_lang_source_line ON chunks(language, source, start_line);
CREATE INDEX idx_chunks_stable_key ON chunks(stable_key);
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
CREATE TABLE rag_schema_version (
	version     INTEGER PRIMARY KEY,
	description TEXT NOT NULL,
	applied_at  INTEGER NOT NULL
);
INSERT INTO rag_schema_version (version, description, applied_at) VALUES (1, 'baseline schema', 1000);
INSERT INTO rag_schema_version (version, description, applied_at) VALUES (2, 'add indexed_at, FTS5 index, and sync triggers', 1000);
INSERT INTO rag_schema_version (version, description, applied_at) VALUES (3, 'add stable_key column for chunk identity', 1000);
INSERT INTO rag_schema_version (version, description, applied_at) VALUES (4, 'add source_content_hash for incremental indexing', 1000);
`

// openV4DB returns an in-memory SQLite handle pre-loaded with the v4 schema
// fixture. Single MaxOpenConns keeps schema state on one connection.
func openV4DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(v4Schema); err != nil {
		t.Fatalf("create v4 schema: %v", err)
	}
	return db
}

func TestMigration_v4_to_v5_addsColumn(t *testing.T) {
	db := openV4DB(t)

	// Pre-existing v4 row, written without knowledge of vector_space_id.
	if _, err := db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash)
		 VALUES ('legacy-1', 'hello', 'main.go', 1, 5, 'go', '{}', '[0.1]', 12345, 'sk1', 'h1')`,
	); err != nil {
		t.Fatalf("seed v4 row: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	var maxVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if maxVersion != 7 {
		t.Errorf("max schema version = %d, want 7", maxVersion)
	}

	rows, err := db.Query(`PRAGMA table_info(chunks)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		found   bool
		colType string
		notNull int
		dfltVal sql.NullString
	)
	for rows.Next() {
		var (
			cid   int
			name  string
			ctype string
			nn    int
			dflt  sql.NullString
			pk    int
		)
		if err := rows.Scan(&cid, &name, &ctype, &nn, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		if name == "vector_space_id" {
			found = true
			colType = ctype
			notNull = nn
			dfltVal = dflt
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma rows.Err: %v", err)
	}
	if !found {
		t.Fatal("vector_space_id column missing after v5 migration")
	}
	if colType != "TEXT" {
		t.Errorf("vector_space_id type = %q, want TEXT", colType)
	}
	if notNull != 1 {
		t.Errorf("vector_space_id NOT NULL = %d, want 1", notNull)
	}
	// SQLite stores the default literal verbatim, including quotes.
	if !dfltVal.Valid || dfltVal.String != "''" {
		t.Errorf("vector_space_id default = %v, want ''", dfltVal)
	}

	var vsid string
	if err := db.QueryRow(
		`SELECT vector_space_id FROM chunks WHERE id = 'legacy-1'`,
	).Scan(&vsid); err != nil {
		t.Fatalf("scan vector_space_id: %v", err)
	}
	if vsid != "" {
		t.Errorf("legacy row vector_space_id = %q, want \"\"", vsid)
	}
}

func TestMigration_v4_to_v5_partialIndex(t *testing.T) {
	db := openV4DB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	rowsToInsert := []struct {
		id, vsid string
	}{
		{"a", ""},
		{"b", "ollama/qwen3-embedding:8b"},
		{"c", "ollama/nomic-embed-text"},
		{"d", ""},
	}
	for _, r := range rowsToInsert {
		if _, err := db.Exec(
			`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			 VALUES (?, 'x', 's.go', 1, 1, 'go', '{}', '[0.1]', 1, '', '', ?)`,
			r.id, r.vsid,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	planRows, err := db.Query(`EXPLAIN QUERY PLAN
		SELECT vector_space_id FROM chunks
		 WHERE vector_space_id <> ''
		 ORDER BY vector_space_id ASC
		 LIMIT 1`)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = planRows.Close() }()

	var planText string
	for planRows.Next() {
		var (
			id     int
			parent int
			notUsd int
			detail string
		)
		if err := planRows.Scan(&id, &parent, &notUsd, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		planText += detail + "\n"
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("plan rows.Err: %v", err)
	}
	if !strings.Contains(planText, "idx_chunks_vector_space_id_nonempty") {
		t.Errorf("query plan does not reference idx_chunks_vector_space_id_nonempty:\n%s", planText)
	}

	var nonEmptyCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM chunks WHERE vector_space_id <> ''`,
	).Scan(&nonEmptyCount); err != nil {
		t.Fatalf("count non-empty: %v", err)
	}
	if nonEmptyCount != 2 {
		t.Errorf("non-empty count = %d, want 2", nonEmptyCount)
	}
}

// TestMigration_v5_partialIndex_descBookend pairs with TestMigration_v4_to_v5_partialIndex
// to verify both ASC and DESC bookend probes serve from the partial index.
// ProbeVectorSpaces relies on this for its lex-min/lex-max single-index-entry
// reads; without coverage of the DESC plan, a planner regression could
// silently turn the max-bookend into a full index scan.
func TestMigration_v5_partialIndex_descBookend(t *testing.T) {
	db := openV4DB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	// Insert in non-alphabetical order so DESC plan is meaningful.
	for _, r := range []struct{ id, vsid string }{
		{"a", "B"},
		{"b", "A"},
	} {
		if _, err := db.Exec(
			`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			 VALUES (?, 'x', 's.go', 1, 1, 'go', '{}', '[0.1]', 1, '', '', ?)`,
			r.id, r.vsid,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	planRows, err := db.Query(`EXPLAIN QUERY PLAN
		SELECT vector_space_id FROM chunks
		 WHERE vector_space_id <> ''
		 ORDER BY vector_space_id DESC
		 LIMIT 1`)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = planRows.Close() }()

	var planText string
	for planRows.Next() {
		var (
			id     int
			parent int
			notUsd int
			detail string
		)
		if err := planRows.Scan(&id, &parent, &notUsd, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		planText += detail + "\n"
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("plan rows.Err: %v", err)
	}
	if !strings.Contains(planText, "idx_chunks_vector_space_id_nonempty") {
		t.Errorf("DESC bookend plan does not reference idx_chunks_vector_space_id_nonempty:\n%s", planText)
	}
}

func TestMigrationV6PreservesChunksAndCreatesManagedRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	if _, err := db.Exec(v4Schema); err != nil {
		t.Fatalf("create v4 schema: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin v5 migration: %v", err)
	}
	if err := migrateV5(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrate v5: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO rag_schema_version (version, description, applied_at) VALUES (5, 'v5', 1000)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("record v5: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit v5: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO chunks (
			id, content, source, start_line, end_line, language, metadata,
			embedding, indexed_at, stable_key, source_content_hash, vector_space_id
		) VALUES ('legacy', 'preserve me', 'legacy.md', 1, 1, 'markdown', '{}', '[1]', 1000, '', 'hash', '')
	`); err != nil {
		t.Fatalf("seed v5 chunk: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v5 db: %v", err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var version int
	if err := store.db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&version); err != nil {
		t.Fatalf("query version: %v", err)
	}
	if version != 7 {
		t.Fatalf("schema version = %d, want 7", version)
	}
	chunks, err := store.GetBySource(context.Background(), "legacy.md")
	if err != nil {
		t.Fatalf("GetBySource() error: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Chunk.Content != "preserve me" {
		t.Fatalf("legacy chunks = %#v, want preserved row", chunks)
	}
	var managed int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM managed_documents`).Scan(&managed); err != nil {
		t.Fatalf("query managed registry: %v", err)
	}
	if managed != 0 {
		t.Fatalf("managed registry rows = %d, want 0", managed)
	}
	var chunkCountColumn int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('managed_documents') WHERE name = 'chunk_count'`).Scan(&chunkCountColumn); err != nil {
		t.Fatalf("query managed chunk_count column: %v", err)
	}
	if chunkCountColumn != 1 {
		t.Fatalf("managed chunk_count columns = %d, want 1", chunkCountColumn)
	}
}

func TestMigration_v5_open_idempotent(t *testing.T) {
	db := openV4DB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}
	var firstVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM rag_schema_version`).Scan(&firstVersion); err != nil {
		t.Fatalf("query first version: %v", err)
	}
	if firstVersion != 7 {
		t.Fatalf("first run max version = %d, want 7", firstVersion)
	}

	var firstRowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rag_schema_version`).Scan(&firstRowCount); err != nil {
		t.Fatalf("count version rows: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}
	var secondRowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM rag_schema_version`).Scan(&secondRowCount); err != nil {
		t.Fatalf("count version rows after re-run: %v", err)
	}
	if secondRowCount != firstRowCount {
		t.Errorf("schema_version rows after re-open = %d, want %d (no duplicate inserts)", secondRowCount, firstRowCount)
	}
}
