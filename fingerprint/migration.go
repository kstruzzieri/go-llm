package fingerprint

import (
	"context"
	"database/sql"
	"errors"
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
	{
		version:     2,
		description: "capability_probes table for tri-state capability verdicts",
		fn:          migrateV2,
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

func migrateV2(tx *sql.Tx) error {
	const stmt = `CREATE TABLE IF NOT EXISTS capability_probes (
		backend_id    TEXT NOT NULL,
		model_name    TEXT NOT NULL,
		capability    TEXT NOT NULL,
		state         TEXT NOT NULL,
		model_digest  TEXT NOT NULL,
		probe_version INTEGER NOT NULL,
		tested_at     INTEGER NOT NULL,
		expires_at    INTEGER,
		PRIMARY KEY (backend_id, model_name, capability)
	)`
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("fingerprint: migrate v2: %w", err)
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsWith(ctx, db, migrations)
}

func runMigrationsWith(ctx context.Context, db *sql.DB, list []migration) error {
	if ctx == nil {
		return errors.New("fingerprint: nil migration context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	currentVersion, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if currentVersion >= list[len(list)-1].version {
		return nil
	}
	const createVersionTable = `CREATE TABLE IF NOT EXISTS fingerprint_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("fingerprint: create version table: %w", err)
	}
	for _, m := range list {
		if m.version <= currentVersion {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("fingerprint: begin migration v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("fingerprint: rollback migration v%d: %w", m.version, rbErr))
		}
	}()

	// Claim before any reads or DDL so SQLite waits for the writer without a
	// read-lock upgrade. The claim and migration commit together; a duplicate
	// version means another opener already committed this step.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO fingerprint_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("fingerprint: claim migration v%d: %w", m.version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("fingerprint: claim migration v%d rows: %w", m.version, err)
	}
	if affected == 0 {
		return nil
	}
	if err := m.fn(tx); err != nil {
		return fmt.Errorf("fingerprint: migration v%d (%s): %w", m.version, m.description, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("fingerprint: commit migration v%d: %w", m.version, err)
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'fingerprint_schema_version')`,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("fingerprint: check version table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM fingerprint_schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("fingerprint: query schema version: %w", err)
	}
	return version, nil
}
