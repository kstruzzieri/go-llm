package conversation

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
		description: "baseline conversations table",
		fn:          migrateV1,
	},
	{
		version:     2,
		description: "conversation search shadow table and FTS5 index",
		fn:          migrateV2,
	},
	{
		version:     3,
		description: "durable conversation summaries",
		fn:          migrateV3,
	},
	{
		version:     4,
		description: "conversation revisions",
		fn:          migrateV4,
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

func migrateV3(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE conversations ADD COLUMN summary_content TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN summary_message_count INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("conversation: migrate v3: %w", err)
		}
	}
	return nil
}

func migrateV4(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE conversations ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("conversation: migrate v4: %w", err)
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsWith(ctx, db, migrations)
}

func runMigrationsWith(ctx context.Context, db *sql.DB, list []migration) error {
	if ctx == nil {
		return errors.New("conversation: nil migration context")
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
	const createVersionTable = `CREATE TABLE IF NOT EXISTS conversation_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("conversation: create version table: %w", err)
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
		return fmt.Errorf("conversation: begin migration v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("conversation: rollback migration v%d: %w", m.version, rbErr))
		}
	}()

	// Claim before any reads or DDL so SQLite waits for the writer without a
	// read-lock upgrade. The claim and migration commit together; a duplicate
	// version means another opener already committed this step.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("conversation: claim migration v%d: %w", m.version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("conversation: claim migration v%d rows: %w", m.version, err)
	}
	if affected == 0 {
		return nil
	}
	if err := m.fn(tx); err != nil {
		return fmt.Errorf("conversation: migration v%d (%s): %w", m.version, m.description, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("conversation: commit migration v%d: %w", m.version, err)
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'conversation_schema_version')`,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("conversation: check version table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM conversation_schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("conversation: query schema version: %w", err)
	}
	return version, nil
}
