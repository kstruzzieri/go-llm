// transcript/migration.go
package transcript

import (
	"context"
	"database/sql"
	"errors"
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

// auditUpgradeHooks are per-call test seams for addMissingAuditColumnsWith.
// Production passes the zero value.
type auditUpgradeHooks struct {
	afterProbe  func()                 // the lock-free probe found a missing column
	afterLock   func()                 // the write lock is held and columns re-probed
	beforeAlter func(col string) error // before each ALTER
}

func addMissingAuditColumns(ctx context.Context, db *sql.DB) error {
	return addMissingAuditColumnsWith(ctx, db, auditUpgradeHooks{})
}

// addMissingAuditColumnsWith adds the audit columns a legacy conversations
// table lacks. A lock-free probe returns early when every column exists, so a
// current-schema open only reads. Otherwise the upgrade takes SQLite's write
// lock with its first statement, re-probes under the lock, and adds every
// still-missing column in one transaction: a concurrent opener waits on
// busy_timeout, then finds the columns the first one committed. Reading before
// writing would instead fail at once with SQLITE_BUSY or SQLITE_BUSY_SNAPSHOT
// rather than wait. Cancellation is observed between statements, not during
// the lock wait; the DSN's busy_timeout (5s) bounds that wait.
func addMissingAuditColumnsWith(ctx context.Context, db *sql.DB, hooks auditUpgradeHooks) (err error) {
	existing, err := conversationColumns(ctx, db)
	if err != nil {
		return err
	}
	if !missingAuditColumn(existing) {
		return nil
	}
	if hooks.afterProbe != nil {
		hooks.afterProbe()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transcript: begin audit-column upgrade: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("transcript: rollback audit-column upgrade: %w", rbErr))
		}
	}()
	// A zero-row UPDATE takes the write lock without changing any row.
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET id = id WHERE 0"); err != nil {
		return fmt.Errorf("transcript: claim audit-column upgrade: %w", err)
	}
	if existing, err = conversationColumns(ctx, tx); err != nil {
		return fmt.Errorf("transcript: re-probe audit columns under lock: %w", err)
	}
	if hooks.afterLock != nil {
		hooks.afterLock()
	}
	for _, col := range auditColumns {
		if existing[col.name] {
			continue
		}
		if hooks.beforeAlter != nil {
			if err := hooks.beforeAlter(col.name); err != nil {
				return err
			}
		}
		stmt := fmt.Sprintf("ALTER TABLE conversations ADD COLUMN %s %s", col.name, col.ddl)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("transcript: add column %s: %w", col.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("transcript: commit audit-column upgrade: %w", err)
	}
	return nil
}

func missingAuditColumn(existing map[string]bool) bool {
	for _, col := range auditColumns {
		if !existing[col.name] {
			return true
		}
	}
	return false
}

// queryer is the QueryContext method shared by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func conversationColumns(ctx context.Context, q queryer) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info(conversations)")
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
