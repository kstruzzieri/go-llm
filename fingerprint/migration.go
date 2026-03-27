package fingerprint

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
		description: "baseline fingerprint profiles and failures tables",
		fn:          migrateV1,
	},
}

func migrateV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS fingerprint_profiles (
			backend_id                  TEXT NOT NULL,
			model_name                  TEXT NOT NULL,
			model_digest                TEXT NOT NULL,
			model_kind                  TEXT NOT NULL,
			capabilities                TEXT NOT NULL DEFAULT '[]',
			incomplete_capabilities     TEXT NOT NULL DEFAULT '[]',
			kind_source                 TEXT NOT NULL DEFAULT '',
			profile_version             INTEGER NOT NULL DEFAULT 1,
			tested_at                   INTEGER NOT NULL,
			effective_context           INTEGER NOT NULL DEFAULT 0,
			tool_calling_rate           REAL NOT NULL DEFAULT -1,
			instruction_score           REAL NOT NULL DEFAULT -1,
			generation_tokens_per_sec   REAL NOT NULL DEFAULT -1,
			prompt_latency_ns           INTEGER NOT NULL DEFAULT 0,
			cold_start_latency_ns       INTEGER NOT NULL DEFAULT 0,
			embedding_dim               INTEGER NOT NULL DEFAULT 0,
			embedding_coherence         REAL NOT NULL DEFAULT -1,
			embedding_latency_ns        INTEGER NOT NULL DEFAULT 0,
			peak_memory_mb              INTEGER NOT NULL DEFAULT 0,
			gpu_layers_used             INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (backend_id, model_name)
		)`,
		`CREATE TABLE IF NOT EXISTS fingerprint_failures (
			backend_id      TEXT NOT NULL,
			model_name      TEXT NOT NULL,
			model_digest    TEXT NOT NULL,
			last_error      TEXT NOT NULL,
			attempted_at    INTEGER NOT NULL,
			attempt_count   INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (backend_id, model_name)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("fingerprint: migrate v1: %w", err)
		}
	}
	return nil
}

func runMigrations(db *sql.DB) error {
	const createVersionTable = `CREATE TABLE IF NOT EXISTS fingerprint_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("fingerprint: create version table: %w", err)
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
			return fmt.Errorf("fingerprint: begin migration v%d: %w", m.version, err)
		}

		if err := m.fn(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("fingerprint: migration v%d (%s): %w", m.version, m.description, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO fingerprint_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().UnixMilli(),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("fingerprint: record version %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("fingerprint: commit migration v%d: %w", m.version, err)
		}
	}

	return nil
}

func currentSchemaVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM fingerprint_schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("fingerprint: query schema version: %w", err)
	}
	return version, nil
}
