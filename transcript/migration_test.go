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
