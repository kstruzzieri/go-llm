package conversation

import (
	"context"
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
	if version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
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
	if version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
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

func TestRunMigrations_SearchTablesExist(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	for _, name := range []string{"conversation_search", "conversation_fts"} {
		var count int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`,
			name,
		).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected %s table, count=%d err=%v", name, count, err)
		}
	}
}

func TestRunMigrations_DurableSummaryColumnsExist(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	for _, name := range []string{"summary_content", "summary_message_count"} {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name = ?`, name).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected conversations.%s column, count=%d err=%v", name, count, err)
		}
	}
}

// TestRunMigrations_V2ToV3BackwardCompat verifies that a database created at
// schema v2 (no summary columns) upgrades to v3 in place and that pre-v3 rows
// load with a nil DurableSummary. This is the spec's backward-compatibility
// requirement: the v3 migration is additive and never rewrites existing data.
func TestRunMigrations_V2ToV3BackwardCompat(t *testing.T) {
	db := openTestDB(t)

	// Build a v2-state database: version table + v1/v2 schema, stamped at v2,
	// with one pre-v3 conversation row (conversations has no summary columns yet).
	if _, err := db.Exec(`CREATE TABLE conversation_schema_version (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create version table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV1(tx); err != nil {
		t.Fatalf("migrateV1: %v", err)
	}
	if err := migrateV2(tx); err != nil {
		t.Fatalf("migrateV2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_schema_version (version, description, applied_at)
		VALUES (1, 'v1', 0), (2, 'v2', 0)`); err != nil {
		t.Fatalf("stamp versions: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, messages, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"old1", "old title", `[{"role":"user","content":"hi"}]`, 1, 2,
	); err != nil {
		t.Fatalf("insert pre-v3 row: %v", err)
	}

	// Only v3 should apply now (v1/v2 already stamped).
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version); err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 3 {
		t.Fatalf("schema version = %d, want 3", version)
	}

	// The pre-v3 row loads with a nil DurableSummary and preserved content.
	// NewStore re-runs migrations idempotently (already at v3).
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	conv, err := store.Load(context.Background(), "old1")
	if err != nil {
		t.Fatalf("Load pre-v3 row: %v", err)
	}
	if conv.DurableSummary != nil {
		t.Fatalf("pre-v3 row must load with nil DurableSummary, got %+v", conv.DurableSummary)
	}
	if conv.Title != "old title" || len(conv.Messages) != 1 {
		t.Fatalf("pre-v3 row content not preserved: title=%q msgs=%d", conv.Title, len(conv.Messages))
	}
}
