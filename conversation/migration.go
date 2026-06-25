package conversation

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
		description: "baseline conversations table",
		fn:          migrateV1,
	},
	{
		version:     2,
		description: "conversation search shadow table and FTS5 index",
		fn:          migrateV2,
	},
}

func migrateV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS conversations (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL DEFAULT '',
			messages   TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_conversations_updated_id
			ON conversations(updated_at DESC, id ASC)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("conversation: migrate v1: %w", err)
		}
	}
	return nil
}

func migrateV2(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS conversation_search (
			id            TEXT PRIMARY KEY,
			title         TEXT NOT NULL DEFAULT '',
			body          TEXT NOT NULL,
			message_count INTEGER NOT NULL,
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS conversation_fts USING fts5(
			id UNINDEXED,
			title,
			body
		)`,
		`INSERT OR REPLACE INTO conversation_search (id, title, body, message_count, created_at, updated_at)
		 SELECT id, title, messages, json_array_length(messages), created_at, updated_at
		   FROM conversations`,
		`DELETE FROM conversation_fts`,
		`INSERT INTO conversation_fts (id, title, body)
		 SELECT id, title, body FROM conversation_search`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("conversation: migrate v2: %w", err)
		}
	}
	return nil
}

func runMigrations(db *sql.DB) error {
	const createVersionTable = `CREATE TABLE IF NOT EXISTS conversation_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("conversation: create version table: %w", err)
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
			return fmt.Errorf("conversation: begin migration v%d: %w", m.version, err)
		}

		if err := m.fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("conversation: migration v%d (%s): %w", m.version, m.description, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO conversation_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().UnixMilli(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("conversation: record version %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("conversation: commit migration v%d: %w", m.version, err)
		}
	}

	return nil
}

func currentSchemaVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("conversation: query schema version: %w", err)
	}
	return version, nil
}
