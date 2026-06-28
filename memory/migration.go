package memory

import (
	"database/sql"
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

func runMigrations(db *sql.DB) error {
	const createVersion = `CREATE TABLE IF NOT EXISTS memory_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersion); err != nil {
		return fmt.Errorf("memory: create version table: %w", err)
	}
	var cur int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM memory_schema_version`).Scan(&cur); err != nil {
		return fmt.Errorf("memory: query schema version: %w", err)
	}
	for _, m := range migrations {
		if m.version <= cur {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("memory: begin migration v%d: %w", m.version, err)
		}
		if err := m.fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("memory: migration v%d (%s): %w", m.version, m.description, err)
		}
		if _, err := tx.Exec(`INSERT INTO memory_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("memory: record version %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("memory: commit migration v%d: %w", m.version, err)
		}
	}
	return nil
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
