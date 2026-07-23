// transcript/migration.go
package transcript

import (
	"context"
	"database/sql"
	"fmt"
)

const createRawTable = `CREATE TABLE IF NOT EXISTS raw_chat_calls (
	call_id            TEXT PRIMARY KEY,
	conversation_key   TEXT NOT NULL,
	conversation_id    TEXT NOT NULL,
	identity_source    TEXT NOT NULL,
	request_messages   TEXT NOT NULL,
	response_message   TEXT NOT NULL,
	model              TEXT NOT NULL DEFAULT '',
	provider           TEXT NOT NULL DEFAULT '',
	route_outcome_json TEXT,
	created_at         INTEGER NOT NULL,
	projection_status  TEXT NOT NULL,
	projection_error   TEXT
)`

const createRawIndex = `CREATE INDEX IF NOT EXISTS idx_raw_calls_conv
	ON raw_chat_calls(conversation_id, created_at)`

// createConversationsTable is a strict superset of the five columns capture.go
// reads. Audit columns carry NOT NULL DEFAULT so the ALTER path (legacy DBs)
// can add them; fresh rows always supply explicit values.
const createConversationsTable = `CREATE TABLE IF NOT EXISTS conversations (
	id               TEXT PRIMARY KEY,
	title            TEXT NOT NULL DEFAULT '',
	messages         TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	conversation_key TEXT NOT NULL DEFAULT '',
	identity_source  TEXT NOT NULL DEFAULT '',
	latest_call_id   TEXT NOT NULL DEFAULT '',
	message_count    INTEGER NOT NULL DEFAULT 0,
	stitch_status    TEXT NOT NULL DEFAULT '',
	rendered_messages TEXT NOT NULL DEFAULT ''
)`

const createConvKeyIndex = `CREATE INDEX IF NOT EXISTS idx_conversations_key
	ON conversations(conversation_key)`

const createConvUpdatedIndex = `CREATE INDEX IF NOT EXISTS idx_conversations_updated
	ON conversations(updated_at DESC, id ASC)`

// auditColumns are the conversations columns beyond the five-column
// conversation/ baseline, added via ALTER TABLE ADD COLUMN on legacy DBs.
var auditColumns = []struct{ name, ddl string }{
	{"conversation_key", "TEXT NOT NULL DEFAULT ''"},
	{"identity_source", "TEXT NOT NULL DEFAULT ''"},
	{"latest_call_id", "TEXT NOT NULL DEFAULT ''"},
	{"message_count", "INTEGER NOT NULL DEFAULT 0"},
	{"stitch_status", "TEXT NOT NULL DEFAULT ''"},
	{"rendered_messages", "TEXT NOT NULL DEFAULT ''"},
}

// migrate ensures both tables, their indexes, and the conversations audit
// columns exist. Idempotent and safe against a pre-existing five-column
// conversations table written by the conversation/ store.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{createRawTable, createRawIndex, createConversationsTable} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("transcript: migrate: %w", err)
		}
	}
	if err := addMissingAuditColumns(ctx, db); err != nil {
		return err
	}
	for _, stmt := range []string{createConvKeyIndex, createConvUpdatedIndex} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("transcript: migrate index: %w", err)
		}
	}
	return nil
}

func addMissingAuditColumns(ctx context.Context, db *sql.DB) error {
	existing, err := conversationColumns(ctx, db)
	if err != nil {
		return err
	}
	for _, col := range auditColumns {
		if existing[col.name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE conversations ADD COLUMN %s %s", col.name, col.ddl)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("transcript: add column %s: %w", col.name, err)
		}
	}
	return nil
}

func conversationColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(conversations)")
	if err != nil {
		return nil, fmt.Errorf("transcript: table_info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dfltValue        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return nil, fmt.Errorf("transcript: scan table_info: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("transcript: iterate table_info: %w", err)
	}
	return cols, nil
}
