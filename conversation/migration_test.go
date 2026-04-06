package conversation

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open :memory: db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunMigrations_FreshDB(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count)
	if err != nil {
		t.Fatalf("conversations table missing: %v", err)
	}

	var version int
	err = db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}

	var version int
	err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}
}

func TestRunMigrations_IndexExists(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_conversations_updated_id'`,
	).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected idx_conversations_updated_id index, count=%d err=%v", count, err)
	}
}
