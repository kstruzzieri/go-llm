package feedback

import (
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	version     int
	description string
	fn          func(tx *sql.Tx) error
}

var migrations = []migration{
	{
		version:     1,
		description: "baseline feedback tables",
		fn:          migrateV1,
	},
}

func migrateV1(tx *sql.Tx) error {
	stmts := []string{
		// Layer 1 -- Retrieval Log
		`CREATE TABLE IF NOT EXISTS feedback_retrievals (
			retrieval_id TEXT PRIMARY KEY,
			query        TEXT NOT NULL,
			chunk_keys   TEXT NOT NULL,
			created_at   INTEGER NOT NULL
		)`,

		// Layer 2 -- Signal Events
		`CREATE TABLE IF NOT EXISTS feedback_signals (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			retrieval_id TEXT NOT NULL,
			chunk_key    TEXT NOT NULL,
			signal_kind  TEXT NOT NULL,
			strength     REAL NOT NULL,
			created_at   INTEGER NOT NULL,
			FOREIGN KEY (retrieval_id) REFERENCES feedback_retrievals(retrieval_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_signals_chunk
			ON feedback_signals(chunk_key, created_at)`,

		// Layer 3 -- Materialized Aggregates
		`CREATE TABLE IF NOT EXISTS feedback_aggregates (
			chunk_key       TEXT PRIMARY KEY,
			retrieval_count INTEGER NOT NULL DEFAULT 0,
			weighted_score  REAL NOT NULL DEFAULT 0.0,
			last_signal_at  INTEGER NOT NULL DEFAULT 0,
			recomputed_at   INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("feedback: migrate v1: %w", err)
		}
	}
	return nil
}

func runMigrations(db *sql.DB) error {
	const createVersionTable = `CREATE TABLE IF NOT EXISTS feedback_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("feedback: create version table: %w", err)
	}

	currentVersion, err := currentSchemaVersion(db)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= currentVersion {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("feedback: begin migration v%d: %w", m.version, err)
		}

		if err := m.fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("feedback: migration v%d (%s): %w", m.version, m.description, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO feedback_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().UnixMilli(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("feedback: record version %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("feedback: commit migration v%d: %w", m.version, err)
		}
	}

	return nil
}

func currentSchemaVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM feedback_schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("feedback: query schema version: %w", err)
	}
	return version, nil
}
