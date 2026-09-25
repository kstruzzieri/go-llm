package feedback

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

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsWith(ctx, db, migrations)
}

func runMigrationsWith(ctx context.Context, db *sql.DB, list []migration) error {
	if ctx == nil {
		return errors.New("feedback: nil migration context")
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
	const createVersionTable = `CREATE TABLE IF NOT EXISTS feedback_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("feedback: create version table: %w", err)
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
		return fmt.Errorf("feedback: begin migration v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("feedback: rollback migration v%d: %w", m.version, rbErr))
		}
	}()

	// Claim before any reads or DDL so SQLite waits for the writer without a
	// read-lock upgrade. The claim and migration commit together; a duplicate
	// version means another opener already committed this step.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO feedback_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("feedback: claim migration v%d: %w", m.version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("feedback: claim migration v%d rows: %w", m.version, err)
	}
	if affected == 0 {
		return nil
	}
	if err := m.fn(tx); err != nil {
		return fmt.Errorf("feedback: migration v%d (%s): %w", m.version, m.description, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback: commit migration v%d: %w", m.version, err)
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'feedback_schema_version')`,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("feedback: check version table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM feedback_schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("feedback: query schema version: %w", err)
	}
	return version, nil
}
