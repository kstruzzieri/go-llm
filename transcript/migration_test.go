// transcript/migration_test.go
package transcript

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // :memory: opens a private DB per connection otherwise
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out[name] = true
	}
	return out
}

func TestMigrate_FreshCreatesBothTables(t *testing.T) {
	db := openMem(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	raw := columns(t, db, "raw_chat_calls")
	for _, c := range []string{
		"call_id", "conversation_key", "conversation_id", "identity_source",
		"request_messages", "response_message", "model", "provider", "route_outcome_json",
		"created_at", "projection_status", "projection_error",
	} {
		if !raw[c] {
			t.Errorf("raw_chat_calls missing column %q", c)
		}
	}
	conv := columns(t, db, "conversations")
	for _, c := range []string{
		"id", "title", "messages", "created_at", "updated_at",
		"conversation_key", "identity_source", "latest_call_id", "message_count", "stitch_status",
	} {
		if !conv[c] {
			t.Errorf("conversations missing column %q", c)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openMem(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrate_LegacyFiveColumnTableGetsAuditColumns(t *testing.T) {
	db := openMem(t)
	// Simulate a conversation/ store DB: exact five-column baseline + one row.
	if _, err := db.Exec(`CREATE TABLE conversations (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		messages   TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, messages, created_at, updated_at) VALUES (?,?,?,?,?)`,
		"legacy-1", "old", `[{"role":"user","content":"hi"}]`, int64(1), int64(2),
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	conv := columns(t, db, "conversations")
	for _, c := range []string{"conversation_key", "identity_source", "latest_call_id", "message_count", "stitch_status"} {
		if !conv[c] {
			t.Errorf("legacy conversations missing audit column %q after migrate", c)
		}
	}

	// Pre-existing row survives and stays capture-readable (five base columns).
	var (
		id, title, messages string
		created, updated    int64
	)
	if err := db.QueryRow(
		`SELECT id, title, messages, created_at, updated_at FROM conversations WHERE id = ?`, "legacy-1",
	).Scan(&id, &title, &messages, &created, &updated); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if id != "legacy-1" || title != "old" || messages != `[{"role":"user","content":"hi"}]` {
		t.Errorf("legacy row mutated: id=%q title=%q messages=%q", id, title, messages)
	}

	// Backfilled audit defaults are usable.
	var convKey string
	var msgCount int
	if err := db.QueryRow(
		`SELECT conversation_key, message_count FROM conversations WHERE id = ?`, "legacy-1",
	).Scan(&convKey, &msgCount); err != nil {
		t.Fatalf("read audit cols: %v", err)
	}
	if convKey != "" || msgCount != 0 {
		t.Errorf("legacy audit defaults = (%q,%d), want (\"\",0)", convKey, msgCount)
	}
}
