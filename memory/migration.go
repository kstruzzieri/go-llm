package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type migration struct {
	version     int
	description string
	fn          func(tx *sql.Tx) error
}

var migrations = []migration{
	{version: 1, description: "baseline memories table + FTS5", fn: migrateV1},
	{version: 2, description: "agent memory records + FTS5", fn: migrateV2},
	{version: 3, description: "signed agent memory records", fn: migrateV3},
}

func migrateV3(tx *sql.Tx) error {
	for _, statement := range []string{
		`ALTER TABLE memory_records ADD COLUMN origin_tool TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_records ADD COLUMN origin_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_records ADD COLUMN trust_class TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_records ADD COLUMN signature_alg TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_records ADD COLUMN signature_key_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_records ADD COLUMN signature BLOB NOT NULL DEFAULT X''`,
		`CREATE TABLE memory_record_signing (id INTEGER PRIMARY KEY CHECK (id = 1), initialized_at INTEGER NOT NULL)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("memory: migrate v3: %w", err)
		}
	}
	return nil
}

func migrateV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memories (
			id                TEXT PRIMARY KEY,
			text              TEXT NOT NULL,
			scope             TEXT NOT NULL,
			workspace_id      TEXT NOT NULL DEFAULT '',
			source_session_id TEXT NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL,
			updated_at        INTEGER NOT NULL,
			deleted_at        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_live
			ON memories(scope, workspace_id) WHERE deleted_at = 0`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts
			USING fts5(id UNINDEXED, text, tokenize='porter')`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("memory: migrate v1: %w", err)
		}
	}
	return nil
}

func migrateV2(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memory_records (
			id           TEXT PRIMARY KEY,
			kind         TEXT NOT NULL,
			content      TEXT NOT NULL,
			namespace    TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '',
			session_id   TEXT NOT NULL DEFAULT '',
			source_kind  TEXT NOT NULL DEFAULT '',
			source_id    TEXT NOT NULL DEFAULT '',
			source_start INTEGER NOT NULL DEFAULT 0,
			source_end   INTEGER NOT NULL DEFAULT 0,
			source_hash  TEXT NOT NULL DEFAULT '',
			metadata     TEXT NOT NULL DEFAULT '{}',
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			expires_at   INTEGER NOT NULL DEFAULT 0,
			deleted_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_records_live
			ON memory_records(kind, namespace, workspace_id, session_id) WHERE deleted_at = 0`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_records_fts
			USING fts5(id UNINDEXED, content, tokenize='porter')`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("memory: migrate v2: %w", err)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsWith(ctx, db, migrations)
}

func runMigrationsWith(ctx context.Context, db *sql.DB, list []migration) error {
	if ctx == nil {
		return errors.New("memory: nil migration context")
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
	const createVersionTable = `CREATE TABLE IF NOT EXISTS memory_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("memory: create version table: %w", err)
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
		return fmt.Errorf("memory: begin migration v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("memory: rollback migration v%d: %w", m.version, rbErr))
		}
	}()

	// Claim before any reads or DDL so SQLite waits for the writer without a
	// read-lock upgrade. The claim and migration commit together; a duplicate
	// version means another opener already committed this step.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO memory_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("memory: claim migration v%d: %w", m.version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: claim migration v%d rows: %w", m.version, err)
	}
	if affected == 0 {
		return nil
	}
	if err := m.fn(tx); err != nil {
		return fmt.Errorf("memory: migration v%d (%s): %w", m.version, m.description, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit migration v%d: %w", m.version, err)
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'memory_schema_version')`,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("memory: check version table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM memory_schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("memory: query schema version: %w", err)
	}
	return version, nil
}

// sanitizeFTS5Query tokenizes into letter/digit/underscore runs and ANDs them as
// quoted FTS5 terms. Copied (small, pure) from conversation.sanitizeFTS5Query to
// avoid exporting that package's internal; behavior must stay identical.
func sanitizeFTS5Query(query string) string {
	var tokens []string
	var current strings.Builder
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			current.WriteRune(r)
		} else if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}
