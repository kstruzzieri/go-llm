package rag

import (
	"database/sql"
	"fmt"
	"time"
)

// migration represents a single schema migration step.
type migration struct {
	version     int
	description string
	fn          func(tx *sql.Tx) error
}

// migrations defines the ordered list of schema migrations.
// Version 1 is the baseline (existing schema).
// Version 2 adds indexed_at, FTS5, and triggers.
var migrations = []migration{
	{
		version:     1,
		description: "baseline schema",
		fn:          migrateV1,
	},
	{
		version:     2,
		description: "add indexed_at, FTS5 index, and sync triggers",
		fn:          migrateV2,
	},
	{
		version:     3,
		description: "add stable_key column for chunk identity",
		fn:          migrateV3,
	},
}

// migrateV1 creates the baseline chunks table and indexes.
// This corresponds to the original initSchema logic.
func migrateV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS chunks (
			id TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			source TEXT NOT NULL,
			start_line INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			language TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '{}',
			embedding TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_source_line ON chunks(source, start_line)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_lang_source_line ON chunks(language, source, start_line)`,
		`DROP INDEX IF EXISTS idx_chunks_source`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rag: migrate v1: %w", err)
		}
	}
	return nil
}

// migrateV2 adds the indexed_at column, FTS5 virtual table, and sync triggers.
func migrateV2(tx *sql.Tx) error {
	stmts := []string{
		// Add indexed_at column.
		`ALTER TABLE chunks ADD COLUMN indexed_at INTEGER NOT NULL DEFAULT 0`,

		// Backfill existing rows with current timestamp.
		`UPDATE chunks SET indexed_at = strftime('%s', 'now') WHERE indexed_at = 0`,

		// Create FTS5 virtual table backed by the chunks table.
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			content, source,
			content=chunks, content_rowid=rowid
		)`,

		// After INSERT trigger: sync new rows into FTS5.
		`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
			INSERT INTO chunks_fts(rowid, content, source)
			VALUES (new.rowid, new.content, new.source);
		END`,

		// After DELETE trigger: remove deleted rows from FTS5.
		`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
			VALUES ('delete', old.rowid, old.content, old.source);
		END`,

		// After UPDATE trigger: delete old entry and insert new one in FTS5.
		`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, content, source)
			VALUES ('delete', old.rowid, old.content, old.source);
			INSERT INTO chunks_fts(rowid, content, source)
			VALUES (new.rowid, new.content, new.source);
		END`,

		// Rebuild FTS5 index to include any pre-existing rows.
		`INSERT INTO chunks_fts(chunks_fts) VALUES ('rebuild')`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rag: migrate v2: %w", err)
		}
	}
	return nil
}

// migrateV3 adds the stable_key column for logical chunk identity.
// Existing rows get an empty default that will be populated on re-index.
func migrateV3(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE chunks ADD COLUMN stable_key TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_stable_key ON chunks(stable_key)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rag: migrate v3: %w", err)
		}
	}
	return nil
}

// runMigrations applies all pending schema migrations to the database.
// It handles three scenarios:
//  1. Fresh database: creates rag_schema_version table and runs all migrations.
//  2. Existing database with rag_schema_version: runs only migrations newer than
//     the current version.
//  3. Pre-migration database (has chunks table but no rag_schema_version): marks
//     version 1 as already applied and runs version 2+.
func runMigrations(db *sql.DB) error {
	// Ensure the version tracking table exists.
	const createVersionTable = `CREATE TABLE IF NOT EXISTS rag_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("rag: create version table: %w", err)
	}

	// Detect pre-migration databases: chunks table exists but no version records.
	currentVersion, err := currentSchemaVersion(db)
	if err != nil {
		return err
	}

	if currentVersion == 0 && tableExists(db, "chunks") {
		// Database was created before the migration system. Determine
		// which version the schema actually represents and record all
		// applicable versions atomically so a crash cannot leave a
		// partial version state.
		detectedVersion := 1
		if tableExists(db, "chunks_fts") {
			detectedVersion = 2
		}
		if err := recordVersionsUpTo(db, detectedVersion); err != nil {
			return err
		}
		currentVersion = detectedVersion
	} else if currentVersion > 0 && currentVersion < 2 && tableExists(db, "chunks_fts") {
		// Schema has v2 artifacts but only version 1 is recorded.
		// This can happen if a prior repair wrote v1 but crashed
		// before writing v2. Record v2 to prevent re-running it.
		if err := recordVersionsUpTo(db, 2); err != nil {
			return err
		}
		currentVersion = 2
	}

	// Apply pending migrations in order.
	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("rag: begin migration v%d: %w", m.version, err)
		}

		if err := m.fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rag: migration v%d (%s): %w", m.version, m.description, err)
		}

		// Record version inside the transaction so it commits atomically
		// with the DDL. Without this, a crash between commit and version
		// recording would leave an applied-but-unrecorded migration,
		// and non-idempotent DDL (e.g., ALTER TABLE ADD COLUMN) would
		// fail on the next startup.
		if _, err := tx.Exec(
			`INSERT INTO rag_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rag: record version %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("rag: commit migration v%d: %w", m.version, err)
		}
	}

	return nil
}

// currentSchemaVersion returns the highest applied migration version, or 0 if none.
func currentSchemaVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM rag_schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("rag: query schema version: %w", err)
	}
	return version, nil
}

// tableExists checks whether a table with the given name exists in the database.
func tableExists(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count)
	return err == nil && count > 0
}

// recordVersion inserts a version record into rag_schema_version.
func recordVersion(db *sql.DB, version int, description string) error {
	_, err := db.Exec(
		`INSERT INTO rag_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
		version, description, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("rag: record version %d: %w", version, err)
	}
	return nil
}

// recordVersionsUpTo atomically inserts version records for all migrations
// up to and including the given version. Used during pre-migration database
// detection to ensure a crash cannot leave a partial version state.
func recordVersionsUpTo(db *sql.DB, upTo int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("rag: begin version recording: %w", err)
	}
	now := time.Now().Unix()
	for _, m := range migrations {
		if m.version > upTo {
			break
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO rag_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description+" (pre-existing)", now,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rag: record version %d: %w", m.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: commit version recording: %w", err)
	}
	return nil
}
